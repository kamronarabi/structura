package resolve_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

func libraryHint(from, tech string, vendor bool, edge schema.EdgeKind) resolve.Hint {
	return resolve.Hint{
		FromNode:      from,
		Kind:          resolve.HintLibrary,
		Raw:           tech + "-client",
		Tokens:        []string{tech},
		Protocol:      tech,
		Vendor:        vendor,
		SuggestedEdge: edge,
		Source: schema.Evidence{
			Extractor: "manifest", Path: "package.json", Line: 1,
			Rule:   "manifest_dependency_implies",
			Detail: "package.json depends on something, which indicates " + tech,
		},
	}
}

// The case the table was extended for: an application whose backend is a
// managed service, with no container and no connection string anywhere.
func TestVendorDependencyDrawsTheCompany(t *testing.T) {
	b := &builder{}
	app := b.node(schema.KindService, "", "storefront",
		schema.Attrs{"directory": "."})
	b.hint(libraryHint(app, "supabase", true, schema.EdgeCalls))

	r := b.run()
	if !r.hasEdge("storefront", "supabase") {
		t.Fatalf("no edge to the vendor; found %v", r.edgeList())
	}
	e := r.edge(t, "storefront", "supabase")
	if e.Confidence != schema.ConfWeak {
		t.Errorf("Confidence = %v, want %v: a dependency names the company and not the endpoint",
			e.Confidence, schema.ConfWeak)
	}
	if e.Evidence[0].Rule != "dependency_names_vendor" {
		t.Errorf("Rule = %q, want dependency_names_vendor", e.Evidence[0].Rule)
	}

	n := r.node(t, "supabase")
	if n.Kind != schema.KindExternal {
		t.Errorf("Kind = %q, want external", n.Kind)
	}
	// Nothing named an endpoint, so the node must not claim one.
	if _, ok := n.Attrs["hostname"]; ok {
		t.Errorf("the vendor node invented a hostname: %+v", n.Attrs)
	}
	if n.Attrs["vendor"] != "supabase" {
		t.Errorf("attrs = %+v, want the vendor recorded", n.Attrs)
	}
}

// A driver names a kind of thing. Inventing "a Postgres" would be a box
// standing for nothing.
func TestTechnologyDependencyDrawsNothing(t *testing.T) {
	b := &builder{}
	app := b.node(schema.KindService, "", "storefront",
		schema.Attrs{"directory": "."})
	b.hint(libraryHint(app, "postgres", false, schema.EdgePersistsTo))

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("edges = %v, want none", r.edgeList())
	}
	for _, n := range r.Nodes {
		if n.Name == "postgres" {
			t.Error("a technology was drawn as a component")
		}
	}
	// It still says what the service uses.
	n := r.node(t, "storefront")
	tech, _ := n.Attrs["usesTechnology"].([]string)
	if len(tech) != 1 || tech[0] != "postgres" {
		t.Errorf("usesTechnology = %v, want [postgres]", tech)
	}
}

// When configuration named the endpoint, that is strictly better than the
// dependency knowing the company: it has the account in it. The dependency
// corroborates and draws nothing.
func TestConfigEndpointWinsOverTheVendorName(t *testing.T) {
	b := &builder{}
	app := b.node(schema.KindService, "", "storefront",
		schema.Attrs{"directory": "."})
	// An env URL resolved to the real project endpoint.
	b.hint(resolve.Hint{
		FromNode: app, Kind: resolve.HintEnvURL,
		Raw:           "https://kbqjfnwtsdplmvxr.supabase.co",
		Tokens:        []string{"kbqjfnwtsdplmvxr.supabase.co"},
		SuggestedEdge: schema.EdgeCalls,
		Source:        schema.Evidence{Extractor: "dotenv", Path: ".env", Line: 1, Rule: "dotenv_reference"},
	})
	b.hint(libraryHint(app, "supabase", true, schema.EdgeCalls))

	r := b.run()
	for _, n := range r.Nodes {
		if n.Name == "supabase" {
			t.Error("a bare vendor node was drawn beside the endpoint that names the same system")
		}
	}
	e := r.edge(t, "storefront", "kbqjfnwtsdplmvxr.supabase.co")
	if len(e.Evidence) != 2 {
		t.Fatalf("evidence = %+v, want the endpoint and the dependency both recorded", e.Evidence)
	}
	var corroborated bool
	for _, ev := range e.Evidence {
		if ev.Rule == "library_corroborates" {
			corroborated = true
		}
	}
	if !corroborated {
		t.Errorf("the dependency did not corroborate the endpoint: %+v", e.Evidence)
	}
	if e.Confidence != schema.ConfHostMatch {
		t.Errorf("Confidence = %v, want the endpoint's %v", e.Confidence, schema.ConfHostMatch)
	}
}

// Two services using one company share one node, or the context diagram grows
// a box per caller.
func TestOneVendorNodeServesEveryCaller(t *testing.T) {
	b := &builder{}
	web := b.node(schema.KindService, "", "web", schema.Attrs{"directory": "web"})
	api := b.node(schema.KindService, "", "api", schema.Attrs{"directory": "api"})
	b.hint(libraryHint(web, "stripe", true, schema.EdgeCalls))
	b.hint(libraryHint(api, "stripe", true, schema.EdgeCalls))

	r := b.run()
	count := 0
	for _, n := range r.Nodes {
		if n.Name == "stripe" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("stripe nodes = %d, want 1", count)
	}
	if !r.hasEdge("web", "stripe") || !r.hasEdge("api", "stripe") {
		t.Errorf("both callers should reach it; found %v", r.edgeList())
	}
}

// A declared instance of the technology still wins, which is the behaviour
// that existed before vendors could be drawn.
func TestDeclaredInstanceIsCorroboratedNotDuplicated(t *testing.T) {
	b := &builder{}
	app := b.node(schema.KindService, "shop", "api", schema.Attrs{"directory": "api"})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Framework: "redis"}
	cache := b.node(schema.KindDatastore, "shop", "cache", nil)
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Framework: "redis"}
	_ = cache

	b.in.Edges = append(b.in.Edges, schema.Edge{
		From: app, To: cache, Kind: schema.EdgePersistsTo, Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{Extractor: "compose", Rule: "compose_depends_on"}},
	})
	b.hint(libraryHint(app, "redis", false, schema.EdgePersistsTo))

	r := b.run()
	e := r.edge(t, "api", "cache")
	var corroborated bool
	for _, ev := range e.Evidence {
		if ev.Rule == "library_corroborates" {
			corroborated = true
		}
	}
	if !corroborated {
		t.Errorf("the driver did not corroborate the declared instance: %+v", e.Evidence)
	}
}

// The protocol on a library hint is the technology name, which would render
// as "over supabase".
func TestVendorEdgeCarriesAUsefulProtocol(t *testing.T) {
	b := &builder{}
	app := b.node(schema.KindService, "", "web", schema.Attrs{"directory": "."})
	b.hint(libraryHint(app, "clerk", true, schema.EdgeCalls))

	r := b.run()
	e := r.edge(t, "web", "clerk")
	if e.Protocol != "https" {
		t.Errorf("Protocol = %q, want https", e.Protocol)
	}
}
