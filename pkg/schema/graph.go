// Package schema defines the Structura Architecture Graph (SAG): the node,
// edge, and diagnostic types that `structura scan` emits and that the MCP
// server, the UI, and the cloud tier consume.
//
// This package is the project's public API. Breaking changes to these types
// are breaking changes to SchemaVersion, which is versioned independently of
// the CLI binary — a CLI bugfix must not imply a schema change, and the cloud
// tier has to ingest graphs produced by many CLI versions at once.
//
// Two invariants hold across the whole package:
//
//   - Every path is repo-relative and slash-separated, on every platform.
//     Absolute paths would leak the author's home directory into a file that
//     gets committed and pasted into LLM context, and would make graphs from
//     two machines undiffable.
//
//   - Serialization is deterministic. Two scans of an unchanged tree produce
//     byte-identical output. Golden tests, the scan cache, and drift
//     detection all depend on it.
package schema

import "time"

// Version is the schema version this package implements. It follows SemVer
// independently of the CLI version.
const Version = "0.1.0"

// Graph is a complete architecture snapshot of one repository.
type Graph struct {
	SchemaVersion string `json:"schemaVersion"`

	// GeneratedAt and Stats.DurationMs are wall-clock values and therefore
	// vary between otherwise identical scans. Canonical strips them; see
	// Graph.Canonical.
	GeneratedAt time.Time `json:"generatedAt"`

	// ContentHash is a SHA-256 over the canonical serialization, excluding
	// the volatile fields and this field itself. Equal hashes mean two scans
	// found the same architecture, which is what drift detection compares.
	ContentHash string `json:"contentHash"`

	Root        Root         `json:"root"`
	Nodes       []Node       `json:"nodes"`
	Edges       []Edge       `json:"edges"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Stats       Stats        `json:"stats"`
}

// Root identifies the scanned repository. It deliberately carries no absolute
// path: only the directory's base name and, when available, its VCS state.
type Root struct {
	Name string `json:"name"`
	VCS  *VCS   `json:"vcs,omitempty"`
}

// VCS records the commit a graph was generated from, so a stored graph can be
// tied back to the tree that produced it.
type VCS struct {
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Dirty is a pointer because "we could not tell" is a real answer:
	// commit and branch are readable straight out of .git, but detecting
	// uncommitted changes needs git itself, which may not be installed.
	// Reporting false in that case would be a claim we cannot support.
	Dirty *bool `json:"dirty,omitempty"`
}

// Node is one element of the architecture: a service, a datastore, a queue, a
// third-party system, a cloud resource, a package, or a grouping boundary.
type Node struct {
	// ID is semantic, not a content hash: "kind:source/namespace/name".
	// A content hash would change when a file moves, turning every drift
	// diff into a false positive, and IDs appear verbatim in LLM answers, so
	// they need to stay readable.
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`

	// Layer maps onto C4. Phase 1 emits only context and container; the
	// field exists now so Phase 2 can add component without a schema break.
	Layer Layer `json:"layer"`

	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`

	Tech    *Tech    `json:"tech,omitempty"`
	Attrs   Attrs    `json:"attrs,omitempty"`
	Sources []Source `json:"sources,omitempty"`

	Confidence float64 `json:"confidence"`
}

// Tech describes what a node is built with, when that is known.
type Tech struct {
	Language  string `json:"language,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
	Framework string `json:"framework,omitempty"`
}

// Attrs holds extractor-specific detail. Values must be JSON primitives,
// slices, or maps thereof. Slice order is significant and is the producer's
// responsibility to make deterministic; map keys are sorted on marshal.
type Attrs map[string]any

// Source records a file that contributed to a node's existence.
type Source struct {
	Extractor string `json:"extractor"`
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
}

// Edge is an inferred or declared relationship between two nodes.
//
// Confidence and Evidence are not optional extras. Configuration files rarely
// state what calls what, so most edges are inferred by matching names across
// artifacts. Shipping imperfect inference is acceptable; shipping imperfect
// inference that presents itself as certain is not, because a model will
// reason on top of it as fact.
type Edge struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     EdgeKind `json:"kind"`
	Protocol string   `json:"protocol,omitempty"`

	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence,omitempty"`
}

// Evidence explains why an edge exists: which rule fired, where, and on what.
// It is what lets the UI answer "why is this arrow here?" and what keeps the
// LLM from treating a guess as a fact.
type Evidence struct {
	Extractor string `json:"extractor"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Rule      string `json:"rule"`
	Detail    string `json:"detail,omitempty"`
}

// Diagnostic reports something the scan could not do: an unrendered Helm
// chart, an unresolvable Terraform reference, an ambiguous name. Gaps are
// reported explicitly rather than guessed at.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

// Stats summarizes the scan run.
type Stats struct {
	FilesScanned int `json:"filesScanned"`
	FilesParsed  int `json:"filesParsed"`
	NodeCount    int `json:"nodeCount"`
	EdgeCount    int `json:"edgeCount"`

	// DurationMs is wall-clock and therefore volatile; Canonical zeroes it.
	DurationMs int64 `json:"durationMs"`
}
