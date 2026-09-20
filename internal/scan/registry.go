package scan

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

// FileMeta is what the walker knows about a file without opening it.
type FileMeta struct {
	// Path is repo-relative and slash-separated on every platform.
	Path string
	// Dir is the repo-relative parent directory, "" at the root.
	Dir string
	// Name is the base name, with extension.
	Name string
	// Ext is the lowercased extension including the leading dot, "" if none.
	Ext string
	// Size is the file size in bytes.
	Size int64
	// ModTime is the file's modification time, as a Unix timestamp. It is
	// what the MCP server compares against a stored graph to decide whether
	// to rescan, and is half of the cache key the pipeline reserves.
	ModTime int64
}

func newFileMeta(relPath string, size, modTime int64) FileMeta {
	return FileMeta{
		Path:    relPath,
		Dir:     slashDir(relPath),
		Name:    path.Base(relPath),
		Ext:     strings.ToLower(path.Ext(relPath)),
		Size:    size,
		ModTime: modTime,
	}
}

func slashDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// File is a matched file with its contents loaded.
type File struct {
	FileMeta
	Content []byte
}

// Extractor turns one file into graph elements.
//
// This interface is the plan's load-bearing abstraction: adding a format or a
// language must be registration, never a refactor. Phase 2's Go and
// TypeScript AST extractors implement this same contract, as will every
// language after them.
//
// Three rules keep it from rotting:
//
//  1. Match never reads file contents. Classification has to stay cheap so
//     the walker can run over a large monorepo in seconds.
//
//  2. An extractor sees one file and nothing else. Cross-file knowledge is
//     emitted as a resolve.Hint. This is what makes extractors independently
//     testable, safe to run in parallel, and order-independent.
//
//  3. An extractor never panics. It runs on untrusted input; the pipeline
//     recovers at its boundary and converts a panic into a diagnostic, but an
//     extractor that relies on that is hiding a bug.
type Extractor interface {
	// Name is stable and appears in every evidence and source record.
	Name() string

	// Match decides whether this extractor handles the file, using only the
	// metadata the walker already has.
	Match(f FileMeta) bool

	// Extract parses the file and emits what it found.
	Extract(ctx context.Context, f *File, emit Emitter) error
}

// Emitter receives an extractor's output.
type Emitter interface {
	// Node records a component that exists.
	Node(schema.Node)
	// Edge records a relationship knowable from this file alone. Anything
	// requiring another file's contents is a Hint instead.
	Edge(schema.Edge)
	// Hint records an unresolved reference for the global resolver.
	Hint(resolve.Hint)
	// Alias records a name that routes to a component rather than being one,
	// such as a Kubernetes Service. The resolver attaches it to whichever
	// node it turns out to select.
	Alias(resolve.Alias)
	// Diag records a gap: something the extractor noticed but could not
	// model. Reporting a gap is always better than guessing at it.
	Diag(schema.Diagnostic)
}

// Registry holds the extractors a scan will run.
type Registry struct {
	extractors []Extractor
}

// NewRegistry returns a registry containing the given extractors.
func NewRegistry(extractors ...Extractor) *Registry {
	r := &Registry{}
	for _, e := range extractors {
		r.Register(e)
	}
	return r
}

// Register adds an extractor. Registration order does not affect output:
// extractors are sorted by name before use, and the merge step is
// order-independent regardless.
func (r *Registry) Register(e Extractor) {
	r.extractors = append(r.extractors, e)
	sort.SliceStable(r.extractors, func(i, j int) bool {
		return r.extractors[i].Name() < r.extractors[j].Name()
	})
}

// MatchAll returns every extractor that claims the file. More than one may:
// a values.yaml is both a Helm signal and a plain YAML document.
func (r *Registry) MatchAll(f FileMeta) []Extractor {
	var out []Extractor
	for _, e := range r.extractors {
		if e.Match(f) {
			out = append(out, e)
		}
	}
	return out
}

// Names lists the registered extractors, sorted.
func (r *Registry) Names() []string {
	out := make([]string, len(r.extractors))
	for i, e := range r.extractors {
		out[i] = e.Name()
	}
	return out
}

// Len reports how many extractors are registered.
func (r *Registry) Len() int { return len(r.extractors) }
