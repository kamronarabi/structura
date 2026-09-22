package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kamronarabi/structura/internal/buildinfo"
	"github.com/kamronarabi/structura/internal/override"
	"github.com/kamronarabi/structura/internal/project"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Options configures a scan.
type Options struct {
	// Root is the absolute path of the repository to scan.
	Root string

	MaxFileSize           int64
	FollowSymlinkedDirs   bool
	DisableDefaultIgnores bool

	// Concurrency bounds the extraction stage. Zero means GOMAXPROCS.
	Concurrency int

	// Projects names the directories that hold independent projects, for a
	// repository whose boundaries detection cannot see. Empty means detect,
	// and detection defaults to treating the repository as one project.
	Projects []string

	// Overrides are the corrections the repository declares: relationships
	// configuration does not express, relationships inference got wrong, and
	// components it drew twice. They are applied last and they win.
	Overrides override.Rules

	// KeepIntermediate retains the pre-resolution output for --debug-dump.
	// It is off by default because it holds every hint in memory, including
	// unredacted raw values.
	KeepIntermediate bool
}

// Result is a completed scan.
type Result struct {
	Graph schema.Graph
	// Intermediate is populated only when Options.KeepIntermediate is set.
	Intermediate *Intermediate
}

// Intermediate is the raw extractor output, before resolution, grouped by the
// file that produced it.
//
// This is what makes the extractor milestones observable before the resolver
// exists. Extractors produce nodes and hints long before anything turns those
// hints into edges, so without it the only feedback on a new extractor is an
// unchanged, edgeless graph. It stays in the shipped binary as the answer to
// "why didn't Structura find my service?".
type Intermediate struct {
	Files []FileOutput
}

// FileOutput is everything one file contributed.
type FileOutput struct {
	Path        string
	Extractors  []string
	Nodes       []schema.Node
	Edges       []schema.Edge
	Hints       []resolve.Hint
	Aliases     []resolve.Alias
	Diagnostics []schema.Diagnostic
	// Err is set when the file could not be read or an extractor failed.
	Err string
}

// contributed reports whether a file produced anything at all. Some
// extractors claim a file on extension alone and discover on opening it that
// it is not theirs — most YAML in a repository is not a Kubernetes manifest —
// so "matched" is a poor proxy for "parsed" in the stats.
func (f FileOutput) contributed() bool {
	return f.Err == "" && (len(f.Nodes) > 0 || len(f.Edges) > 0 ||
		len(f.Hints) > 0 || len(f.Aliases) > 0 || len(f.Diagnostics) > 0)
}

// Run executes the scan pipeline:
//
//	DISCOVER   walk the tree, applying ignore rules and guards
//	EXTRACT    run matching extractors in parallel, into per-worker buffers
//	MERGE      fold buffers together single-threaded, in sorted path order
//	RESOLVE    match hints against the identity index to produce edges
//	REDACT     mask credentials before anything reaches disk
//	NORMALIZE  sort, dedupe, hash
//
// Extraction is the only concurrent stage, and it shares no mutable state.
// Everything after it is single-threaded and order-fixed, which is what makes
// the output byte-identical between runs.
func Run(ctx context.Context, reg *Registry, opts Options) (Result, error) {
	started := time.Now()

	walkRes, err := Walk(ctx, opts.Root, reg, WalkOptions{
		MaxFileSize:           opts.MaxFileSize,
		FollowSymlinkedDirs:   opts.FollowSymlinkedDirs,
		DisableDefaultIgnores: opts.DisableDefaultIgnores,
	})
	if err != nil {
		return Result{}, err
	}

	outputs, err := extractAll(ctx, reg, opts, walkRes.Files)
	if err != nil {
		return Result{}, err
	}

	// PROJECTS: decide where one project ends and the next begins, before
	// anything is merged. A repository is one project unless something says
	// otherwise, and in that case nothing below this line changes -- the
	// identifiers, and therefore every stored graph, stay exactly as they
	// were.
	projects := project.Discover(opts.Projects)
	var projectDiags []schema.Diagnostic
	if projects.Len() > 1 {
		projectDiags = qualifyByProject(outputs, projects)
	}

	// MERGE: fold the per-file buffers together, single-threaded and in the
	// walker's sorted path order, so the result does not depend on which
	// worker finished first.
	merge := schema.NewBuilder()

	diags := append([]schema.Diagnostic(nil), walkRes.Diagnostics...)
	diags = append(diags, projectDiags...)

	var (
		hints   []resolve.Hint
		aliases []resolve.Alias
		// Every reading, kept apart from the merged set: two accounts of one
		// identifier are what the undeclared-project check looks for, and
		// merging is what destroys the difference between them.
		readings []schema.Node
	)
	parsed := 0
	for _, out := range outputs {
		if out.contributed() {
			parsed++
		}
		for _, n := range out.Nodes {
			merge.AddNode(n)
			readings = append(readings, n)
		}
		diags = append(diags, out.Diagnostics...)
		hints = append(hints, out.Hints...)
		aliases = append(aliases, out.Aliases...)
	}

	mergedNodes, mergeDiags := merge.Nodes()
	diags = append(diags, mergeDiags...)

	// What the scan never opened is as much a gap as what it failed to parse,
	// and silence about it reads as completeness.
	diags = append(diags, unsupportedDiagnostics(walkRes.Unsupported)...)

	// Edges declared by extractors are collected separately from the merged
	// node set, because the resolver may rewrite their endpoints when it
	// discovers that two nodes are one component.
	var declaredEdges []schema.Edge
	for _, out := range outputs {
		declaredEdges = append(declaredEdges, out.Edges...)
	}

	// Asked of the readings rather than of the resolved graph: what this
	// looks for is two files that produced the same identifier, and
	// resolution deliberately merges nodes that did not.
	diags = append(diags, undeclaredProjectDiagnostics(readings, projects)...)

	// RESOLVE: match unresolved references against the finished node set.
	resolved := resolve.Resolve(resolve.Input{
		Nodes:   mergedNodes,
		Edges:   declaredEdges,
		Hints:   hints,
		Aliases: aliases,
	})
	diags = append(diags, resolved.Diagnostics...)

	// Detection cannot split a repository safely, but it can ask. Without
	// this the boundary machinery only helps people who already knew they
	// needed it.
	// OVERRIDE: what the repository declared outright, over what was
	// inferred. After resolution, because a declared relationship may name a
	// component that only exists once resolution has finished; before the
	// graph is built, so the corrected content is what gets hashed and
	// validated rather than being patched into a finished graph.
	corrected, overrideDiags := override.Apply(resolved.Nodes, resolved.Edges, opts.Overrides)
	diags = append(diags, overrideDiags...)

	builder := schema.NewBuilder()
	for _, n := range corrected.Nodes {
		builder.AddNode(n)
	}
	for _, e := range corrected.Edges {
		builder.AddEdge(e)
	}
	for _, d := range aggregateDiagnostics(diags) {
		builder.Diag(d)
	}

	root := schema.Root{
		Name: filepath.Base(opts.Root),
		VCS:  readVCS(ctx, opts.Root),
	}
	stats := schema.Stats{
		FilesScanned: walkRes.Scanned,
		FilesParsed:  parsed,
	}

	graph, err := builder.Build(root, stats)
	if err != nil {
		return Result{}, err
	}

	// REDACT: nothing reaches disk without passing through here. "Every
	// extractor remembered to redact" is not a property anyone can verify;
	// "nothing is serialized without this step" is.
	resolve.RedactGraph(&graph)

	// Stamped here rather than in the builder, because pkg/schema is the
	// public API and must not depend on this binary's build metadata.
	graph.Generator = &schema.Generator{Name: "structura", Version: buildinfo.BuildID()}
	graph.GeneratedAt = time.Now().UTC().Truncate(time.Second)
	graph.Stats.DurationMs = time.Since(started).Milliseconds()
	graph.Normalize()

	res := Result{Graph: graph}
	if opts.KeepIntermediate {
		res.Intermediate = &Intermediate{Files: outputs}
	}
	return res, nil
}

// extractAll runs the matching extractors over every file, bounded by
// Concurrency, and returns the per-file output in the input order.
func extractAll(ctx context.Context, reg *Registry, opts Options, files []FileMeta) ([]FileOutput, error) {
	outputs := make([]FileOutput, len(files))

	limit := opts.Concurrency
	if limit <= 0 {
		limit = runtime.GOMAXPROCS(0)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)

	for i, meta := range files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Each goroutine owns its slot, so no synchronization is needed
			// around the shared slice.
			outputs[i] = extractFile(ctx, reg, opts.Root, meta)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return outputs, nil
}

func extractFile(ctx context.Context, reg *Registry, root string, meta FileMeta) FileOutput {
	out := FileOutput{Path: meta.Path}

	matched := reg.MatchAll(meta)
	if len(matched) == 0 {
		return out
	}
	for _, e := range matched {
		out.Extractors = append(out.Extractors, e.Name())
	}

	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(meta.Path)))
	if err != nil {
		out.Err = err.Error()
		out.Diagnostics = append(out.Diagnostics, schema.Diagnostic{
			Severity: schema.SeverityWarn,
			Code:     "unreadable_file",
			Path:     meta.Path,
			Message:  fmt.Sprintf("could not read file: %v", rootRelativeError(err, root)),
		})
		return out
	}

	buf := &collector{out: &out}
	file := &File{FileMeta: meta, Content: content}

	for _, e := range matched {
		runExtractor(ctx, e, file, buf)
	}
	return out
}

// runExtractor isolates one extractor's failure from the rest of the scan.
//
// Extractors run on untrusted input: a malformed manifest, a YAML bomb, a
// deeply nested structure someone committed by accident. One bad file must
// degrade into a diagnostic, never take the whole scan down — a tool that
// crashes on a repository tells the user nothing about the other nine
// hundred files it had already understood.
func runExtractor(ctx context.Context, e Extractor, file *File, emit Emitter) {
	defer func() {
		if r := recover(); r != nil {
			emit.Diag(schema.Diagnostic{
				Severity: schema.SeverityError,
				Code:     "extractor_panic",
				Path:     file.Path,
				Message: fmt.Sprintf("extractor %q panicked: %v; this is a bug, please report it with the file that triggered it",
					e.Name(), r),
			})
			// The stack goes to stderr, where it helps whoever is debugging,
			// and stays out of graph.json, which gets committed.
			debugStack(e.Name(), file.Path, r)
		}
	}()

	if err := e.Extract(ctx, file, emit); err != nil {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityWarn,
			Code:     "extract_failed",
			Path:     file.Path,
			Message:  fmt.Sprintf("extractor %q could not parse this file: %v", e.Name(), err),
		})
	}
}

// debugStack is a variable so tests can silence it.
var debugStack = func(extractor, path string, recovered any) {
	fmt.Fprintf(os.Stderr, "structura: panic in extractor %s on %s: %v\n%s\n",
		extractor, path, recovered, debug.Stack())
}

// collector is the per-file Emitter. It is owned by a single goroutine, so it
// needs no locking.
type collector struct{ out *FileOutput }

func (c *collector) Node(n schema.Node)       { c.out.Nodes = append(c.out.Nodes, n) }
func (c *collector) Edge(e schema.Edge)       { c.out.Edges = append(c.out.Edges, e) }
func (c *collector) Hint(h resolve.Hint)      { c.out.Hints = append(c.out.Hints, h) }
func (c *collector) Alias(a resolve.Alias)    { c.out.Aliases = append(c.out.Aliases, a) }
func (c *collector) Diag(d schema.Diagnostic) { c.out.Diagnostics = append(c.out.Diagnostics, d) }

// qualifyByProject rewrites one project's identifiers so that they cannot
// collide with another's.
//
// Two independent stacks in one repository routinely declare the same
// namespace -- "prod" is not a distinctive word -- and the node ID has no
// segment that records which stack a component came from. The result is a
// single node claiming members from both, and no diagnostic, because as far
// as the builder is concerned two files simply described the same thing.
//
// Rewriting is safe to do per file because an extractor sees one file and
// nothing else: every identifier in a file's output was computed from that
// file, so qualifying the whole output at once keeps it internally
// consistent. References that reach across files -- a pod naming a ConfigMap
// declared elsewhere -- are qualified by the same rule from the same project,
// so the two halves still meet.
//
// Only the ID is qualified. Node.Namespace keeps the namespace the manifest
// actually declared, because that is what a reader is shown and what another
// file writes when it refers to the node; the project is a separate dimension
// and is recorded separately.
//
// The repository-root project is never qualified. A repository with one
// project must produce byte-identical output to one scanned before this
// existed.
func qualifyByProject(outputs []FileOutput, projects *project.Set) []schema.Diagnostic {
	labels, diags := projectLabels(outputs, projects)

	for i := range outputs {
		out := &outputs[i]
		root := projects.Of(out.Path)
		label := labels[root]
		if label == "" {
			continue
		}

		for j := range out.Nodes {
			n := &out.Nodes[j]
			if id, err := schema.QualifyProject(n.ID, label); err == nil {
				n.ID = id
			}
			n.Project = root
		}
		for j := range out.Edges {
			e := &out.Edges[j]
			if id, err := schema.QualifyProject(e.From, label); err == nil {
				e.From = id
			}
			if id, err := schema.QualifyProject(e.To, label); err == nil {
				e.To = id
			}
		}
		for j := range out.Hints {
			h := &out.Hints[j]
			if h.FromNode != "" {
				if id, err := schema.QualifyProject(h.FromNode, label); err == nil {
					h.FromNode = id
				}
			}
			// A ConfigMap reference travels in Raw as a node ID, and has to
			// be qualified the same way or the pod and the ConfigMap stop
			// meeting.
			if strings.HasPrefix(h.Raw, "configmap:") {
				if id, err := schema.QualifyProject(h.Raw, label); err == nil {
					h.Raw = id
				}
			}
		}
		for j := range out.Aliases {
			// An alias is matched against nodes, so it has to know which
			// project's nodes it may match. Its namespace is left alone: a
			// namespace is a namespace in any project, and conflating the two
			// is what this change exists to stop.
			out.Aliases[j].Project = root
		}
	}
	return diags
}

// projectLabels chooses the label each project's namespaces are qualified
// with, and it exists because QualifyNamespace cannot choose it safely.
//
// That function joins the label and the namespace with a hyphen and folds the
// two together, and a hyphen is an ordinary character in both. So the project
// "web-api" with a namespace "prod" and the project "web" with a namespace
// "api-prod" produce one segment, and two projects merge into the one
// boundary qualification exists to keep apart. QualifyNamespace is handed one
// identifier at a time and has no way to notice; this pass holds every
// project and every namespace in the repository at once, and can.
//
// So it checks, by folding each project's namespaces the same way
// QualifyNamespace will and looking for two projects landing on one segment.
// If none do -- which is the case for nearly every repository -- every
// project keeps the readable label and every identifier is exactly what it
// was before this function existed. If any do, every label gains a short
// digest of its project root. The roots are distinct paths, so the digests
// are distinct, and prefixing them separates projects whose names would
// otherwise run together.
func projectLabels(outputs []FileOutput, projects *project.Set) (map[string]string, []schema.Diagnostic) {
	labels := map[string]string{}
	for _, root := range projects.Roots() {
		labels[root] = project.Label(root)
	}

	namespaces := projectNamespaces(outputs, projects)
	if !foldsCollide(namespaces, labels) {
		return labels, nil
	}

	for root := range labels {
		if labels[root] == "" {
			continue
		}
		labels[root] += "-" + rootDigest(root)
	}

	// Distinct roots give distinct digests, so this settles it. Saying so out
	// loud rather than trusting it: a repository whose identifiers silently
	// merged two projects is the failure this whole pass exists to prevent,
	// and it should not be able to happen quietly a second time.
	if foldsCollide(namespaces, labels) {
		return labels, []schema.Diagnostic{{
			Severity: schema.SeverityWarn,
			Code:     "project_namespace_collision",
			Message: "two projects in this repository produce the same qualified namespace even after disambiguation; " +
				"their components may appear merged into one boundary",
		}}
	}
	return labels, nil
}

// projectNamespaces reports which namespaces each project actually uses,
// taken from the identifiers as extracted rather than from configuration:
// only a namespace that reaches a node can collide with another.
func projectNamespaces(outputs []FileOutput, projects *project.Set) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for i := range outputs {
		root := projects.Of(outputs[i].Path)
		for _, n := range outputs[i].Nodes {
			parsed, err := schema.ParseNodeID(n.ID)
			if err != nil {
				continue
			}
			if out[root] == nil {
				out[root] = map[string]bool{}
			}
			out[root][parsed.Scope] = true
		}
	}
	return out
}

// foldsCollide reports whether two different projects' namespaces fold to one
// qualified segment under the given labels.
//
// The fold is performed by QualifyProject itself, on a throwaway identifier,
// so this cannot drift from the rule it is checking.
func foldsCollide(namespaces map[string]map[string]bool, labels map[string]string) bool {
	owner := map[string]string{}
	for root, set := range namespaces {
		label := labels[root]
		if label == "" {
			continue
		}
		for ns := range set {
			probe, err := schema.QualifyProject(
				schema.NewNodeID(schema.KindBoundary, ns, "probe"), label)
			if err != nil {
				continue
			}
			if prev, seen := owner[probe]; seen && prev != root {
				return true
			}
			owner[probe] = root
		}
	}
	return false
}

// rootDigest is a short, stable identifier for a project root. It is only
// ever used to separate two labels that would otherwise run together, so it
// needs to be distinct and reproducible, not unguessable.
func rootDigest(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:6]
}

// undeclaredProjectDiagnostics reports trees that look like separate projects
// and are not declared as any.
//
// This changes nothing about the graph. It is the one place the scan says "I
// may have merged two systems", and it exists because the alternative -- a
// silently merged graph and a person who never learns the configuration key
// exists -- is how the collision went unnoticed in the first place.
func undeclaredProjectDiagnostics(readings []schema.Node, set *project.Set) []schema.Diagnostic {
	suspects := project.Suspects(readings, set)
	out := make([]schema.Diagnostic, 0, len(suspects))
	for _, s := range suspects {
		out = append(out, schema.Diagnostic{
			Severity: schema.SeverityWarn,
			Code:     "undeclared_projects",
			Message: fmt.Sprintf(
				"%s declare %s, and %s. Their components are currently merged as "+
					"though they were one system. If they are separate, list them under "+
					"\"projects:\" in .structura.yaml; if they are one, nothing needs doing",
				trees(s.Trees), list(s.Shared), because(s.Ground)),
		})
	}
	return out
}

// trees names the directories in question. Two is the common case and reads
// as a sentence; a repository of sample stacks produces a dozen and reads as
// a list, so the two are spelled differently.
func trees(dirs []string) string {
	if len(dirs) == 2 {
		return dirs[0] + " and " + dirs[1] + " both"
	}
	return list(dirs) + " all"
}

// because turns the grounds for suspicion into the clause that says why,
// because the two grounds are different observations and one sentence
// covering both would describe neither.
func because(g project.Ground) string {
	if g == project.GroundDescribedDifferently {
		return "describe it differently -- a different image, or built from a " +
			"different directory -- which is what two components sharing a name look like"
	}
	return "share no other component, which is what two independent projects look like"
}
