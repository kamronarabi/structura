// Package schema defines the Structura Architecture Graph (SAG): the node,
// edge, and diagnostic types that `structura scan` emits and that the MCP
// server, the UI, and the cloud tier consume.
//
// This package is the project's public API. Breaking changes to these types
// are breaking changes to SchemaVersion. What counts as a break, what a
// version bump promises, and how a build should treat a graph written by a
// different one are all defined in compat.go, alongside Version itself.
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

	// Generator records what wrote this graph. SchemaVersion says what the
	// format is; this says what produced it, and they are different
	// questions. Two builds of the same CLI emit the same schemaVersion and
	// can still disagree about the same repository, because an extractor
	// changed between them -- so a reader with no way to tell them apart
	// serves a stored graph that the code has already superseded.
	//
	// Cleared by Canonical: it is provenance, not architecture, and two
	// builds that find the same thing should agree on the content hash.
	Generator *Generator `json:"generator,omitempty"`

	Root        Root         `json:"root"`
	Nodes       []Node       `json:"nodes"`
	Edges       []Edge       `json:"edges"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Stats       Stats        `json:"stats"`
}

// Generator names the tool that produced a graph.
//
// Version is opaque and compared only for equality: it identifies a build,
// and what makes one build distinguishable from another is the producer's
// business, not this package's.
type Generator struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
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
	// ID is semantic, not a content hash: "layer:name" or "layer:@scope/name".
	// A content hash would change when a file moves, turning every drift
	// diff into a false positive, and IDs appear verbatim in LLM answers, so
	// they need to stay readable. It is derived from the identity fields
	// below; see ids.go for why only those are in it.
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`

	// Layer maps onto C4. Phase 1 emits only context and container; the
	// field exists now so Phase 2 can add component without a schema break.
	Layer Layer `json:"layer"`

	Name string `json:"name"`

	// Namespace is the deployment scope the component lives in: a Kubernetes
	// namespace, and nothing else. It is empty for the many components whose
	// repository declares no scope for them.
	//
	// It once held whatever each extractor had to hand -- a chart name, a
	// Compose project, a Terraform module directory, a language -- because it
	// was the identifier's only disambiguating segment. Those are recorded
	// where they belong now, and this means one thing.
	Namespace string `json:"namespace,omitempty"`

	// Project is the declared project this component belongs to, empty when
	// the repository is a single project, which is the usual case. It is part
	// of identity, which is why it is a field and not an attribute: two
	// projects each declaring a "prod" namespace are not one namespace.
	Project string `json:"project,omitempty"`

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

	// Connected counts components taking part in at least one relationship,
	// excluding boundaries and containment. Everything else is a component
	// this scan found and could say nothing further about.
	Connected int `json:"connected"`

	// Clusters counts the independent groups the graph falls into, over both
	// relationships and containment.
	//
	// It is what separates a system from a collection. A repository of two
	// hundred unrelated charts yields two hundred clusters and is not an
	// architecture; a service mesh yields one. A reader given "417
	// components" without this number will take the first for the second,
	// and a canvas will draw four hundred disconnected dots as though they
	// were a diagram.
	Clusters int `json:"clusters"`

	// DurationMs is wall-clock and therefore volatile; Canonical zeroes it.
	DurationMs int64 `json:"durationMs"`
}

// Cohesion measures how much of this graph hangs together, between 0 and 1.
//
// It is the share of non-boundary components that take part in at least one
// relationship. Low cohesion is not a defect in the repository — a chart
// library genuinely has no internal architecture — but it does mean the graph
// should be presented as an inventory rather than as a picture of a system.
func (g Graph) Cohesion() float64 {
	var components int
	for _, n := range g.Nodes {
		if n.Kind != KindBoundary {
			components++
		}
	}
	if components == 0 {
		return 0
	}
	return float64(g.Stats.Connected) / float64(components)
}

// Measure fills in the derived counts on Stats. Normalize calls it.
func (g *Graph) Measure() {
	g.Stats.NodeCount = len(g.Nodes)
	g.Stats.EdgeCount = len(g.Edges)

	connected := map[string]bool{}
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for _, n := range g.Nodes {
		find(n.ID)
	}
	for _, e := range g.Edges {
		// Containment joins a cluster without making its members related:
		// two charts in one repository are separate systems even though the
		// repository holds both.
		if e.Kind != EdgeContains {
			connected[e.From] = true
			connected[e.To] = true
		}
		union(e.From, e.To)
	}

	g.Stats.Connected = 0
	for _, n := range g.Nodes {
		if n.Kind != KindBoundary && connected[n.ID] {
			g.Stats.Connected++
		}
	}

	roots := map[string]bool{}
	for _, n := range g.Nodes {
		roots[find(n.ID)] = true
	}
	g.Stats.Clusters = len(roots)
}
