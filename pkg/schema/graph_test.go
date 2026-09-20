package schema_test

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/kamronarabi/structura/pkg/schema"
)

// contribution is one extractor's output, used to prove that merging is
// independent of the order the pipeline delivers work in.
type contribution struct {
	nodes []schema.Node
	edges []schema.Edge
	diags []schema.Diagnostic
}

func sampleContributions() []contribution {
	api := schema.NewNodeID(schema.KindService, "k8s", "prod", "api-gateway")
	db := schema.NewNodeID(schema.KindDatastore, "k8s", "prod", "postgres")
	cache := schema.NewNodeID(schema.KindDatastore, "compose", "default", "redis")
	stripe := schema.NewNodeID(schema.KindExternal, "resolver", "net", "api.stripe.com")

	return []contribution{
		{
			nodes: []schema.Node{{
				ID: api, Kind: schema.KindService, Layer: schema.LayerContainer,
				Name: "api-gateway", Namespace: "prod",
				Tech:       &schema.Tech{Language: "go", Runtime: "container"},
				Attrs:      schema.Attrs{"replicas": 3, "ports": []int{8080}},
				Sources:    []schema.Source{{Extractor: "k8s", Path: "deploy/prod/api.yaml", Line: 12}},
				Confidence: schema.ConfDeclared,
			}},
		},
		{
			nodes: []schema.Node{{
				ID: db, Kind: schema.KindDatastore, Layer: schema.LayerContainer,
				Name: "postgres", Namespace: "prod",
				Attrs:      schema.Attrs{"image": "postgres:16", "ports": []int{5432}},
				Sources:    []schema.Source{{Extractor: "k8s", Path: "deploy/prod/db.yaml", Line: 4}},
				Confidence: schema.ConfDeclared,
			}},
			edges: []schema.Edge{{
				From: api, To: db, Kind: schema.EdgePersistsTo, Protocol: "postgres",
				Confidence: schema.ConfHostMatch,
				Evidence: []schema.Evidence{{
					Extractor: "resolver", Path: "deploy/prod/api.yaml", Line: 30,
					Rule:   "env_host_match",
					Detail: "env DATABASE_URL host 'postgres' matches Service postgres.prod",
				}},
			}},
		},
		{
			nodes: []schema.Node{
				{
					ID: cache, Kind: schema.KindDatastore, Layer: schema.LayerContainer,
					Name: "redis", Attrs: schema.Attrs{"image": "redis:7"},
					Sources:    []schema.Source{{Extractor: "compose", Path: "docker-compose.yml", Line: 22}},
					Confidence: schema.ConfDeclared,
				},
				{
					ID: stripe, Kind: schema.KindExternal, Layer: schema.LayerContext,
					Name: "api.stripe.com", Confidence: schema.ConfHostMatch,
				},
			},
			edges: []schema.Edge{{
				From: api, To: stripe, Kind: schema.EdgeCalls, Protocol: "https",
				Confidence: schema.ConfHostMatch,
				Evidence: []schema.Evidence{{
					Extractor: "resolver", Path: "deploy/prod/api.yaml", Line: 34,
					Rule: "env_url_external", Detail: "STRIPE_API=https://api.stripe.com",
				}},
			}},
			diags: []schema.Diagnostic{{
				Severity: schema.SeverityWarn, Code: "helm_unrendered",
				Path: "charts/api/templates", Message: "18 Helm templates not rendered",
			}},
		},
		{
			// The same service seen a second time, by a different extractor
			// and with lower confidence. It must merge, not duplicate.
			nodes: []schema.Node{{
				ID: api, Kind: schema.KindService, Layer: schema.LayerContainer,
				Name: "api-gateway",
				Attrs: schema.Attrs{
					"image":    "ghcr.io/acme/api:1.2.0",
					"replicas": 99, // loses: lower confidence than the k8s reading
				},
				Sources:    []schema.Source{{Extractor: "compose", Path: "docker-compose.yml", Line: 3}},
				Confidence: schema.ConfWeak,
			}},
		},
	}
}

func build(t *testing.T, contribs []contribution) schema.Graph {
	t.Helper()
	b := schema.NewBuilder()
	for _, c := range contribs {
		for _, n := range c.nodes {
			b.AddNode(n)
		}
		for _, e := range c.edges {
			b.AddEdge(e)
		}
		for _, d := range c.diags {
			b.Diag(d)
		}
	}
	g, err := b.Build(
		schema.Root{Name: "acme", VCS: &schema.VCS{Commit: "5bfd317", Branch: "main", Dirty: new(bool)}},
		schema.Stats{FilesScanned: 1247, FilesParsed: 38},
	)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return g
}

// The core M1 acceptance criterion.
func TestMarshalIsDeterministic(t *testing.T) {
	g := build(t, sampleContributions())

	first, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		again, err := schema.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("marshal %d differs from marshal 0", i)
		}
	}
}

// Go randomizes map iteration, and the pipeline extracts in parallel, so the
// merge order is not fixed. The output must be anyway.
func TestBuildIsOrderIndependent(t *testing.T) {
	want, err := schema.Marshal(build(t, sampleContributions()))
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	for trial := range 50 {
		contribs := sampleContributions()
		rng.Shuffle(len(contribs), func(i, j int) { contribs[i], contribs[j] = contribs[j], contribs[i] })

		got, err := schema.Marshal(build(t, contribs))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("trial %d produced a different graph for the same contributions", trial)
		}
	}
}

func TestRoundTripIsFixedPoint(t *testing.T) {
	g := build(t, sampleContributions())
	g.GeneratedAt = time.Date(2026, 9, 20, 14, 22, 31, 0, time.UTC)

	first, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := schema.Unmarshal(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := schema.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("load/save round trip changed the bytes:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestContentHashIgnoresVolatileFields(t *testing.T) {
	a := build(t, sampleContributions())
	a.GeneratedAt = time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC)
	a.Stats.DurationMs = 412
	a.Normalize()

	b := build(t, sampleContributions())
	b.GeneratedAt = time.Date(2030, 1, 1, 9, 30, 0, 0, time.UTC)
	b.Stats.DurationMs = 9999
	b.Normalize()

	if a.ContentHash == "" {
		t.Fatal("content hash is empty")
	}
	// Two scans of an unchanged tree differ only in wall-clock values, and
	// drift detection must not read that as an architecture change.
	if a.ContentHash != b.ContentHash {
		t.Errorf("content hash changed with the timestamp:\n %s\n %s", a.ContentHash, b.ContentHash)
	}
	if !strings.HasPrefix(a.ContentHash, "sha256:") {
		t.Errorf("content hash %q lacks its algorithm prefix", a.ContentHash)
	}
}

func TestContentHashTracksRealChanges(t *testing.T) {
	base := build(t, sampleContributions())

	changed := build(t, sampleContributions())
	changed.Nodes[0].Attrs["replicas"] = 7
	changed.Normalize()

	if base.ContentHash == changed.ContentHash {
		t.Error("content hash did not change when an attribute did")
	}
}

func TestMergeKeepsTheMoreConfidentReading(t *testing.T) {
	g := build(t, sampleContributions())

	var api *schema.Node
	for i := range g.Nodes {
		if strings.HasSuffix(g.Nodes[i].ID, "api-gateway") {
			api = &g.Nodes[i]
		}
	}
	if api == nil {
		t.Fatal("api-gateway node is missing")
	}

	// Two contributions described this node; it must appear once.
	count := 0
	for _, n := range g.Nodes {
		if n.ID == api.ID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("api-gateway appears %d times, want 1", count)
	}

	if api.Attrs["replicas"] != 3 {
		t.Errorf("replicas = %v, want 3 (the higher-confidence reading wins)", api.Attrs["replicas"])
	}
	// The lower-confidence contribution still supplies attributes the
	// higher-confidence one lacked.
	if api.Attrs["image"] != "ghcr.io/acme/api:1.2.0" {
		t.Errorf("image = %v, want the value only the compose reading had", api.Attrs["image"])
	}
	if len(api.Sources) != 2 {
		t.Errorf("sources = %d, want both contributors recorded", len(api.Sources))
	}
	if api.Confidence != schema.ConfDeclared {
		t.Errorf("confidence = %v, want the max of the two", api.Confidence)
	}
}

func TestBuildRejectsDanglingEdge(t *testing.T) {
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID: "service:k8s/prod/api", Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "api", Confidence: 1,
	})
	b.AddEdge(schema.Edge{
		From: "service:k8s/prod/api", To: "datastore:k8s/prod/ghost",
		Kind: schema.EdgePersistsTo, Confidence: 0.8,
		Evidence: []schema.Evidence{{Extractor: "resolver", Rule: "env_host_match"}},
	})

	_, err := b.Build(schema.Root{Name: "x"}, schema.Stats{})
	if err == nil {
		t.Fatal("Build() = nil, want an error for an edge pointing at a node that was never added")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to name the missing endpoint", err)
	}
}

func TestBuildRejectsEdgeWithoutEvidence(t *testing.T) {
	b := schema.NewBuilder()
	for _, id := range []string{"service:k8s/prod/api", "datastore:k8s/prod/db"} {
		kind := schema.KindService
		if strings.HasPrefix(id, "datastore") {
			kind = schema.KindDatastore
		}
		b.AddNode(schema.Node{ID: id, Kind: kind, Layer: schema.LayerContainer, Name: "x", Confidence: 1})
	}
	b.AddEdge(schema.Edge{
		From: "service:k8s/prod/api", To: "datastore:k8s/prod/db",
		Kind: schema.EdgePersistsTo, Confidence: 0.8,
	})

	// An edge with no evidence is an unsourced assertion, and the model
	// downstream cannot tell it apart from a declared fact.
	if _, err := b.Build(schema.Root{Name: "x"}, schema.Stats{}); err == nil {
		t.Fatal("Build() = nil, want an error for an edge with no evidence")
	}
}

func TestBuildRejectsAbsolutePaths(t *testing.T) {
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID: "service:k8s/prod/api", Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "api", Confidence: 1,
		Sources: []schema.Source{{Extractor: "k8s", Path: "/Users/someone/repo/deploy/api.yaml"}},
	})
	// Absolute paths leak the author's home directory into a file that gets
	// committed and pasted into LLM context, and make graphs from two
	// machines undiffable.
	if _, err := b.Build(schema.Root{Name: "x"}, schema.Stats{}); err == nil {
		t.Fatal("Build() = nil, want an error for an absolute source path")
	}
}

func TestBuildRejectsWindowsSeparators(t *testing.T) {
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID: "service:k8s/prod/api", Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "api", Confidence: 1,
		Sources: []schema.Source{{Extractor: "k8s", Path: `deploy\prod\api.yaml`}},
	})
	if _, err := b.Build(schema.Root{Name: "x"}, schema.Stats{}); err == nil {
		t.Fatal("Build() = nil, want an error for a backslash-separated path")
	}
}

func TestNodeKindConflictIsReported(t *testing.T) {
	id := schema.NewNodeID(schema.KindService, "k8s", "prod", "redis")
	b := schema.NewBuilder()
	b.AddNode(schema.Node{ID: id, Kind: schema.KindService, Layer: schema.LayerContainer, Name: "redis", Confidence: 1})
	b.AddNode(schema.Node{ID: id, Kind: schema.KindDatastore, Layer: schema.LayerContainer, Name: "redis", Confidence: 1})

	g, err := b.Build(schema.Root{Name: "x"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range g.Diagnostics {
		if d.Code == "node_kind_conflict" {
			found = true
		}
	}
	if !found {
		t.Error("two extractors disagreed on a node's kind and nothing was reported")
	}
}

func TestConfidenceIsRoundedAndClamped(t *testing.T) {
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID: "service:k8s/prod/api", Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "api", Confidence: 0.7000000000000001,
	})
	g, err := b.Build(schema.Root{Name: "x"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Nodes[0].Confidence != 0.7 {
		t.Errorf("confidence = %v, want it rounded to 0.7", g.Nodes[0].Confidence)
	}

	out, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("0.7000000000000001")) {
		t.Error("float noise reached the serialized output")
	}
}

func TestEmptyGraphSerializesAsEmptyArrays(t *testing.T) {
	b := schema.NewBuilder()
	g, err := b.Build(schema.Root{Name: "empty"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	// null would force every consumer into a nil check for no reason.
	if bytes.Contains(out, []byte("null")) {
		t.Errorf("empty collections serialized as null:\n%s", out)
	}
}

func TestMarshalDoesNotEscapeHTML(t *testing.T) {
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID: "external:resolver/net/example.com", Kind: schema.KindExternal, Layer: schema.LayerContext,
		Name: "example.com", Confidence: 0.8,
		Attrs: schema.Attrs{"url": "https://example.com/?a=1&b=2"},
	})
	g, err := b.Build(schema.Root{Name: "x"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	// Spelled byte-by-byte so the literal escape sequence survives any
	// tooling that rewrites backslash escapes in source.
	escapedAmp := []byte{'\\', 'u', '0', '0', '2', '6'}
	if bytes.Contains(out, escapedAmp) {
		t.Errorf("ampersand was escaped; the file is meant to be read by humans:\n%s", out)
	}
}
