package mcpserver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// view indexes a graph for the lookups the tools perform.
type view struct {
	graph schema.Graph

	byID     map[string]*schema.Node
	outgoing map[string][]schema.Edge
	incoming map[string][]schema.Edge
	byName   map[string][]string
	byPath   map[string][]schema.Diagnostic
}

func newView(g schema.Graph) *view {
	v := &view{
		graph:    g,
		byID:     make(map[string]*schema.Node, len(g.Nodes)),
		outgoing: map[string][]schema.Edge{},
		incoming: map[string][]schema.Edge{},
		byName:   map[string][]string{},
		byPath:   map[string][]schema.Diagnostic{},
	}
	for _, d := range g.Diagnostics {
		if d.Path != "" {
			v.byPath[d.Path] = append(v.byPath[d.Path], d)
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		v.byID[n.ID] = n
		lower := strings.ToLower(n.Name)
		v.byName[lower] = append(v.byName[lower], n.ID)
	}
	for _, e := range g.Edges {
		v.outgoing[e.From] = append(v.outgoing[e.From], e)
		v.incoming[e.To] = append(v.incoming[e.To], e)
	}
	return v
}

// diagnosticsFor returns what the scan reported about the files a component
// is declared in.
//
// The global diagnostic list is where the reason for an absence lives, but a
// reader looking at one component has no way to find the entry that concerns
// it -- they would have to read all of them and correlate by filename. An
// empty dependency list with an unexplained reference in the same file means
// something quite different from an empty list with nothing reported, and
// that difference is the whole question when a component appears to stand
// alone.
//
// Attribution is by file, not by line, so a manifest holding twenty objects
// attributes its diagnostics to all twenty. Callers say so rather than
// implying the entry is about this component specifically.
func (v *view) diagnosticsFor(n *schema.Node) []schema.Diagnostic {
	seen := map[string]bool{}
	var out []schema.Diagnostic
	for _, src := range n.Sources {
		for _, d := range v.byPath[src.Path] {
			key := fmt.Sprintf("%s:%d:%s", d.Path, d.Line, d.Code)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// resolveRef accepts either a node ID or a name.
//
// A model will reach for the name, because that is what it saw in the
// overview and what a person would say. Requiring the full identifier would
// cost a round trip every time, and the whole point of the tool surface is to
// answer a question in as few calls as possible.
func (v *view) resolveRef(ref string) (nodeID string, candidates []string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil, fmt.Errorf("no node given")
	}
	if _, ok := v.byID[ref]; ok {
		return ref, nil, nil
	}

	matches := append([]string(nil), v.byName[strings.ToLower(ref)]...)

	// Every tool renders an ambiguous node as "name.namespace", so that form
	// has to be accepted as input. Printing an identifier and then refusing
	// it makes each edge in every response a dead end the caller has to
	// re-resolve by hand, and costs a round trip to learn the real id.
	if len(matches) == 0 {
		if name, namespace, ok := strings.Cut(ref, "."); ok {
			for _, id := range v.byName[strings.ToLower(name)] {
				if n := v.byID[id]; n != nil && strings.EqualFold(n.Namespace, namespace) {
					matches = append(matches, id)
				}
			}
		}
	}

	if len(matches) == 0 {
		// A partial name is the next most likely thing a model will send.
		for id, n := range v.byID {
			if strings.Contains(strings.ToLower(n.Name), strings.ToLower(ref)) {
				matches = append(matches, id)
			}
		}
	}
	sort.Strings(matches)

	switch len(matches) {
	case 0:
		return "", nil, fmt.Errorf("no component matches %q", ref)
	case 1:
		return matches[0], nil, nil
	default:
		return "", matches, fmt.Errorf("%q matches %d components", ref, len(matches))
	}
}

// label renders a node for a human and a model to read: the name they asked
// about, qualified when the name alone is ambiguous.
func (v *view) label(nodeID string) string {
	n, ok := v.byID[nodeID]
	if !ok {
		return nodeID
	}
	if len(v.byName[strings.ToLower(n.Name)]) > 1 && n.Namespace != "" {
		return n.Name + "." + n.Namespace
	}
	return n.Name
}

// degree reports how connected a node is, ignoring containment, which is
// structure rather than dependency.
func (v *view) degree(nodeID string) (out, in int) {
	for _, e := range v.outgoing[nodeID] {
		if e.Kind != schema.EdgeContains {
			out++
		}
	}
	for _, e := range v.incoming[nodeID] {
		if e.Kind != schema.EdgeContains {
			in++
		}
	}
	return out, in
}

// nodeFilter narrows a node list.
type nodeFilter struct {
	kind      string
	layer     string
	namespace string
	query     string
}

func (f nodeFilter) matches(n schema.Node) bool {
	if f.kind != "" && !strings.EqualFold(string(n.Kind), f.kind) {
		return false
	}
	if f.layer != "" && !strings.EqualFold(string(n.Layer), f.layer) {
		return false
	}
	if f.namespace != "" && !strings.EqualFold(n.Namespace, f.namespace) {
		return false
	}
	if f.query != "" {
		q := strings.ToLower(f.query)
		if !strings.Contains(strings.ToLower(n.Name), q) &&
			!strings.Contains(strings.ToLower(n.ID), q) &&
			!strings.Contains(strings.ToLower(techString(n.Tech)), q) {
			return false
		}
	}
	return true
}

// maxPathDepth bounds how long a route may be before it stops explaining
// anything. Nothing in a dependency graph is usefully described by a
// twelve-hop chain.
const maxPathDepth = 8

// pathSearchBudget bounds the total work of an enumeration.
//
// Counting every simple path between two nodes is exponential in a dense
// graph. The budget keeps a pathological input from hanging a tool call, and
// exhausting it is reported rather than hidden, because a truncated search
// that claims completeness is the bug this function was rewritten to fix.
const pathSearchBudget = 200_000

// paths enumerates the distinct routes from one node to another, shortest
// first.
//
// This deliberately finds every simple path within the depth cap, not just
// the shortest ones. The tool built on it answers blast-radius questions,
// where a missed route is the whole failure: a breadth-first search that
// stops at the first time it reaches a node will report that A reaches B one
// way, when in fact a second service also sits on a route between them. The
// earlier implementation did exactly that and stated "1 path" as a fact.
//
// exhausted reports that the search hit its budget, so the caller can say the
// list may be incomplete instead of implying it is not.
func (v *view) paths(from, to string, maxPaths int) (found [][]schema.Edge, exhausted bool) {
	if from == to {
		return nil, false
	}

	// Depth-first with a path-local visited set: a node already on the
	// current path is skipped, so routes stay simple, but a node visited on
	// some other path stays available to this one.
	var (
		path   []schema.Edge
		onPath = map[string]bool{from: true}
		steps  int
		walk   func(node string) bool
	)

	walk = func(node string) bool {
		if len(path) >= maxPathDepth {
			return false
		}
		edges := append([]schema.Edge(nil), v.outgoing[node]...)
		sort.SliceStable(edges, func(i, j int) bool {
			if edges[i].To != edges[j].To {
				return edges[i].To < edges[j].To
			}
			return edges[i].Kind < edges[j].Kind
		})

		for _, e := range edges {
			if e.Kind == schema.EdgeContains {
				continue
			}
			steps++
			if steps > pathSearchBudget {
				return true // budget exhausted; stop everything
			}
			if e.To == to {
				found = append(found, append(append([]schema.Edge(nil), path...), e))
				continue
			}
			if onPath[e.To] {
				continue
			}
			onPath[e.To] = true
			path = append(path, e)
			stop := walk(e.To)
			path = path[:len(path)-1]
			onPath[e.To] = false
			if stop {
				return true
			}
		}
		return false
	}

	exhausted = walk(from)

	// Shortest first, then by weakest link descending, so the most direct and
	// best-evidenced route is the one a reader sees first.
	sort.SliceStable(found, func(i, j int) bool {
		if len(found[i]) != len(found[j]) {
			return len(found[i]) < len(found[j])
		}
		wi, wj := weakestLink(found[i]), weakestLink(found[j])
		if wi != wj {
			return wi > wj
		}
		return pathKey(found[i]) < pathKey(found[j])
	})

	// total is reported before truncation so the caller can say how many were
	// withheld rather than silently dropping them.
	if maxPaths > 0 && len(found) > maxPaths {
		return found[:maxPaths], true
	}
	return found, exhausted
}

// weakestLink is a path's least confident edge, which is as much as the whole
// chain can be trusted.
func weakestLink(path []schema.Edge) float64 {
	weakest := 1.0
	for _, e := range path {
		if e.Confidence < weakest {
			weakest = e.Confidence
		}
	}
	return weakest
}

// pathKey renders a path for deterministic ordering.
func pathKey(path []schema.Edge) string {
	var b strings.Builder
	for _, e := range path {
		b.WriteString(e.To)
		b.WriteByte('\x00')
	}
	return b.String()
}

func techString(t *schema.Tech) string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, s := range []string{t.Language, t.Framework, t.Runtime} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "/")
}

// attrSummary renders a node's attributes compactly, most useful first.
//
// Order is fixed rather than alphabetical: an image and a port say more about
// a component than a dependency count, and a model reading a truncated line
// should get the informative half.
func attrSummary(attrs schema.Attrs, limit int) string {
	if len(attrs) == 0 {
		return ""
	}
	priority := []string{
		"image", "ports", "replicas", "resourceType", "workload", "engine",
		"language", "module", "package", "schedule", "hosts", "multiplicity",
		"managed", "external", "usesTechnology",
	}

	var parts []string
	seen := map[string]bool{}
	for _, key := range priority {
		if v, ok := attrs[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, v))
			seen[key] = true
			if len(parts) >= limit {
				return strings.Join(parts, " ")
			}
		}
	}

	rest := make([]string, 0, len(attrs))
	for k := range attrs {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if len(parts) >= limit {
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, attrs[k]))
	}
	return strings.Join(parts, " ")
}
