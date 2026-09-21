package override_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/override"
	"github.com/kamronarabi/structura/pkg/schema"
)

func node(id string, kind schema.NodeKind, name, namespace string) schema.Node {
	return schema.Node{
		ID: id, Kind: kind, Name: name, Namespace: namespace,
		Layer: schema.LayerContainer, Confidence: 1,
		Sources: []schema.Source{{Extractor: "test", Path: "compose.yml", Line: 1}},
	}
}

func edge(from, to string, kind schema.EdgeKind, confidence float64, rule string) schema.Edge {
	return schema.Edge{
		From: from, To: to, Kind: kind, Confidence: confidence,
		Evidence: []schema.Evidence{{Extractor: "test", Rule: rule}},
	}
}

// fixture: web -> api (calls), api -> stripe (calls). No edge to cache.
func fixture() ([]schema.Node, []schema.Edge) {
	nodes := []schema.Node{
		node("service:compose/shop/web", schema.KindService, "web", "shop"),
		node("service:compose/shop/api", schema.KindService, "api", "shop"),
		node("datastore:compose/shop/cache", schema.KindDatastore, "cache", "shop"),
		node("external:resolver/net/api.stripe.com", schema.KindExternal, "api.stripe.com", "net"),
	}
	edges := []schema.Edge{
		edge("service:compose/shop/web", "service:compose/shop/api", schema.EdgeCalls, 0.8, "name_exact"),
		edge("service:compose/shop/api", "external:resolver/net/api.stripe.com", schema.EdgeCalls, 0.8, "external_host"),
	}
	return nodes, edges
}

func findEdge(t *testing.T, res override.Result, fromName, toName string) (schema.Edge, bool) {
	t.Helper()
	names := map[string]string{}
	for _, n := range res.Nodes {
		names[n.ID] = n.Name
	}
	for _, e := range res.Edges {
		if names[e.From] == fromName && names[e.To] == toName {
			return e, true
		}
	}
	return schema.Edge{}, false
}

func codes(diags []schema.Diagnostic) []string {
	out := make([]string, len(diags))
	for i, d := range diags {
		out[i] = d.Code
	}
	return out
}

// The case a configuration scan cannot reach by construction: a dependency
// that exists only in application code.
func TestDeclaredRelationshipIsAdded(t *testing.T) {
	nodes, edges := fixture()
	res, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{
			From: "api", To: "cache", Kind: "persists_to", Protocol: "redis",
			Note: "redis client built in api/internal/cache.go",
		}},
	})

	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", codes(diags))
	}
	e, ok := findEdge(t, res, "api", "cache")
	if !ok {
		t.Fatalf("the declared relationship was not added; edges: %+v", res.Edges)
	}
	if e.Confidence != schema.ConfDeclared {
		t.Errorf("Confidence = %v, want %v: a person saying so is a declaration",
			e.Confidence, schema.ConfDeclared)
	}
	if e.Kind != schema.EdgePersistsTo || e.Protocol != "redis" {
		t.Errorf("kind/protocol = %q/%q, want persists_to/redis", e.Kind, e.Protocol)
	}
	if len(e.Evidence) != 1 {
		t.Fatalf("evidence = %+v, want one entry", e.Evidence)
	}
	ev := e.Evidence[0]
	if ev.Rule != "declared_override" || ev.Path != override.SourcePath {
		t.Errorf("evidence = %+v, want the config file named as the source", ev)
	}
	if !strings.Contains(ev.Detail, "internal/cache.go") {
		t.Errorf("the reason was not carried into the evidence: %q", ev.Detail)
	}
}

// An unspecified kind is the weakest claim that is still a relationship.
func TestDeclaredRelationshipDefaultsToDependsOn(t *testing.T) {
	nodes, edges := fixture()
	res, _ := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "cache"}},
	})

	e, ok := findEdge(t, res, "api", "cache")
	if !ok {
		t.Fatal("no edge was added")
	}
	if e.Kind != schema.EdgeDependsOn {
		t.Errorf("Kind = %q, want depends_on", e.Kind)
	}
}

// Without a reason, evidence would say only what the confidence already says.
func TestDeclaredRelationshipWithoutANoteStillExplainsItself(t *testing.T) {
	nodes, edges := fixture()
	res, _ := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "cache"}},
	})
	e, _ := findEdge(t, res, "api", "cache")
	if e.Evidence[0].Detail == "" {
		t.Error("evidence has no detail at all")
	}
}

func TestRemovingARelationship(t *testing.T) {
	nodes, edges := fixture()
	res, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "web", To: "api", Remove: true}},
	})

	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", codes(diags))
	}
	if _, ok := findEdge(t, res, "web", "api"); ok {
		t.Error("the relationship was not removed")
	}
	if _, ok := findEdge(t, res, "api", "api.stripe.com"); !ok {
		t.Error("an unrelated relationship was removed as well")
	}
}

// A third-party system exists only because an edge pointed at it. Removing
// that edge must not leave a box asserting the system is part of the
// architecture with nothing connecting it.
func TestRemovingTheLastEdgeToAnExternalDropsIt(t *testing.T) {
	nodes, edges := fixture()
	res, _ := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "api.stripe.com", Remove: true}},
	})

	for _, n := range res.Nodes {
		if n.Name == "api.stripe.com" {
			t.Error("an orphaned external system survived")
		}
	}
	// A service with no relationships is a real finding and must stay.
	var sawCache bool
	for _, n := range res.Nodes {
		if n.Name == "cache" {
			sawCache = true
		}
	}
	if !sawCache {
		t.Error("a datastore with no edges was collected; only externals should be")
	}
}

// A rule that does nothing is worse than no rule: the person believes the
// graph is fixed and stops looking.
func TestStaleRemovalIsReported(t *testing.T) {
	nodes, edges := fixture()
	_, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "web", To: "cache", Remove: true}},
	})

	if len(diags) != 1 || diags[0].Code != "override_no_effect" {
		t.Fatalf("diagnostics = %v, want [override_no_effect]", codes(diags))
	}
	if diags[0].Severity != schema.SeverityWarn {
		t.Errorf("Severity = %q, want warn", diags[0].Severity)
	}
	if diags[0].Path != override.SourcePath {
		t.Errorf("Path = %q, want the config file", diags[0].Path)
	}
}

func TestReferenceThatMatchesNothingIsReported(t *testing.T) {
	nodes, edges := fixture()
	res, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "nowhere", Kind: "calls"}},
	})

	if len(diags) != 1 || diags[0].Code != "override_unresolved" {
		t.Fatalf("diagnostics = %v, want [override_unresolved]", codes(diags))
	}
	if len(res.Edges) != 2 {
		t.Errorf("edges changed despite the rule failing: %+v", res.Edges)
	}
}

// Resolving an ambiguous reference to one of its candidates would be a wrong
// edge nobody can see. It is refused, and the candidates are named.
func TestAmbiguousReferenceIsRefused(t *testing.T) {
	nodes, edges := fixture()
	nodes = append(nodes, node("service:k8s/prod/api", schema.KindService, "api", "prod"))

	_, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "cache"}},
	})

	if len(diags) != 1 || diags[0].Code != "override_ambiguous" {
		t.Fatalf("diagnostics = %v, want [override_ambiguous]", codes(diags))
	}
	for _, want := range []string{"api.shop", "api.prod"} {
		if !strings.Contains(diags[0].Message, want) {
			t.Errorf("the message does not name candidate %q: %s", want, diags[0].Message)
		}
	}
}

// A namespace-qualified name disambiguates, and so does a full id.
func TestQualifiedAndFullReferencesResolve(t *testing.T) {
	nodes, edges := fixture()
	nodes = append(nodes, node("service:k8s/prod/api", schema.KindService, "api", "prod"))

	for _, ref := range []string{"api.shop", "service:compose/shop/api"} {
		res, diags := override.Apply(nodes, edges, override.Rules{
			Relationships: []override.Relationship{{From: ref, To: "cache"}},
		})
		if len(diags) != 0 {
			t.Errorf("%s: diagnostics = %v, want none", ref, codes(diags))
			continue
		}
		if _, ok := findEdge(t, res, "api", "cache"); !ok {
			t.Errorf("%s: no edge was added", ref)
		}
	}
}

func TestUnknownKindIsReported(t *testing.T) {
	nodes, edges := fixture()
	_, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{From: "api", To: "cache", Kind: "talks-to"}},
	})

	if len(diags) != 1 || diags[0].Code != "override_unknown_kind" {
		t.Fatalf("diagnostics = %v, want [override_unknown_kind]", codes(diags))
	}
	if !strings.Contains(diags[0].Message, "persists_to") {
		t.Errorf("the message does not list the valid kinds: %s", diags[0].Message)
	}
}

// Declaring a relationship inference already found is not redundant: the
// declaration is the better source, and the reason is worth recording.
func TestDeclaringAnInferredRelationshipUpgradesIt(t *testing.T) {
	nodes, edges := fixture()
	res, diags := override.Apply(nodes, edges, override.Rules{
		Relationships: []override.Relationship{{
			From: "web", To: "api", Kind: "calls", Note: "confirmed by hand",
		}},
	})

	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", codes(diags))
	}
	e, _ := findEdge(t, res, "web", "api")
	if e.Confidence != schema.ConfDeclared {
		t.Errorf("Confidence = %v, want %v", e.Confidence, schema.ConfDeclared)
	}
	if len(e.Evidence) != 2 {
		t.Errorf("evidence = %+v, want the inference and the declaration both kept", e.Evidence)
	}
}

// Two boxes a person knows are one component. This is also the remedy for the
// nested-project case the basename join deliberately refuses.
func TestMergingTwoComponents(t *testing.T) {
	nodes, edges := fixture()
	nodes = append(nodes, node("service:manifest/go/api", schema.KindService, "acme/api", "go"))
	edges = append(edges,
		edge("service:manifest/go/api", "datastore:compose/shop/cache", schema.EdgePersistsTo, 0.8, "library"))

	res, diags := override.Apply(nodes, edges, override.Rules{
		Components: []override.Component{{Same: []string{"api.shop", "acme/api"}}},
	})

	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", codes(diags))
	}
	for _, n := range res.Nodes {
		if n.Name == "acme/api" {
			t.Error("the folded component survived as its own box")
		}
	}
	// The edge the folded node owned has to move to the survivor.
	if _, ok := findEdge(t, res, "api", "cache"); !ok {
		t.Errorf("the folded component's relationship was lost: %+v", res.Edges)
	}
	// And what it knew about where it was declared.
	for _, n := range res.Nodes {
		if n.Name == "api" && len(n.Sources) != 2 {
			t.Errorf("sources = %+v, want both files", n.Sources)
		}
	}
}

// A merge routinely turns an edge between the two halves into an edge from a
// node to itself, which is not a relationship.
func TestMergingDropsTheEdgeBetweenTheHalves(t *testing.T) {
	nodes, edges := fixture()
	nodes = append(nodes, node("service:manifest/go/api", schema.KindService, "acme/api", "go"))
	edges = append(edges,
		edge("service:compose/shop/api", "service:manifest/go/api", schema.EdgeDependsOn, 0.7, "image"))

	res, _ := override.Apply(nodes, edges, override.Rules{
		Components: []override.Component{{Same: []string{"api.shop", "acme/api"}}},
	})

	for _, e := range res.Edges {
		if e.From == e.To {
			t.Errorf("a self-edge survived the merge: %+v", e)
		}
	}
}

func TestMergeNeedsTwoComponents(t *testing.T) {
	nodes, edges := fixture()
	_, diags := override.Apply(nodes, edges, override.Rules{
		Components: []override.Component{{Same: []string{"api"}}},
	})
	if len(diags) != 1 || diags[0].Code != "override_incomplete" {
		t.Fatalf("diagnostics = %v, want [override_incomplete]", codes(diags))
	}
}

func TestNoRulesChangesNothing(t *testing.T) {
	nodes, edges := fixture()
	res, diags := override.Apply(nodes, edges, override.Rules{})

	if len(diags) != 0 {
		t.Errorf("diagnostics = %v, want none", codes(diags))
	}
	if len(res.Nodes) != len(nodes) || len(res.Edges) != len(edges) {
		t.Errorf("the graph changed with no rules: %d/%d nodes, %d/%d edges",
			len(res.Nodes), len(nodes), len(res.Edges), len(edges))
	}
}

// A merge and a relationship naming the folded half have to compose: the
// relationship should land on the node that survived.
func TestRelationshipAfterAMergeLandsOnTheSurvivor(t *testing.T) {
	nodes, edges := fixture()
	nodes = append(nodes, node("service:manifest/go/api", schema.KindService, "acme/api", "go"))

	res, diags := override.Apply(nodes, edges, override.Rules{
		Components: []override.Component{{Same: []string{"api.shop", "acme/api"}}},
		Relationships: []override.Relationship{{
			From: "api.shop", To: "cache", Kind: "persists_to", Note: "in code",
		}},
	})

	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", codes(diags))
	}
	if _, ok := findEdge(t, res, "api", "cache"); !ok {
		t.Errorf("the relationship did not land on the surviving component: %+v", res.Edges)
	}
}
