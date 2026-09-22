package schema_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Now that only the layer is in a node's identifier, two readings that
// disagree about the kind arrive at one node and the builder has to choose.
// Before 1.0.0 they had different identifiers and never met, which is why this
// case belongs here rather than in the resolver: by the time the resolver runs,
// the choice has been made.

func node(kind schema.NodeKind, scope, name string, attrs schema.Attrs) schema.Node {
	return schema.Node{
		ID: schema.NewNodeID(kind, scope, name), Kind: kind,
		Layer: schema.LayerOf(kind), Name: name, Namespace: scope,
		Attrs: attrs, Confidence: 1,
	}
}

func built(t *testing.T, nodes ...schema.Node) schema.Graph {
	t.Helper()
	b := schema.NewBuilder()
	for _, n := range nodes {
		b.AddNode(n)
	}
	g, err := b.Build(schema.Root{Name: "test"}, schema.Stats{})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return g
}

// The case this exists for. docker-compose.yml gives carts-db an image of
// mongo and it becomes a datastore; docker-compose.logging.yml mentions the
// same service to attach a log driver, names no image, and gets the fallback.
// One Compose project cannot hold two services called carts-db.
func TestOneNameWithTwoKindsBecomesOneNode(t *testing.T) {
	g := built(t,
		node(schema.KindDatastore, "shop", "carts-db", schema.Attrs{"image": "mongo:3.4"}),
		node(schema.KindService, "shop", "carts-db", nil),
	)

	if len(g.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(g.Nodes), g.Nodes)
	}
	// The reading that named an image is where the kind came from; the other
	// was not classified, it was defaulted.
	if g.Nodes[0].Kind != schema.KindDatastore {
		t.Errorf("Kind = %q, want datastore", g.Nodes[0].Kind)
	}
	if g.Nodes[0].Layer != schema.LayerContainer {
		t.Errorf("Layer = %q, want container", g.Nodes[0].Layer)
	}
}

// Which reading wins cannot depend on the order they arrived in. Sorted path
// order would have handed carts-db to docker-compose.logging.yml.
func TestTheClassifiedKindWinsWhicheverOrderTheyArrive(t *testing.T) {
	classified := node(schema.KindQueue, "shop", "queue", schema.Attrs{"image": "rabbitmq:3"})
	defaulted := node(schema.KindService, "shop", "queue", nil)

	for _, order := range [][]schema.Node{
		{classified, defaulted},
		{defaulted, classified},
	} {
		g := built(t, order...)
		if len(g.Nodes) != 1 {
			t.Fatalf("nodes = %d, want 1", len(g.Nodes))
		}
		if g.Nodes[0].Kind != schema.KindQueue {
			t.Errorf("Kind = %q, want queue; arrival order decided the answer",
				g.Nodes[0].Kind)
		}
	}
}

// A disagreement still has to be reported. It is usually harmless, but it is
// the only signal that two files describe one thing differently.
func TestAKindDisagreementIsReported(t *testing.T) {
	g := built(t,
		node(schema.KindDatastore, "shop", "cache", schema.Attrs{"image": "redis:7"}),
		node(schema.KindService, "shop", "cache", nil),
	)

	var found bool
	for _, d := range g.Diagnostics {
		if d.Code == "node_kind_conflict" {
			found = true
			if !strings.Contains(d.Message, "datastore") || !strings.Contains(d.Message, "service") {
				t.Errorf("the diagnostic does not name both readings: %s", d.Message)
			}
		}
	}
	if !found {
		t.Errorf("a kind disagreement was resolved silently: %+v", g.Diagnostics)
	}
}

// Neither reading named an image, so the specific kind wins: service is what
// a component is called when nothing said otherwise.
func TestASpecificKindBeatsTheFallback(t *testing.T) {
	g := built(t,
		node(schema.KindService, "shop", "broker", nil),
		node(schema.KindQueue, "shop", "broker", nil),
	)
	if len(g.Nodes) != 1 || g.Nodes[0].Kind != schema.KindQueue {
		t.Errorf("nodes = %+v, want one queue", g.Nodes)
	}
}

// Two readings that disagree about the layer describe a container and the
// thing that contains it, and must never merge: a chart called podinfo deploys
// a workload called podinfo.
func TestReadingsInDifferentLayersNeverMerge(t *testing.T) {
	g := built(t,
		node(schema.KindBoundary, "", "podinfo", nil),
		node(schema.KindService, "", "podinfo", nil),
	)
	if len(g.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2: %+v", len(g.Nodes), g.Nodes)
	}
	for _, d := range g.Diagnostics {
		if d.Code == "node_kind_conflict" {
			t.Errorf("a boundary and a workload were reported as a kind conflict: %s", d.Message)
		}
	}
}

// Two files describing one service in one scope are one node whatever found
// them. This is the property the extractor segment used to prevent.
func TestTwoFormatsInOneScopeAreOneNode(t *testing.T) {
	g := built(t,
		node(schema.KindService, "prod", "api", schema.Attrs{"replicas": 3}),
		node(schema.KindService, "prod", "api", schema.Attrs{"image": "api:1"}),
	)
	if len(g.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(g.Nodes), g.Nodes)
	}
	n := g.Nodes[0]
	if n.Attrs["replicas"] == nil || n.Attrs["image"] == nil {
		t.Errorf("attributes from both readings did not survive: %+v", n.Attrs)
	}
}
