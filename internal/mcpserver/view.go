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
}

func newView(g schema.Graph) *view {
	v := &view{
		graph:    g,
		byID:     make(map[string]*schema.Node, len(g.Nodes)),
		outgoing: map[string][]schema.Edge{},
		incoming: map[string][]schema.Edge{},
		byName:   map[string][]string{},
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

// paths finds the shortest routes between two nodes.
//
// Breadth-first from the source, so every path returned is of minimal length.
// Longer alternatives are deliberately not explored: a model asking how A
// reaches B wants the dependency chain, and enumerating every walk through a
// connected graph would blow the budget on paths nobody would draw.
func (v *view) paths(from, to string, maxPaths int) [][]schema.Edge {
	if from == to {
		return nil
	}

	type state struct {
		node string
		path []schema.Edge
	}
	queue := []state{{node: from}}
	visited := map[string]bool{from: true}

	var found [][]schema.Edge
	depth := 0
	for len(queue) > 0 && len(found) < maxPaths {
		// Bound the search: beyond a handful of hops a "path" stops being an
		// explanation of anything.
		depth++
		if depth > 32 {
			break
		}
		var next []state
		levelVisited := map[string]bool{}

		for _, s := range queue {
			edges := append([]schema.Edge(nil), v.outgoing[s.node]...)
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
				path := append(append([]schema.Edge(nil), s.path...), e)
				if e.To == to {
					found = append(found, path)
					if len(found) >= maxPaths {
						break
					}
					continue
				}
				if visited[e.To] {
					continue
				}
				levelVisited[e.To] = true
				next = append(next, state{node: e.To, path: path})
			}
			if len(found) >= maxPaths {
				break
			}
		}
		// Marking visited a level at a time rather than on enqueue lets two
		// distinct shortest paths through different neighbours both surface.
		for node := range levelVisited {
			visited[node] = true
		}
		queue = next
	}
	return found
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
