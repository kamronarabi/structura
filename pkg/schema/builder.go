package schema

import (
	"errors"
	"fmt"
	"sort"
)

// Builder accumulates nodes, edges, and diagnostics from many extractors and
// merges them into one graph.
//
// The same element is routinely discovered more than once — a Compose file
// and a Kubernetes manifest can both describe "postgres" — so merge, not
// append, is the default. Merging is order-independent by construction: the
// result depends only on the set of contributions, never on the order the
// pipeline happened to deliver them in.
//
// Builder is not safe for concurrent use. The pipeline extracts in parallel
// into per-worker buffers and merges single-threaded, which is what keeps the
// output deterministic.
type Builder struct {
	nodes map[string]*Node
	edges map[string]*Edge
	diags []Diagnostic
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{
		nodes: make(map[string]*Node),
		edges: make(map[string]*Edge),
	}
}

// AddNode merges a node into the graph. When a node with the same ID already
// exists, sources are unioned, the higher confidence wins, and attributes
// from the more confident contributor take precedence.
func (b *Builder) AddNode(n Node) {
	existing, ok := b.nodes[n.ID]
	if !ok {
		cp := n
		cp.Attrs = cloneAttrs(n.Attrs)
		cp.Sources = append([]Source(nil), n.Sources...)
		b.nodes[n.ID] = &cp
		return
	}

	// A kind disagreement means two files read the same thing as different
	// types of component, and one of the two readings is usually just
	// uninformed: a Compose file that names an image of mongo produces a
	// datastore, while a sibling file mentioning the same service to attach a
	// log driver names no image and produces the service fallback.
	//
	// So the better-informed reading wins rather than the first one. Taking
	// the first was safe while the kind was part of the identifier, because
	// the two never met; now they merge, and sorted path order would have
	// handed carts-db to docker-compose.logging.yml.
	//
	// Swapping the kind cannot invalidate the identifier, because only the
	// layer is in it and every kind that maps to one layer produces the same
	// identifier. Two readings that disagree about the layer never merge.
	if existing.Kind != n.Kind {
		was, kept := existing.Kind, existing.Kind
		if betterClassified(&n, existing) {
			kept = n.Kind
			existing.Kind = n.Kind
			existing.Layer = LayerOf(n.Kind)
		}
		b.Diag(Diagnostic{
			Severity: SeverityInfo,
			Code:     "node_kind_conflict",
			Path:     firstPath(n.Sources),
			Message: fmt.Sprintf("%s was read as %q and also as %q; keeping %q, "+
				"which is the reading that identified what it runs",
				n.ID, was, n.Kind, kept),
		})
	}

	// Strictly greater, so an equally confident reading does not displace
	// the one already here: the first contribution wins every field below.
	//
	// That makes the merged node depend on the order AddNode is called in,
	// which is only safe because the caller fixes that order. The pipeline
	// merges per-file outputs in the walker's sorted path order rather than
	// in the order its workers finished, so "first" means "from the file
	// whose path sorts first" and not "whichever goroutine won the race".
	// A caller that folds contributions in a different order will get a
	// different node, and a caller that folds them in no fixed order will
	// get a different graph on every run.
	incomingWins := n.Confidence > existing.Confidence
	if incomingWins {
		existing.Confidence = n.Confidence
	}
	// A declared name beats a derived one. A go.mod states a module; a
	// Dockerfile can only be named after the directory it sits in and records
	// that in nameFrom. Both now produce one identifier -- they are keyed on
	// the same directory -- so without this the winner is whichever file's
	// path sorts first, and "Dockerfile" sorts before "go.mod".
	switch {
	case existing.Name == "":
		existing.Name = n.Name
		delete(existing.Attrs, nameFromAttr)
	case existing.Attrs[nameFromAttr] != nil && n.Attrs[nameFromAttr] == nil && n.Name != "":
		existing.Name = n.Name
		delete(existing.Attrs, nameFromAttr)
	}
	if existing.Namespace == "" {
		existing.Namespace = n.Namespace
	}
	existing.Tech = mergeTech(existing.Tech, n.Tech, incomingWins)
	existing.Attrs = mergeAttrs(existing.Attrs, n.Attrs, incomingWins)
	existing.Sources = append(existing.Sources, n.Sources...)
}

// AddEdge merges an edge. A missing ID is derived from the endpoints. When
// the same relationship is found twice, the evidence accumulates and the
// highest confidence wins — two independent rules agreeing is not a reason to
// downgrade.
func (b *Builder) AddEdge(e Edge) {
	if e.ID == "" {
		e.ID = NewEdgeID(e.From, e.To, e.Kind, e.Protocol)
	}
	existing, ok := b.edges[e.ID]
	if !ok {
		cp := e
		cp.Evidence = append([]Evidence(nil), e.Evidence...)
		b.edges[e.ID] = &cp
		return
	}
	if e.Confidence > existing.Confidence {
		existing.Confidence = e.Confidence
	}
	existing.Evidence = append(existing.Evidence, e.Evidence...)
}

// Diag records a diagnostic.
func (b *Builder) Diag(d Diagnostic) { b.diags = append(b.diags, d) }

// NodeIDs returns every accumulated node ID in sorted order.
func (b *Builder) NodeIDs() []string {
	ids := make([]string, 0, len(b.nodes))
	for id := range b.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Nodes returns the merged node set and any diagnostics merging produced,
// without finalizing the graph.
//
// The resolver needs the complete, deduplicated node set to build its
// identity index, and it may then rewrite node IDs — so this has to be
// available before Build runs, not after.
func (b *Builder) Nodes() (nodes []Node, diagnostics []Diagnostic) {
	ids := b.NodeIDs()
	nodes = make([]Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, *b.nodes[id])
	}
	return nodes, append([]Diagnostic(nil), b.diags...)
}

// Build validates and normalizes the accumulated graph.
//
// An edge whose endpoint is missing is treated as a programming error rather
// than a user-input problem: it means an extractor or the resolver referenced
// a node it never added. Returning it as an error surfaces the bug at the
// seam instead of shipping a graph with arrows pointing at nothing.
func (b *Builder) Build(root Root, stats Stats) (Graph, error) {
	g := Graph{
		SchemaVersion: Version,
		Root:          root,
		Stats:         stats,
		Nodes:         make([]Node, 0, len(b.nodes)),
		Edges:         make([]Edge, 0, len(b.edges)),
		Diagnostics:   append([]Diagnostic(nil), b.diags...),
	}
	for _, id := range b.NodeIDs() {
		g.Nodes = append(g.Nodes, *b.nodes[id])
	}
	edgeIDs := make([]string, 0, len(b.edges))
	for id := range b.edges {
		edgeIDs = append(edgeIDs, id)
	}
	sort.Strings(edgeIDs)
	for _, id := range edgeIDs {
		g.Edges = append(g.Edges, *b.edges[id])
	}

	var problems []error
	for _, n := range g.Nodes {
		if err := n.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	for _, e := range g.Edges {
		if err := e.Validate(); err != nil {
			problems = append(problems, err)
			continue
		}
		if _, ok := b.nodes[e.From]; !ok {
			problems = append(problems, fmt.Errorf("edge %s: from-node %s does not exist", e.ID, e.From))
		}
		if _, ok := b.nodes[e.To]; !ok {
			problems = append(problems, fmt.Errorf("edge %s: to-node %s does not exist", e.ID, e.To))
		}
	}
	for _, d := range g.Diagnostics {
		if err := d.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	if len(problems) > 0 {
		return Graph{}, fmt.Errorf("graph is invalid: %w", errors.Join(problems...))
	}

	g.Normalize()
	return g, nil
}

func mergeTech(existing, incoming *Tech, incomingWins bool) *Tech {
	if incoming == nil {
		return existing
	}
	if existing == nil {
		cp := *incoming
		return &cp
	}
	merged := *existing
	fill := func(dst *string, src string) {
		if src != "" && (*dst == "" || incomingWins) {
			*dst = src
		}
	}
	fill(&merged.Language, incoming.Language)
	fill(&merged.Runtime, incoming.Runtime)
	fill(&merged.Framework, incoming.Framework)
	return &merged
}

func mergeAttrs(existing, incoming Attrs, incomingWins bool) Attrs {
	if len(incoming) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(Attrs, len(incoming))
	}
	for k, v := range incoming {
		if _, present := existing[k]; !present || incomingWins {
			existing[k] = v
		}
	}
	return existing
}

func cloneAttrs(a Attrs) Attrs {
	if a == nil {
		return nil
	}
	out := make(Attrs, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

func firstPath(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}
	return sources[0].Path
}

// nameFromAttr marks a node whose name was derived from its surroundings
// rather than declared by the file that described it. The extractor that can
// only name a component after its directory says so here, and merge uses it to
// prefer a name somebody actually wrote.
const nameFromAttr = "nameFrom"

// betterClassified reports whether candidate's kind was established rather
// than defaulted.
//
// A kind comes from recognizing an image, so the node that named one is the
// node that knows. Where neither did, the more specific kind wins, since
// service is what a component is called when nothing said otherwise.
func betterClassified(candidate, current *Node) bool {
	if hasImage(candidate) != hasImage(current) {
		return hasImage(candidate)
	}
	return current.Kind == KindService && candidate.Kind != KindService
}

func hasImage(n *Node) bool {
	image, _ := n.Attrs["image"].(string)
	return image != ""
}
