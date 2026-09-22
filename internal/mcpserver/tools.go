package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Token budgets per tool. These are the numbers the tool surface is designed
// around: an overview a model can afford on every question, and detail views
// it can afford several of.
const (
	budgetOverview    = 500
	budgetListNodes   = 2000
	budgetDescribe    = 1500
	budgetTracePath   = 2000
	budgetDiagnostics = 1000
	budgetImpact      = 2000
)

// mostConnected bounds the overview's busiest-components list. The overview
// is meant to orient, not to enumerate; list_nodes is where the full set
// lives.
const mostConnected = 8

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// errorResult reports a problem to the model rather than to the protocol.
//
// A tool error is something the model can act on — a name it should spell
// differently, a scan it should run — so it comes back as content it can
// read, not as a JSON-RPC error it cannot.
func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// OverviewArgs takes no parameters.
type OverviewArgs struct{}

// overview is the orientation call: what this system is made of, in the
// smallest number of tokens that still supports a follow-up question.
func (s *Server) overview(ctx context.Context, _ *mcp.CallToolRequest, _ OverviewArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	v := newView(g)
	r := NewResponse(budgetOverview)

	name := g.Root.Name
	if g.Root.VCS != nil && g.Root.VCS.Commit != "" {
		short := g.Root.VCS.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		name += fmt.Sprintf(" @ %s", short)
		if g.Root.VCS.Branch != "" {
			name += " (" + g.Root.VCS.Branch + ")"
		}
	}
	r.Headerf("%s", name)
	// Both numbers, not just the one that flatters the scan. "from 2 files"
	// on a repository of five reads as a complete reading of a small tree;
	// "from 2 of 5 files" is the same fact and prompts the right question.
	files := fmt.Sprintf("%d files", g.Stats.FilesParsed)
	if g.Stats.FilesScanned > g.Stats.FilesParsed {
		files = fmt.Sprintf("%d of %d files", g.Stats.FilesParsed, g.Stats.FilesScanned)
	}
	r.Headerf("%d components, %d relationships, from %s",
		len(g.Nodes), countRelationships(g), files)
	if note := shapeNote(g); note != "" {
		r.Headerf("")
		r.Headerf("%s", note)
	}

	if len(g.Nodes) == 0 {
		r.Headerf("")
		r.Headerf("No components were found. structura_diagnostics explains why.")
		return textResult(r.String()), nil, nil
	}

	// A repository holding several independent projects is not one system,
	// and a reader given a single component count will treat it as one --
	// tracing paths between stacks that have never heard of each other. The
	// count is the first thing to say, before any of the totals above are
	// interpreted.
	if projects := projectCounts(g); len(projects) > 1 {
		r.Headerf("")
		r.Headerf("This repository holds %d separate projects. Components in different", len(projects))
		r.Headerf("projects are unrelated, and no relationship is drawn between them:")
		for _, p := range projects {
			r.Headerf("  %-40s %d components", p.name, p.count)
		}
	}

	counts := map[schema.NodeKind]int{}
	for _, n := range g.Nodes {
		counts[n.Kind]++
	}
	r.Headerf("")
	r.Headerf("Components by kind:")
	for _, kind := range schema.NodeKinds() {
		if counts[kind] > 0 {
			r.Headerf("  %-15s %d", kind, counts[kind])
		}
	}

	// The most connected components are where a reader should look first,
	// and are usually the answer to "what is the entry point".
	type ranked struct {
		id      string
		out, in int
		total   int
	}
	var byDegree []ranked
	for _, n := range g.Nodes {
		if n.Kind == schema.KindBoundary {
			continue
		}
		out, in := v.degree(n.ID)
		if out+in > 0 {
			byDegree = append(byDegree, ranked{n.ID, out, in, out + in})
		}
	}
	sort.SliceStable(byDegree, func(i, j int) bool {
		if byDegree[i].total != byDegree[j].total {
			return byDegree[i].total > byDegree[j].total
		}
		return byDegree[i].id < byDegree[j].id
	})

	if len(byDegree) > 0 {
		r.Headerf("")
		r.Headerf("Most connected:")
		r.Expect(min(len(byDegree), mostConnected))
		for i, item := range byDegree {
			if i >= mostConnected {
				break
			}
			if !r.Itemf("  %-28s %d out, %d in", v.label(item.id), item.out, item.in) {
				break
			}
		}
	}

	if len(g.Diagnostics) > 0 {
		bySeverity := map[schema.Severity]int{}
		for _, d := range g.Diagnostics {
			bySeverity[d.Severity]++
		}
		var parts []string
		for _, sev := range schema.Severities() {
			if bySeverity[sev] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", bySeverity[sev], sev))
			}
		}
		r.Headerf("")
		r.Headerf("Gaps: %s. Call structura_diagnostics for what is missing and why.",
			strings.Join(parts, ", "))
	}
	return textResult(r.String()), nil, nil
}

// ListNodesArgs filters the component list.
type ListNodesArgs struct {
	Kind      string `json:"kind,omitempty" jsonschema:"filter by kind: service, datastore, queue, external, cloud_resource, package, boundary"`
	Layer     string `json:"layer,omitempty" jsonschema:"filter by C4 layer: context, container, component"`
	Namespace string `json:"namespace,omitempty" jsonschema:"filter by namespace, such as a Kubernetes namespace or Compose project"`
	Query     string `json:"query,omitempty" jsonschema:"substring match against name, id, and technology"`
	Cursor    int    `json:"cursor,omitempty" jsonschema:"offset to resume from, as returned in a truncation notice"`
}

func (s *Server) listNodes(ctx context.Context, _ *mcp.CallToolRequest, args ListNodesArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	if msg, ok := validateEnums(args.Kind, args.Layer); !ok {
		return errorResult("%s", msg), nil, nil
	}

	v := newView(g)
	filter := nodeFilter{kind: args.Kind, layer: args.Layer, namespace: args.Namespace, query: args.Query}

	var matched []schema.Node
	for _, n := range g.Nodes {
		if filter.matches(n) {
			matched = append(matched, n)
		}
	}
	if len(matched) == 0 {
		return textResult(fmt.Sprintf("No components match %s.\n", describeFilter(filter))), nil, nil
	}

	r := NewResponse(budgetListNodes)
	r.Headerf("%d components match %s.", len(matched), describeFilter(filter))
	r.Headerf("")

	start := args.Cursor
	if start < 0 || start >= len(matched) {
		start = 0
	}
	r.Countf(start)
	r.Expect(len(matched))

	for _, n := range matched[start:] {
		out, in := v.degree(n.ID)
		line := fmt.Sprintf("  %s  [%s]", n.ID, n.Kind)
		if tech := techString(n.Tech); tech != "" {
			line += "  " + tech
		}
		if out+in > 0 {
			line += fmt.Sprintf("  (%d out, %d in)", out, in)
		}
		if attrs := attrSummary(n.Attrs, 3); attrs != "" {
			line += "\n      " + attrs
		}
		if !r.Itemf("%s", line) {
			break
		}
	}
	return textResult(r.StringWithCursor(fmt.Sprintf("%d", start+r.Shown()), "components")), nil, nil
}

// DescribeNodeArgs identifies one component.
type DescribeNodeArgs struct {
	Node string `json:"node" jsonschema:"the component's id or name"`
}

// describeNode answers "what is this and what does it touch" in one call.
func (s *Server) describeNode(ctx context.Context, _ *mcp.CallToolRequest, args DescribeNodeArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	v := newView(g)

	id, candidates, err := v.resolveRef(args.Node)
	if err != nil {
		if len(candidates) > 0 {
			return errorResult("%v: %s. Call again with one of those ids.",
				err, strings.Join(candidates, ", ")), nil, nil
		}
		return errorResult("%v. Use structura_list_nodes to see what exists.", err), nil, nil
	}
	n := v.byID[id]

	r := NewResponse(budgetDescribe)
	r.Headerf("%s", n.ID)
	r.Headerf("  name       %s", n.Name)
	r.Headerf("  kind       %s (%s layer)", n.Kind, n.Layer)
	if n.Namespace != "" {
		r.Headerf("  namespace  %s", n.Namespace)
	}
	if tech := techString(n.Tech); tech != "" {
		r.Headerf("  tech       %s", tech)
	}
	if n.Confidence < 1 {
		r.Headerf("  confidence %.2f — inferred rather than declared", n.Confidence)
	}
	if attrs := attrSummary(n.Attrs, 12); attrs != "" {
		r.Headerf("  attrs      %s", attrs)
	}
	for _, src := range n.Sources {
		r.Headerf("  declared   %s", location(src.Path, src.Line))
	}

	writeEdges(r, v, "Depends on", v.outgoing[id], func(e schema.Edge) string { return e.To })
	writeEdges(r, v, "Depended on by", v.incoming[id], func(e schema.Edge) string { return e.From })

	// What the scan reported about this component's own files, next to the
	// relationships rather than buried in a global list a reader would have
	// to correlate by filename. An empty dependency list with an unresolved
	// reference in the same file means something quite different from an
	// empty list with nothing reported.
	related := v.diagnosticsFor(n)

	// Containment does not count. Every workload sits inside the namespace
	// that deploys it, so counting that edge would mean no component is ever
	// isolated and the explanation below would never be reached.
	out, in := v.degree(id)
	isolated := out == 0 && in == 0

	if isolated {
		r.Headerf("")
		if len(related) == 0 {
			r.Headerf("No relationships, and nothing was reported about the files this")
			r.Headerf("component is declared in. Either it genuinely depends on nothing,")
			r.Headerf("or it reaches its dependencies from application code, which a")
			r.Headerf("configuration scan cannot see.")
		} else {
			r.Headerf("No relationships were found. The scan did report problems with the")
			r.Headerf("files this component is declared in, listed below, which may be why.")
		}
	}

	if len(related) > 0 {
		r.Headerf("")
		if isolated {
			r.Headerf("Reported for these files:")
		} else {
			r.Headerf("Reported for this component's files, so the picture above may be incomplete:")
		}
		r.Expect(len(related))
		for _, d := range related {
			if !r.Itemf("  [%s] %s at %s\n      %s",
				d.Severity, d.Code, location(d.Path, d.Line), d.Message) {
				break
			}
		}
		// Attribution is by file, and a manifest often holds many objects.
		r.Headerf("")
		r.Headerf("(Matched by file, so an entry may concern another object in the same file.)")
	}
	return textResult(r.String()), nil, nil
}

// writeEdges renders one direction of a node's relationships, with the
// evidence for each.
//
// The evidence is the point. An architecture tool that says "api talks to
// postgres" and cannot say why is asking to be believed; one that names the
// file, the line, and the rule can be checked.
func writeEdges(r *Response, v *view, heading string, edges []schema.Edge, other func(schema.Edge) string) {
	var relevant []schema.Edge
	for _, e := range edges {
		if e.Kind != schema.EdgeContains {
			relevant = append(relevant, e)
		}
	}
	if len(relevant) == 0 {
		return
	}
	sort.SliceStable(relevant, func(i, j int) bool {
		if relevant[i].Confidence != relevant[j].Confidence {
			return relevant[i].Confidence > relevant[j].Confidence
		}
		return other(relevant[i]) < other(relevant[j])
	})

	r.Headerf("")
	r.Headerf("%s:", heading)
	r.Expect(len(relevant))
	for _, e := range relevant {
		line := fmt.Sprintf("  %s %s (%.2f)", e.Kind, v.label(other(e)), e.Confidence)
		if e.Protocol != "" {
			line += " over " + e.Protocol
		}
		for _, ev := range e.Evidence {
			line += fmt.Sprintf("\n      %s", ev.Rule)
			if ev.Path != "" {
				line += " at " + location(ev.Path, ev.Line)
			}
			if ev.Detail != "" {
				line += "\n        " + ev.Detail
			}
		}
		if !r.Itemf("%s", line) {
			break
		}
	}
}

// TracePathArgs identifies the endpoints of a trace.
type TracePathArgs struct {
	From     string `json:"from" jsonschema:"the component the path starts at, by id or name"`
	To       string `json:"to" jsonschema:"the component the path ends at, by id or name"`
	MaxPaths int    `json:"max_paths,omitempty" jsonschema:"how many shortest paths to return (default 3)"`
}

// tracePath answers "how does A reach B", which is the question behind most
// blast-radius and dependency questions.
func (s *Server) tracePath(ctx context.Context, _ *mcp.CallToolRequest, args TracePathArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	v := newView(g)

	from, fromCandidates, err := v.resolveRef(args.From)
	if err != nil {
		return errorResult("from: %v%s", err, candidateHint(fromCandidates)), nil, nil
	}
	to, toCandidates, err := v.resolveRef(args.To)
	if err != nil {
		return errorResult("to: %v%s", err, candidateHint(toCandidates)), nil, nil
	}
	if from == to {
		return textResult(fmt.Sprintf("%s and %s are the same component.\n",
			args.From, args.To)), nil, nil
	}

	// The default is generous because this tool answers blast-radius
	// questions, where a route left out is the failure. Returning three of
	// five routes and heading the answer "3 paths" is how a reader concludes
	// a service is uninvolved when it is on the critical path.
	maxPaths := args.MaxPaths
	if maxPaths <= 0 {
		maxPaths = 10
	}
	if maxPaths > 25 {
		maxPaths = 25
	}

	r := NewResponse(budgetTracePath)
	paths, incomplete := v.paths(from, to, maxPaths)
	if len(paths) == 0 {
		r.Headerf("No path from %s to %s within %d hops.", v.label(from), v.label(to), maxPathDepth)
		r.Headerf("")
		// The absence of a path in the graph is not the absence of a
		// dependency in the system, and saying so is the difference between
		// an honest answer and a misleading one.
		r.Headerf("Nothing in this repository's configuration connects them. They may still")
		r.Headerf("interact through code, or through a manifest this scan could not read;")
		r.Headerf("structura_diagnostics lists what was skipped.")
		return textResult(r.String()), nil, nil
	}

	// The count is only stated as a fact when the search was complete.
	// "1 path" when a second exists is an affirmative false statement, and it
	// is exactly the answer someone repeats in an incident channel.
	if incomplete {
		r.Headerf("At least %d %s from %s to %s (more may exist; the search was cut short):",
			len(paths), plural(len(paths), "route", "routes"), v.label(from), v.label(to))
	} else {
		r.Headerf("%d %s from %s to %s. This is every route in the graph:", len(paths),
			plural(len(paths), "path", "paths"), v.label(from), v.label(to))
	}

	for i, path := range paths {
		var b strings.Builder
		fmt.Fprintf(&b, "\n  %d. %s", i+1, v.label(from))
		weakest := 1.0
		for _, e := range path {
			fmt.Fprintf(&b, "\n       -[%s %.2f]-> %s", e.Kind, e.Confidence, v.label(e.To))
			if len(e.Evidence) > 0 {
				ev := e.Evidence[0]
				fmt.Fprintf(&b, "\n          %s", ev.Rule)
				if ev.Path != "" {
					fmt.Fprintf(&b, " at %s", location(ev.Path, ev.Line))
				}
			}
			if e.Confidence < weakest {
				weakest = e.Confidence
			}
		}
		// A chain is only as trustworthy as its least certain link, and a
		// model summarizing the path needs that number, not the average.
		fmt.Fprintf(&b, "\n     weakest link: %.2f", weakest)
		if !r.Itemf("%s", b.String()) {
			break
		}
	}
	return textResult(r.String()), nil, nil
}

// DiagnosticsArgs filters the gap report.
type DiagnosticsArgs struct {
	Severity string `json:"severity,omitempty" jsonschema:"filter by severity: info, warn, error"`
	Code     string `json:"code,omitempty" jsonschema:"filter by diagnostic code"`
	Cursor   int    `json:"cursor,omitempty" jsonschema:"offset to resume from"`
}

// diagnostics reports what the scan could not do.
//
// This is what stops the graph from being read as complete. A model that
// concludes a service has no database, when in fact the chart declaring it
// was never rendered, has been misled by an omission it had no way to see.
func (s *Server) diagnostics(ctx context.Context, _ *mcp.CallToolRequest, args DiagnosticsArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	if args.Severity != "" && !schema.Severity(strings.ToLower(args.Severity)).Valid() {
		return errorResult("unknown severity %q; valid values are %s",
			args.Severity, joinSeverities()), nil, nil
	}

	var matched []schema.Diagnostic
	for _, d := range g.Diagnostics {
		if args.Severity != "" && !strings.EqualFold(string(d.Severity), args.Severity) {
			continue
		}
		if args.Code != "" && !strings.EqualFold(d.Code, args.Code) {
			continue
		}
		matched = append(matched, d)
	}

	r := NewResponse(budgetDiagnostics)
	if len(matched) == 0 {
		// "No diagnostics" is read as "nothing is missing", and the two are
		// not the same thing. What this scan recognized, it parsed; what it
		// does not recognize it never mentions, and neither does it read
		// application code at all.
		if args.Code != "" || args.Severity != "" {
			r.Headerf("No diagnostics match that filter. Call structura_diagnostics with no arguments for all of them.")
			return textResult(r.String()), nil, nil
		}
		r.Headerf("No gaps to report: everything this scan recognized, it parsed.")
		r.Headerf("")
		r.Headerf("That is not the same as nothing being missing. Structura reads")
		r.Headerf("configuration, not application code, so a dependency that exists only")
		r.Headerf("in source -- a client built at runtime, a URL assembled from parts --")
		r.Headerf("is not in this graph and is not counted here.")
		return textResult(r.String()), nil, nil
	}
	r.Headerf("%d %s. Each one is a part of the architecture that may be missing from the graph.",
		len(matched), plural(len(matched), "gap", "gaps"))

	start := args.Cursor
	if start < 0 || start >= len(matched) {
		start = 0
	}
	// Only on the first page. A continuation is a second call in the same
	// conversation, so the model still has the breakdown in front of it and
	// repeating it would spend budget that belongs to the entries.
	if start == 0 {
		writeCodeBreakdown(r, matched)
	}
	r.Headerf("")
	r.Countf(start)
	r.Expect(len(matched))

	for _, d := range matched[start:] {
		line := fmt.Sprintf("  [%s] %s", d.Severity, d.Code)
		if d.Path != "" {
			line += "  " + location(d.Path, d.Line)
		}
		line += "\n      " + d.Message
		if !r.Itemf("%s", line) {
			break
		}
	}
	return textResult(r.StringWithCursor(fmt.Sprintf("%d", start+r.Shown()), "diagnostics")), nil, nil
}

// maxBreakdownCodes bounds the breakdown, which is written as headers and so
// is never truncated. A repository that somehow produced every diagnostic
// this project can emit must not be able to spend the whole budget on the
// summary of them.
const maxBreakdownCodes = 12

// writeCodeBreakdown lists how many diagnostics each code accounts for.
//
// Diagnostics are sorted by path, so a truncated list is arbitrary with
// respect to the kind of problem it reports -- and the distribution is
// steeply skewed, which makes that worse than it sounds. On online-boutique
// the budget goes on twenty-one near-identical kustomize_unrendered entries
// and runs out before the model ever learns that a Helm chart went
// unrendered too, that a patch fragment was skipped, or that a Terraform
// resource type was unrecognized: 17 of 38 shown, and the 21 missing span
// three problem classes it never hears about.
//
// A model asking what is missing from the graph needs the shape of what is
// missing more than it needs a twenty-first instance of one entry. The
// breakdown costs a few tokens, is complete even when the detail below it is
// not, and is also the only way to learn what to pass to code=.
func writeCodeBreakdown(r *Response, matched []schema.Diagnostic) {
	counts := map[string]int{}
	for _, d := range matched {
		counts[d.Code]++
	}
	// Nothing to summarize when every entry is its own kind: the breakdown
	// would restate the list it is meant to compress.
	if len(counts) < 2 || len(matched) <= len(counts) {
		return
	}

	codes := make([]string, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		if counts[codes[i]] != counts[codes[j]] {
			return counts[codes[i]] > counts[codes[j]]
		}
		return codes[i] < codes[j]
	})

	r.Headerf("")
	shown := codes
	if len(shown) > maxBreakdownCodes {
		shown = shown[:maxBreakdownCodes]
	}
	for _, c := range shown {
		r.Headerf("  %4d  %s", counts[c], c)
	}
	if rest := len(codes) - len(shown); rest > 0 {
		var n int
		for _, c := range codes[len(shown):] {
			n += counts[c]
		}
		r.Headerf("  %4d  across %d other %s", n, rest, plural(rest, "code", "codes"))
	}
	r.Headerf("")
	r.Headerf("Pass code=<name> for every entry of one kind.")
}

// shapeNote warns when a graph is an inventory rather than a system.
//
// A repository of two hundred unrelated charts produces four hundred
// components and a handful of relationships. Reported as "417 components" it
// reads exactly like a large architecture, and a model asked what depends on
// what will describe one. The counts are true and the impression is false, so
// the shape has to be stated rather than left to be inferred from them.
func shapeNote(g schema.Graph) string {
	var components int
	for _, n := range g.Nodes {
		if n.Kind != schema.KindBoundary {
			components++
		}
	}
	if components < 8 {
		// Too small for the ratio to mean anything.
		return ""
	}

	cohesion := g.Cohesion()
	switch {
	case cohesion < 0.25 && g.Stats.Clusters > components/4:
		return fmt.Sprintf(
			"This does not look like one system. %d of %d components have no relationship "+
				"to any other, and what remains falls into %d independent groups — the shape "+
				"of a package or example collection rather than an architecture. Treat this "+
				"as an inventory; questions about how components interact will mostly have "+
				"no answer here.",
			components-g.Stats.Connected, components, g.Stats.Clusters)
	case cohesion < 0.5:
		return fmt.Sprintf(
			"Only %d of %d components have any relationship; the rest stand alone. "+
				"structura_diagnostics explains what could not be read, which is often why.",
			g.Stats.Connected, components)
	}
	return ""
}

func countRelationships(g schema.Graph) int {
	n := 0
	for _, e := range g.Edges {
		if e.Kind != schema.EdgeContains {
			n++
		}
	}
	return n
}

func location(path string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", path, line)
	}
	return path
}

func describeFilter(f nodeFilter) string {
	var parts []string
	if f.kind != "" {
		parts = append(parts, "kind="+f.kind)
	}
	if f.layer != "" {
		parts = append(parts, "layer="+f.layer)
	}
	if f.namespace != "" {
		parts = append(parts, "namespace="+f.namespace)
	}
	if f.query != "" {
		parts = append(parts, "query="+f.query)
	}
	if len(parts) == 0 {
		return "any filter"
	}
	return strings.Join(parts, " ")
}

// validateEnums rejects a filter value before it silently matches nothing.
//
// A model that asks for kind="database" and is told "0 components match" will
// conclude the system has no databases. Telling it the word it wants is
// "datastore" costs one call instead of a wrong answer.
func validateEnums(kind, layer string) (message string, ok bool) {
	if kind != "" && !schema.NodeKind(strings.ToLower(kind)).Valid() {
		names := make([]string, 0, len(schema.NodeKinds()))
		for _, k := range schema.NodeKinds() {
			names = append(names, string(k))
		}
		return fmt.Sprintf("unknown kind %q; valid values are %s", kind, strings.Join(names, ", ")), false
	}
	if layer != "" && !schema.Layer(strings.ToLower(layer)).Valid() {
		names := make([]string, 0, len(schema.Layers()))
		for _, l := range schema.Layers() {
			names = append(names, string(l))
		}
		return fmt.Sprintf("unknown layer %q; valid values are %s", layer, strings.Join(names, ", ")), false
	}
	return "", true
}

func joinSeverities() string {
	names := make([]string, 0, len(schema.Severities()))
	for _, s := range schema.Severities() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

func candidateHint(candidates []string) string {
	if len(candidates) == 0 {
		return ". Use structura_list_nodes to see what exists."
	}
	return ": " + strings.Join(candidates, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ImpactOfArgs identifies a component and which way to look from it.
type ImpactOfArgs struct {
	Node      string `json:"node" jsonschema:"the component to start from, by id or name"`
	Direction string `json:"direction,omitempty" jsonschema:"dependents (what breaks if this fails, the default) or dependencies (what this needs to work)"`
	MaxDepth  int    `json:"maxDepth,omitempty" jsonschema:"how many hops to follow; defaults to 8"`
}

// impactOf reports the transitive closure around a component.
//
// describe_node answers one hop, and "what breaks if the database goes down"
// is not a one-hop question. Recovering the answer from one-hop calls means
// one call per component, a graph walk performed by hand, and no way to know
// when the set has closed -- which is both the token cost this server exists
// to avoid and a walk a model gets wrong in a way that reads as confident.
func (s *Server) impactOf(ctx context.Context, _ *mcp.CallToolRequest, args ImpactOfArgs) (*mcp.CallToolResult, any, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return errorResult("could not read the architecture graph: %v", err), nil, nil
	}
	v := newView(g)

	dependents := true
	switch strings.ToLower(strings.TrimSpace(args.Direction)) {
	case "", "dependents", "upstream":
	case "dependencies", "downstream":
		dependents = false
	default:
		return errorResult("unknown direction %q; use dependents (what breaks if this fails) "+
			"or dependencies (what this needs to work)", args.Direction), nil, nil
	}

	id, candidates, err := v.resolveRef(args.Node)
	if err != nil {
		return errorResult("%v%s", err, candidateHint(candidates)), nil, nil
	}
	n := v.byID[id]

	found := v.reachable(id, dependents, args.MaxDepth)
	r := NewResponse(budgetImpact)

	if len(found) == 0 {
		writeEmptyImpact(r, v, n, dependents)
		return textResult(r.String()), nil, nil
	}

	verb := "depend on"
	if !dependents {
		verb = "are depended on by"
	}
	// The share matters as much as the count. A datastore half the system
	// reaches is a different risk from one two services use, and "12
	// components" alone does not distinguish them.
	components := countComponents(g)
	r.Headerf("%d of %d components %s %s, directly or indirectly.",
		len(found), components, verb, v.label(id))

	if weakest := weakestReach(found); weakest < schema.ConfDeclared {
		r.Headerf("Some of it is reached only through inferred edges, the weakest at %.2f.", weakest)
	}

	for depth := 1; depth <= maxReachDepth; depth++ {
		at := reachAtDepth(found, depth)
		if len(at) == 0 {
			continue
		}
		r.Headerf("")
		r.Headerf("%s (%d):", hopHeading(depth), len(at))
		r.Expect(len(at))
		for _, c := range at {
			line := fmt.Sprintf("  %-28s %s (%.2f)", v.label(c.id), c.kind, c.weakest)
			if depth > 1 {
				line += "  via " + v.label(c.via)
			}
			if !r.Itemf("%s", line) {
				break
			}
		}
	}

	r.Headerf("")
	r.Headerf("Distance is the shortest route, and the confidence is the weakest edge on it.")
	r.Headerf("structura_trace_path shows the routes themselves; structura_diagnostics")
	r.Headerf("lists what the scan could not read, which is what this set may be missing.")
	return textResult(r.String()), nil, nil
}

// writeEmptyImpact explains an empty closure rather than stating one.
//
// Nothing reaching a component is a real and common answer, but so is the
// scan having been unable to see what does -- and those read identically
// unless the difference is spelled out.
func writeEmptyImpact(r *Response, v *view, n *schema.Node, dependents bool) {
	if dependents {
		r.Headerf("Nothing in the graph depends on %s.", v.label(n.ID))
	} else {
		r.Headerf("%s depends on nothing in the graph.", v.label(n.ID))
	}
	r.Headerf("")

	related := v.diagnosticsFor(n)
	if len(related) == 0 {
		r.Headerf("Nothing was reported about the files this component is declared in")
		r.Headerf("either. It may genuinely stand alone, or it may be reached from")
		r.Headerf("application code, which a configuration scan cannot see.")
		return
	}
	r.Headerf("The scan did report problems with the files it is declared in, which")
	r.Headerf("may be why:")
	r.Expect(len(related))
	for _, d := range related {
		if !r.Itemf("  [%s] %s at %s\n      %s",
			d.Severity, d.Code, location(d.Path, d.Line), d.Message) {
			break
		}
	}
	r.Headerf("")
	r.Headerf("(Matched by file, so an entry may concern another object in the same file.)")
}

func hopHeading(depth int) string {
	if depth == 1 {
		return "Directly"
	}
	return fmt.Sprintf("%d hops away", depth)
}

func reachAtDepth(found []reach, depth int) []reach {
	var out []reach
	for _, c := range found {
		if c.depth == depth {
			out = append(out, c)
		}
	}
	return out
}

func weakestReach(found []reach) float64 {
	weakest := 1.0
	for _, c := range found {
		if c.weakest < weakest {
			weakest = c.weakest
		}
	}
	return weakest
}

// countComponents excludes boundaries, which are groupings rather than things
// that can break.
func countComponents(g schema.Graph) int {
	var n int
	for _, node := range g.Nodes {
		if node.Kind != schema.KindBoundary {
			n++
		}
	}
	return n
}

// projectCount is one project's share of a graph.
type projectCount struct {
	name  string
	count int
}

// projectCounts groups a graph's components by the project they belong to,
// largest first. A single-project repository returns one entry, which callers
// use to stay silent: saying "1 project" to every reader is noise.
func projectCounts(g schema.Graph) []projectCount {
	counts := map[string]int{}
	for _, n := range g.Nodes {
		name := n.Project
		if name == "" {
			name = "."
		}
		counts[name]++
	}

	out := make([]projectCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, projectCount{name: name, count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}
