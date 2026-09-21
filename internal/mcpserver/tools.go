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
)

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
	r.Headerf("%d components, %d relationships, from %d files",
		len(g.Nodes), countRelationships(g), g.Stats.FilesParsed)
	if note := shapeNote(g); note != "" {
		r.Headerf("")
		r.Headerf("%s", note)
	}

	if len(g.Nodes) == 0 {
		r.Headerf("")
		r.Headerf("No components were found. structura_diagnostics explains why.")
		return textResult(r.String()), nil, nil
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
		for i, item := range byDegree {
			if i >= 8 {
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

	if len(v.outgoing[id]) == 0 && len(v.incoming[id]) == 0 {
		r.Headerf("")
		r.Headerf("No relationships were found for this component. This may mean it")
		r.Headerf("genuinely has none, or that its dependencies are expressed in source")
		r.Headerf("code rather than configuration. structura_diagnostics may say which.")
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
		r.Headerf("No diagnostics. Everything this scan recognized, it parsed.")
		return textResult(r.String()), nil, nil
	}
	r.Headerf("%d %s. Each one is a part of the architecture that may be missing from the graph.",
		len(matched), plural(len(matched), "gap", "gaps"))
	r.Headerf("")

	start := args.Cursor
	if start < 0 || start >= len(matched) {
		start = 0
	}
	r.Countf(start)

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
