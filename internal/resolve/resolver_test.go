package resolve_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

// builder assembles resolver input readably.
type builder struct{ in resolve.Input }

func (b *builder) node(kind schema.NodeKind, scope, name string, attrs schema.Attrs) string {
	id := schema.NewNodeID(kind, scope, name)
	b.in.Nodes = append(b.in.Nodes, schema.Node{
		ID: id, Kind: kind, Layer: schema.LayerOf(kind),
		Name: name, Namespace: scope, Attrs: attrs, Confidence: 1,
	})
	return id
}

func (b *builder) hint(h resolve.Hint) {
	if h.Source.Extractor == "" {
		h.Source = schema.Evidence{Extractor: "test", Path: "test.yaml", Line: 1, Rule: "test"}
	}
	b.in.Hints = append(b.in.Hints, h)
}

func (b *builder) alias(a resolve.Alias) {
	if a.Source.Extractor == "" {
		a.Source = schema.Evidence{Extractor: "test", Path: "test.yaml", Line: 1, Rule: "test"}
	}
	b.in.Aliases = append(b.in.Aliases, a)
}

func (b *builder) run() result { return result{resolve.Resolve(b.in)} }

type result struct{ resolve.Result }

func (r result) names() map[string]string {
	out := map[string]string{}
	for _, n := range r.Nodes {
		out[n.ID] = n.Name
	}
	return out
}

func (r result) edge(t *testing.T, from, to string) schema.Edge {
	t.Helper()
	names := r.names()
	for _, e := range r.Edges {
		if names[e.From] == from && names[e.To] == to {
			return e
		}
	}
	t.Fatalf("no edge %s -> %s; found %v", from, to, r.edgeList())
	return schema.Edge{}
}

func (r result) hasEdge(from, to string) bool {
	names := r.names()
	for _, e := range r.Edges {
		if names[e.From] == from && names[e.To] == to {
			return true
		}
	}
	return false
}

func (r result) edgeList() []string {
	names := r.names()
	out := make([]string, len(r.Edges))
	for i, e := range r.Edges {
		out[i] = names[e.From] + "->" + names[e.To]
	}
	return out
}

func (r result) hasDiag(code string) bool {
	for _, d := range r.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func (r result) node(t *testing.T, name string) schema.Node {
	t.Helper()
	for _, n := range r.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node named %q", name)
	return schema.Node{}
}

func TestServiceSelectorConnectsReferencesToWorkloads(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", schema.Attrs{
		"labels": map[string]string{"app": "api"},
	})
	web := b.node(schema.KindService, "prod", "web", nil)

	// The Service is not a node; it contributes the name "api" routes by.
	b.alias(resolve.Alias{
		Name: "api", Namespace: "prod",
		DNS:      resolve.DNSNames("api", "prod"),
		Ports:    []int{8080},
		Selector: map[string]string{"app": "api"},
	})
	b.hint(resolve.Hint{
		FromNode: web, Kind: resolve.HintEnvURL, Raw: "http://api.prod.svc.cluster.local:8080",
		Tokens: []string{"api.prod.svc.cluster.local", "api.prod", "api"},
		Port:   8080, Protocol: "http", SuggestedEdge: schema.EdgeCalls,
	})

	r := b.run()
	e := r.edge(t, "web", "api")
	if e.To != api {
		t.Errorf("edge points at %q, want the workload %q", e.To, api)
	}
	if e.Kind != schema.EdgeCalls {
		t.Errorf("kind = %q, want calls", e.Kind)
	}
	if e.Evidence[0].Rule != "dns_exact" {
		t.Errorf("rule = %q, want dns_exact", e.Evidence[0].Rule)
	}
}

func TestUnmatchedSelectorIsReported(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "prod", "api", schema.Attrs{
		"labels": map[string]string{"app": "something-else"},
	})
	b.alias(resolve.Alias{
		Name: "api", Namespace: "prod", DNS: resolve.DNSNames("api", "prod"),
		Selector: map[string]string{"app": "api"},
	})

	if !b.run().hasDiag("unmatched_service_selector") {
		t.Error("a Service selecting nothing was not reported")
	}
}

// A Service fronting several workloads is legitimate — a canary, a
// blue/green pair — but a reference to its name then identifies no single
// component, and picking one would be a coin flip.
func TestServiceSelectingManyWorkloadsIsReported(t *testing.T) {
	b := &builder{}
	for _, name := range []string{"api-blue", "api-green"} {
		b.node(schema.KindService, "prod", name, schema.Attrs{
			"labels": map[string]string{"app": "api"},
		})
	}
	b.alias(resolve.Alias{
		Name: "api", Namespace: "prod", DNS: resolve.DNSNames("api", "prod"),
		Selector: map[string]string{"app": "api"},
	})

	r := b.run()
	if !r.hasDiag("service_selects_many") {
		t.Error("a Service selecting several workloads was not reported")
	}
}

// The multi-environment case: dev, staging, and prod declare the same names,
// so picking one would be wrong two times in three.
func TestAmbiguousReferenceIsRefusedNotGuessed(t *testing.T) {
	b := &builder{}
	for _, env := range []string{"dev", "staging", "prod"} {
		b.node(schema.KindDatastore, env, "db", nil)
	}
	// The referring node is in no environment, so namespace scoping cannot
	// break the tie.
	api := b.node(schema.KindService, "default", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, Port: 5432, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("an ambiguous reference produced edges: %v", r.edgeList())
	}
	if !r.hasDiag("ambiguous_reference") {
		t.Fatal("an ambiguous reference was not reported")
	}
	for _, d := range r.Diagnostics {
		if d.Code == "ambiguous_reference" {
			// The message has to name the candidates, or the user cannot
			// act on it.
			for _, want := range []string{"db.dev", "db.staging", "db.prod"} {
				if !strings.Contains(d.Message, want) {
					t.Errorf("diagnostic does not name %q: %s", want, d.Message)
				}
			}
		}
	}
}

// A reference from inside a namespace means the thing in that namespace.
func TestNamespaceScopingBreaksTies(t *testing.T) {
	b := &builder{}
	for _, env := range []string{"dev", "prod"} {
		b.node(schema.KindDatastore, env, "db", nil)
	}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	e := r.edge(t, "api", "db")
	if !strings.Contains(e.To, "@prod/") {
		t.Errorf("edge points at %q, want the database in the referrer's own namespace", e.To)
	}
	if r.hasDiag("ambiguous_reference") {
		t.Error("a reference resolvable by namespace was reported as ambiguous")
	}
}

func TestPortNarrowsButNeverDecidesAlone(t *testing.T) {
	b := &builder{}
	b.node(schema.KindDatastore, "prod", "store", schema.Attrs{"ports": []int{5432}})
	b.node(schema.KindDatastore, "prod", "cache", schema.Attrs{"ports": []int{6379}})
	api := b.node(schema.KindService, "prod", "api", nil)

	// A port alone is not a name: half the services in a repository listen
	// on the same handful of ports.
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvHost, Raw: "somewhere:5432",
		Tokens: []string{"somewhere"}, Port: 5432, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("a port-only match produced edges: %v", r.edgeList())
	}
}

func TestExternalHostsBecomeNodes(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvURL, Raw: "https://api.stripe.com",
		Tokens: []string{"api.stripe.com"}, Port: 443, Protocol: "https",
		SuggestedEdge: schema.EdgeCalls,
	})

	r := b.run()
	stripe := r.node(t, "api.stripe.com")
	if stripe.Kind != schema.KindExternal {
		t.Errorf("kind = %q, want external", stripe.Kind)
	}
	// The context layer is what the outer ring of a C4 diagram is drawn from.
	if stripe.Layer != schema.LayerContext {
		t.Errorf("layer = %q, want context", stripe.Layer)
	}
	if !r.hasEdge("api", "api.stripe.com") {
		t.Errorf("no edge to the external system: %v", r.edgeList())
	}
}

// A bare name matching nothing is far more likely a component this scan
// failed to find than a system on the internet, and inventing an external
// node for it would put a fictional third party on the diagram.
func TestUnmatchedBareNamesDoNotBecomeExternalNodes(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvHost, Raw: "search-service",
		Tokens: []string{"search-service"}, SuggestedEdge: schema.EdgeCalls,
	})

	r := b.run()
	for _, n := range r.Nodes {
		if n.Kind == schema.KindExternal {
			t.Errorf("an unmatched bare name became the external node %q", n.Name)
		}
	}
	if !r.hasDiag("unresolved_reference") {
		t.Error("an unresolvable reference was dropped without a diagnostic")
	}
}

func TestClusterInternalSuffixesAreNotExternal(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	for _, host := range []string{"warehouse.legacy.internal", "db.cluster.local", "thing.svc"} {
		b.hint(resolve.Hint{
			FromNode: api, Kind: resolve.HintEnvHost, Raw: host,
			Tokens: []string{host}, SuggestedEdge: schema.EdgeCalls,
		})
	}

	for _, n := range b.run().Nodes {
		if n.Kind == schema.KindExternal {
			t.Errorf("cluster-internal host %q was treated as a third-party system", n.Name)
		}
	}
}

// A Compose build context and a go.mod in the same directory describe one
// component, not a container with no language plus a codebase with no
// deployment.
func TestCodebaseMergesIntoTheContainerThatRunsIt(t *testing.T) {
	b := &builder{}
	deployment := b.node(schema.KindService, "shop", "gateway", schema.Attrs{
		"buildContext": "services/gateway",
	})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Runtime: "container"}

	code := b.node(schema.KindService, "", "services/gateway", schema.Attrs{
		"directory": "services/gateway",
		"module":    "github.com/acme/gateway",
	})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Language: "go", Framework: "gin"}

	db := b.node(schema.KindDatastore, "shop", "db", nil)
	b.hint(resolve.Hint{
		FromNode: deployment, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, SuggestedEdge: schema.EdgePersistsTo,
	})
	_ = code
	_ = db

	r := b.run()
	if len(r.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 after the merge: %v", len(r.Nodes), r.names())
	}
	gateway := r.node(t, "gateway")
	if gateway.Tech.Language != "go" {
		t.Errorf("language = %q, want the codebase's language to survive", gateway.Tech.Language)
	}
	if gateway.Tech.Framework != "gin" {
		t.Errorf("framework = %q, want gin", gateway.Tech.Framework)
	}
	if gateway.Attrs["module"] != "github.com/acme/gateway" {
		t.Errorf("module = %v, want the codebase's module to survive", gateway.Attrs["module"])
	}
	// The deployment survives: it is the thing other infrastructure refers to.
	if gateway.ID != deployment {
		t.Errorf("surviving node is %q, want the deployment %q", gateway.ID, deployment)
	}
}

// A ConfigMap holds the values and the pod holds the reference to it; neither
// file knows about the other.
func TestConfigMapIndirection(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.node(schema.KindService, "prod", "search", nil)

	const ref = "configmap:prod/endpoints"
	b.hint(resolve.Hint{
		FromNode: ref, Kind: resolve.HintConfigValue, Raw: "search",
		Tokens: []string{"search"}, SuggestedEdge: schema.EdgeCalls,
	})
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConfigValue, Raw: ref,
		SuggestedEdge: schema.EdgeDependsOn,
	})

	r := b.run()
	e := r.edge(t, "api", "search")
	// A step removed from the pod declaring the endpoint itself, and priced
	// accordingly.
	if e.Confidence != schema.ConfIndirect {
		t.Errorf("confidence = %v, want %v for a value reached through a mount", e.Confidence, schema.ConfIndirect)
	}
}

func TestConfigMapPseudoNodesNeverReachTheGraph(t *testing.T) {
	b := &builder{}
	b.hint(resolve.Hint{
		FromNode: "configmap:prod/endpoints", Kind: resolve.HintConfigValue,
		Raw: "search", Tokens: []string{"search"},
	})

	for _, n := range b.run().Nodes {
		if strings.HasPrefix(n.ID, "configmap:") {
			t.Errorf("a synthetic ConfigMap identifier became a node: %q", n.ID)
		}
	}
}

// A Postgres driver says this service talks to a Postgres; it never says
// which one. Turning that into an edge would be a guess that reads as a
// finding.
func TestLibraryHintsCorroborateButNeverEstablish(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Language: "go"}
	b.node(schema.KindDatastore, "prod", "db", schema.Attrs{"image": "postgres:16"})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Framework: "postgres"}

	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintLibrary, Raw: "github.com/lib/pq",
		Tokens: []string{"postgres"}, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("a library dependency established an edge on its own: %v", r.edgeList())
	}
	// The information survives on the node.
	uses, _ := r.node(t, "api").Attrs["usesTechnology"].([]string)
	if len(uses) != 1 || uses[0] != "postgres" {
		t.Errorf("usesTechnology = %v, want [postgres]", uses)
	}
}

func TestLibraryHintAddsEvidenceToAnExistingEdge(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	db := b.node(schema.KindDatastore, "prod", "db", nil)
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Framework: "postgres"}

	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, SuggestedEdge: schema.EdgePersistsTo,
	})
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintLibrary, Raw: "github.com/lib/pq",
		Tokens: []string{"postgres"}, SuggestedEdge: schema.EdgePersistsTo,
	})
	_ = db

	e := b.run().edge(t, "api", "db")
	if len(e.Evidence) != 2 {
		t.Errorf("got %d evidence records, want the library to corroborate the connection string", len(e.Evidence))
	}
}

// Showing both makes the graph look like there are two relationships, and
// forces a reader to work out that they are the same arrow.
func TestGenericDependencyCollapsesIntoTheSpecificOne(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "shop", "api", nil)
	db := b.node(schema.KindDatastore, "shop", "db", nil)

	b.in.Edges = append(b.in.Edges, schema.Edge{
		From: api, To: db, Kind: schema.EdgeDependsOn, Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{Extractor: "compose", Rule: "compose_depends_on"}},
	})
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 1 {
		t.Fatalf("got %d edges, want the declaration folded into the specific relationship: %v",
			len(r.Edges), r.edgeList())
	}
	e := r.Edges[0]
	if e.Kind != schema.EdgePersistsTo {
		t.Errorf("kind = %q, want the more specific persists_to", e.Kind)
	}
	// Stated outright and corroborated by a connection string is as certain
	// as this gets.
	if e.Confidence != schema.ConfDeclared {
		t.Errorf("confidence = %v, want %v", e.Confidence, schema.ConfDeclared)
	}
	if len(e.Evidence) != 2 {
		t.Errorf("got %d evidence records, want both readings kept", len(e.Evidence))
	}
}

func TestSelfReferencesAreNotEdges(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvHost, Raw: "api",
		Tokens: []string{"api"}, SuggestedEdge: schema.EdgeCalls,
	})

	if r := b.run(); len(r.Edges) != 0 {
		t.Errorf("a service referring to itself produced an edge: %v", r.edgeList())
	}
}

func TestNormalizedNamesMatchAtLowerConfidence(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "prod", "user-service", nil)
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvHost, Raw: "user_service",
		Tokens: []string{"user_service"}, SuggestedEdge: schema.EdgeCalls,
	})

	e := b.run().edge(t, "api", "user-service")
	// Collapsing separators can bring genuinely different names together, so
	// it scores below an exact match.
	if e.Confidence >= resolve.HintEnvHost.BaseConfidence() {
		t.Errorf("confidence = %v, want less than an exact match's %v",
			e.Confidence, resolve.HintEnvHost.BaseConfidence())
	}
}

func TestResolutionIsOrderIndependent(t *testing.T) {
	build := func() *builder {
		b := &builder{}
		api := b.node(schema.KindService, "prod", "api", nil)
		b.node(schema.KindDatastore, "prod", "db", nil)
		b.node(schema.KindQueue, "prod", "queue", nil)
		for _, token := range []string{"db", "queue"} {
			b.hint(resolve.Hint{
				FromNode: api, Kind: resolve.HintConnString, Raw: token,
				Tokens: []string{token}, SuggestedEdge: schema.EdgePersistsTo,
			})
		}
		return b
	}

	baseline := strings.Join(build().run().edgeList(), ",")
	for range 30 {
		b := build()
		// Reversing the input exercises the path where iteration order would
		// otherwise leak into the output.
		for i, j := 0, len(b.in.Hints)-1; i < j; i, j = i+1, j-1 {
			b.in.Hints[i], b.in.Hints[j] = b.in.Hints[j], b.in.Hints[i]
		}
		if got := strings.Join(b.run().edgeList(), ","); got != baseline {
			t.Fatalf("edge set varied with input order:\n %s\n %s", baseline, got)
		}
	}
}

func TestEveryEdgeCarriesEvidence(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.node(schema.KindDatastore, "prod", "db", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "postgres://db:5432/app",
		Tokens: []string{"db"}, SuggestedEdge: schema.EdgePersistsTo,
	})

	for _, e := range b.run().Edges {
		if len(e.Evidence) == 0 {
			t.Errorf("edge %s -> %s has no evidence", e.From, e.To)
		}
		for _, ev := range e.Evidence {
			if ev.Rule == "" {
				t.Errorf("edge %s -> %s has evidence with no rule", e.From, e.To)
			}
		}
	}
}

// TestReferenceResolvesInsideItsOwnScopeFirst is the bug a real scan found:
// a compose service reached across into an unrelated Kubernetes tree for a
// name that was defined five lines below it in its own file.
//
// The cause was rule precedence beating locality. dns_exact searched a global
// index, found exactly one node, and returned before name_exact could offer
// the local one. One candidate is not the same as the right candidate.
func TestReferenceResolvesInsideItsOwnScopeFirst(t *testing.T) {
	b := &builder{}
	local := b.node(schema.KindDatastore, "shop", "orders-db", nil)
	foreign := b.node(schema.KindDatastore, "prod", "orders-db", nil)
	// A Service in the foreign namespace contributes DNS names, which is
	// what makes dns_exact -- the strongest rule -- reach across.
	b.alias(resolve.Alias{
		Name: "orders-db", Namespace: "prod", TargetName: "orders-db",
		DNS: []string{"orders-db", "orders-db.prod", "orders-db.prod.svc.cluster.local"},
	})
	_ = foreign
	checkout := b.node(schema.KindService, "shop", "checkout", nil)
	b.hint(resolve.Hint{
		FromNode: checkout, Kind: resolve.HintConnString,
		Raw:    "postgres://checkout@orders-db:5432/orders",
		Tokens: []string{"orders-db"}, Port: 5432, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 1 {
		t.Fatalf("want exactly one edge, got %v", r.edgeList())
	}
	if got := r.Edges[0].To; got != local {
		t.Errorf("resolved to %q, want the same-project %q; a name reused by an "+
			"unrelated project is not a dependency", got, local)
	}
}

// TestOutOfScopeReferenceIsRefusedAndReported covers the other half: when the
// only candidate belongs to someone else, no edge is drawn and the refusal is
// visible.
//
// Silence would leave a reader concluding the component has no such
// dependency, when one was found and rejected.
func TestOutOfScopeReferenceIsRefusedAndReported(t *testing.T) {
	b := &builder{}
	b.node(schema.KindQueue, "shop", "broker", nil)
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString, Raw: "amqp://broker:5672",
		Tokens: []string{"broker"}, Port: 5672, SuggestedEdge: schema.EdgePublishesTo,
	})

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("a cross-project reference produced edges: %v", r.edgeList())
	}
	if !r.hasDiag("out_of_scope_reference") {
		t.Fatal("a refused cross-project reference was not reported")
	}
}

// TestExternalsAreReachableFromAnyNamespace guards the fix to the fix.
//
// Scoping references to a namespace must not scope out third parties. An
// external system belongs to no namespace, and dropping those edges would
// remove exactly the dependencies a reader most wants to see.
func TestExternalsAreReachableFromAnyNamespace(t *testing.T) {
	b := &builder{}
	api := b.node(schema.KindService, "prod", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintEnvURL, Raw: "https://api.stripe.com",
		Tokens: []string{"api.stripe.com"}, SuggestedEdge: schema.EdgeCalls,
	})

	r := b.run()
	if len(r.Edges) != 1 {
		t.Fatalf("an external dependency was dropped by namespace scoping: %v", r.edgeList())
	}
}

// TestBoundariesAreNeverResolutionTargets keeps a service from "depending on"
// the grouping that encloses it.
//
// A boundary is a system or a chart, and takes part through contains edges
// only. Charts routinely name the boundary and the service alike, so without
// this a chart referencing its own release name produces an edge that says
// nothing and reads as a finding.
func TestBoundariesAreNeverResolutionTargets(t *testing.T) {
	b := &builder{}
	b.node(schema.KindBoundary, "podinfo", "podinfo", nil)
	svc := b.node(schema.KindService, "podinfo", "podinfo-svc", nil)
	b.hint(resolve.Hint{
		FromNode: svc, Kind: resolve.HintEnvHost, Raw: "podinfo",
		Tokens: []string{"podinfo"}, SuggestedEdge: schema.EdgeCalls,
	})

	r := b.run()
	for _, e := range r.Edges {
		if strings.HasPrefix(e.To, "boundary:") && e.Kind != schema.EdgeContains {
			t.Errorf("a reference resolved to a boundary: %v", r.edgeList())
		}
	}
}

// TestDeploymentJoinsItsCodeWithoutABuildContext covers the common case that
// went unhandled.
//
// A Kubernetes Deployment references an image, not a path, so the build
// context join never fired for a manifest repository. Every service was drawn
// twice: a container with no language, and a codebase with no deployment. For
// a diagram that is not a missing detail, it is the same box twice.
func TestDeploymentJoinsItsCodeWithoutABuildContext(t *testing.T) {
	b := &builder{}
	code := b.node(schema.KindService, "", "checkoutservice",
		schema.Attrs{"directory": "src/checkoutservice", "module": "acme/checkoutservice"})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Language: "go"}
	deployment := b.node(schema.KindService, "default", "checkoutservice",
		schema.Attrs{"image": "checkoutservice:1.0"})

	r := b.run()

	ids := map[string]bool{}
	for _, n := range r.Nodes {
		ids[n.ID] = true
	}
	if ids[code] {
		t.Errorf("the codebase survived as its own node; the diagram would draw "+
			"%s twice", "checkoutservice")
	}
	if !ids[deployment] {
		t.Fatal("the deployment was merged away; it is the thing that runs")
	}
	for _, n := range r.Nodes {
		if n.ID != deployment {
			continue
		}
		if n.Tech == nil || n.Tech.Language != "go" {
			t.Errorf("the language the codebase knew was not carried over: %+v", n.Tech)
		}
	}
	if !r.hasDiag("code_joined_to_deployment") {
		t.Error("an inferred join was not reported; it has to be auditable")
	}
}

// TestNameCollisionAcrossProjectsIsNotAJoin is the guard on that inference.
//
// A repository of independent samples has a dozen services called "web". Each
// would claim the one web/package.json, and checking only that a deployment
// found exactly one codebase does not catch it -- the check has to be unique
// in both directions. Fusing two unrelated stacks into one box is worse than
// drawing two.
func TestNameCollisionAcrossProjectsIsNotAJoin(t *testing.T) {
	b := &builder{}
	code := b.node(schema.KindService, "", "web",
		schema.Attrs{"directory": "nginx-nodejs-redis/web"})
	for _, project := range []string{"angular", "django", "nginx-flask-mongo"} {
		b.node(schema.KindService, project, "web",
			schema.Attrs{"image": "web:latest"})
	}

	r := b.run()

	var found bool
	for _, n := range r.Nodes {
		if n.ID == code {
			found = true
		}
	}
	if !found {
		t.Error("a codebase claimed by several unrelated projects was merged into one of them")
	}
	if r.hasDiag("code_joined_to_deployment") {
		t.Error("an ambiguous join was reported as made")
	}
}

// TestBuildContextJoinStillWins keeps the stronger signal authoritative.
func TestBuildContextJoinStillWins(t *testing.T) {
	b := &builder{}
	code := b.node(schema.KindService, "", "checkout",
		schema.Attrs{"directory": "services/checkout"})
	b.node(schema.KindService, "shop", "checkout",
		schema.Attrs{"buildContext": "services/checkout", "image": "shop-checkout"})

	r := b.run()
	for _, n := range r.Nodes {
		if n.ID == code {
			t.Error("a build context named the directory outright and the join did not happen")
		}
	}
	// A declared build context infers nothing, so it is not reported.
	if r.hasDiag("code_joined_to_deployment") {
		t.Error("a declared build context was reported as an inference")
	}
}

// An edge whose name matched one component and an edge whose name matched
// several must not carry the same number.
//
// The second required a judgement the first did not, and scoring them alike
// asserts a certainty that was not there. The rule name has always recorded
// the difference by appending "+scoped"; the confidence did not, so a reader
// comparing two 0.80 edges had no way to tell which one had been
// disambiguated -- and the number is the part a model actually reads.
func TestADisambiguatedMatchScoresBelowAnUnambiguousOne(t *testing.T) {
	// One component answers to the name, so nothing has to be decided.
	unambiguous := func() float64 {
		b := &builder{}
		b.node(schema.KindDatastore, "shop", "cache", nil)
		api := b.node(schema.KindService, "shop", "api", nil)
		b.hint(resolve.Hint{
			FromNode: api, Kind: resolve.HintConnString,
			Raw:    "redis://cache:6379/0",
			Tokens: []string{"cache"}, SuggestedEdge: schema.EdgePersistsTo,
		})
		r := b.run()
		if len(r.Edges) != 1 {
			t.Fatalf("want one edge from the unambiguous case, got %v", r.edgeList())
		}
		return r.Edges[0].Confidence
	}()

	// Two components answer to it and the referrer is in neither's scope, so
	// the locality gate cannot separate them. What decides is the kind the
	// reference implies: a redis:// URL means the datastore. Correct, and a
	// decision nonetheless.
	//
	// The two are in different scopes, which is the shape this ambiguity has
	// now: one scope cannot hold two components of one name, because that is
	// one identifier. Two scopes reusing a name is the common case -- a dev
	// and a prod overlay, a chart beside a cluster.
	b := &builder{}
	store := b.node(schema.KindDatastore, "staging", "cache", nil)
	b.node(schema.KindService, "prod", "cache", nil)
	api := b.node(schema.KindService, "", "api", nil)
	b.hint(resolve.Hint{
		FromNode: api, Kind: resolve.HintConnString,
		Raw:    "redis://cache:6379/0",
		Tokens: []string{"cache"}, SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if len(r.Edges) != 1 {
		t.Fatalf("want one edge from the ambiguous case, got %v", r.edgeList())
	}
	edge := r.Edges[0]
	if edge.To != store {
		t.Fatalf("resolved to %q, want the datastore %q", edge.To, store)
	}
	if rule := edge.Evidence[0].Rule; !strings.Contains(rule, "+scoped") {
		t.Fatalf("rule %q does not record that the match was decided rather than "+
			"found; the fixture is not exercising the tie-break", rule)
	}
	if edge.Confidence >= unambiguous {
		t.Errorf("a disambiguated match scored %.2f, the same as or above the "+
			"unambiguous %.2f", edge.Confidence, unambiguous)
	}
	// One step, not a tier: it is still the same rule's finding.
	if diff := unambiguous - edge.Confidence; diff > 0.11 {
		t.Errorf("the penalty is %.2f, which demotes the edge past a whole "+
			"confidence tier rather than one step", diff)
	}
}
