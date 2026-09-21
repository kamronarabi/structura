package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kamronarabi/structura/internal/buildinfo"
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

	// MERGE: fold the per-file buffers together, single-threaded and in the
	// walker's sorted path order, so the result does not depend on which
	// worker finished first.
	merge := schema.NewBuilder()

	diags := append([]schema.Diagnostic(nil), walkRes.Diagnostics...)

	var (
		hints   []resolve.Hint
		aliases []resolve.Alias
	)
	parsed := 0
	for _, out := range outputs {
		if out.contributed() {
			parsed++
		}
		for _, n := range out.Nodes {
			merge.AddNode(n)
		}
		diags = append(diags, out.Diagnostics...)
		hints = append(hints, out.Hints...)
		aliases = append(aliases, out.Aliases...)
	}

	mergedNodes, mergeDiags := merge.Nodes()
	diags = append(diags, mergeDiags...)

	// Edges declared by extractors are collected separately from the merged
	// node set, because the resolver may rewrite their endpoints when it
	// discovers that two nodes are one component.
	var declaredEdges []schema.Edge
	for _, out := range outputs {
		declaredEdges = append(declaredEdges, out.Edges...)
	}

	// RESOLVE: match unresolved references against the finished node set.
	resolved := resolve.Resolve(resolve.Input{
		Nodes:   mergedNodes,
		Edges:   declaredEdges,
		Hints:   hints,
		Aliases: aliases,
	})
	diags = append(diags, resolved.Diagnostics...)

	builder := schema.NewBuilder()
	for _, n := range resolved.Nodes {
		builder.AddNode(n)
	}
	for _, e := range resolved.Edges {
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
