package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kamronarabi/structura/internal/golden"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// fixture returns the path to a committed fixture repository.
func fixture(name string) string {
	return filepath.Join("..", "..", "testdata", "golden", name)
}

func run(t *testing.T, root string, mutate ...func(*scan.Options)) scan.Result {
	t.Helper()
	opts := scan.Options{Root: root}
	for _, m := range mutate {
		m(&opts)
	}
	res, err := scan.Run(context.Background(), extractors.Default(), opts)
	if err != nil {
		t.Fatalf("scan.Run() = %v", err)
	}
	return res
}

// fixtures lists every committed fixture repository, so that a test added
// for one milestone automatically covers the ones added later.
func fixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "testdata", "golden"))
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatal("no fixture repositories found")
	}
	return out
}

// The M2 acceptance criterion, and the invariant every later milestone leans
// on: the cache, the golden tests, and drift detection all assume that an
// unchanged tree produces unchanged bytes.
//
// This runs over every fixture rather than a representative one. A
// non-deterministic framework choice in the manifest extractor survived a
// single-fixture version of this test, because the fixture it checked had no
// package.json.
func TestScanIsDeterministic(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			first, err := schema.Marshal(run(t, fixture(name)).Graph.Canonical())
			if err != nil {
				t.Fatal(err)
			}
			// Varying the worker count exercises different completion
			// orders, which is precisely what would leak into the output if
			// the merge stage depended on arrival order rather than on
			// sorted path order.
			for _, workers := range []int{1, 2, 3, 8, 16} {
				again, err := schema.Marshal(run(t, fixture(name), func(o *scan.Options) {
					o.Concurrency = workers
				}).Graph.Canonical())
				if err != nil {
					t.Fatal(err)
				}
				if string(first) != string(again) {
					t.Fatalf("scan with %d workers produced a different graph", workers)
				}
			}
		})
	}
}

// Every fixture's full output is pinned, so that a change in any extractor
// shows up as a reviewable diff rather than as a silently different graph.
func TestAllFixturesGolden(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			out, err := schema.Marshal(run(t, fixture(name)).Graph.Canonical())
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, filepath.Join("testdata", "golden", name+".json"), out)
		})
	}
}

// No fixture's graph may contain anything credential-shaped. Several of them
// deliberately contain credentials in their inputs.
func TestNoFixtureLeaksCredentials(t *testing.T) {
	secrets := []string{"hunter2", "s3cr3t", "sk_live_", "guestpassword", ":pw@"}
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			out, err := schema.Marshal(run(t, fixture(name)).Graph)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range secrets {
				if strings.Contains(string(out), secret) {
					t.Errorf("credential %q reached graph.json", secret)
				}
			}
		})
	}
}

func TestScanProducesTheExpectedComponents(t *testing.T) {
	g := run(t, fixture("compose-monolith")).Graph

	byName := map[string]schema.Node{}
	for _, n := range g.Nodes {
		byName[n.Name] = n
	}
	want := map[string]schema.NodeKind{
		"web":       schema.KindService,
		"worker":    schema.KindService,
		"db":        schema.KindDatastore,
		"cache":     schema.KindDatastore,
		"search":    schema.KindDatastore,
		"queue":     schema.KindQueue,
		"shopfront": schema.KindBoundary,
		// Synthesized by the resolver from STRIPE_API_URL. A third-party
		// system the repository talks to is part of the architecture, and is
		// what the outer ring of a context diagram is drawn from.
		"api.stripe.com": schema.KindExternal,
	}
	for name, kind := range want {
		n, ok := byName[name]
		if !ok {
			t.Errorf("node %q is missing", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%s is %q, want %q", name, n.Kind, kind)
		}
	}
	if len(g.Nodes) != len(want) {
		t.Errorf("got %d nodes, want %d", len(g.Nodes), len(want))
	}
}

// Nothing that looks like a credential may reach the file, and the fixture
// deliberately contains several.
func TestScanOutputCarriesNoCredentials(t *testing.T) {
	out, err := schema.Marshal(run(t, fixture("compose-monolith")).Graph)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "sk_live_EXAMPLENOTAREALKEY"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("credential %q reached graph.json:\n%s", secret, out)
		}
	}
}

// graph.json is committed and diffed across machines, so an absolute path in
// it would leak the author's home directory and make two machines' graphs
// incomparable.
func TestScanOutputContainsNoAbsolutePaths(t *testing.T) {
	res := run(t, fixture("compose-monolith"))
	out, err := schema.Marshal(res.Graph)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(fixture("compose-monolith"))
	if err != nil {
		t.Fatal(err)
	}

	leaks := []string{abs}
	// os.Getenv("HOME") is empty on Windows, which would reduce the needle to
	// "/" and match every node ID in the graph. UserHomeDir reads USERPROFILE
	// there and HOME elsewhere. Both spellings of the separator are checked,
	// because graph paths are slash-separated on every platform while the
	// home directory is reported natively.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		leaks = append(leaks, home+string(filepath.Separator), filepath.ToSlash(home)+"/")
	}
	for _, leak := range leaks {
		if strings.Contains(string(out), leak) {
			t.Errorf("an absolute path (%q) reached graph.json:\n%s", leak, out)
		}
	}
	if res.Graph.Root.Name != "compose-monolith" {
		t.Errorf("root name = %q, want the directory's base name only", res.Graph.Root.Name)
	}
}

func TestEmptyRepositoryProducesAValidGraph(t *testing.T) {
	res := run(t, t.TempDir())

	// Exit cleanly with an empty graph rather than an error: "this
	// repository has no manifests" is an answer, not a failure.
	if len(res.Graph.Nodes) != 0 {
		t.Errorf("got %d nodes for an empty repository", len(res.Graph.Nodes))
	}
	if res.Graph.SchemaVersion != schema.Version {
		t.Errorf("schema version = %q", res.Graph.SchemaVersion)
	}
	if _, err := schema.Marshal(res.Graph); err != nil {
		t.Errorf("an empty graph does not serialize: %v", err)
	}
}

func TestGarbageRepositoryProducesDiagnosticsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"docker-compose.yml":       "\x00\x01\x02 not yaml at all",
		"sub/compose.yaml":         "services: [1, 2, 3]",
		"deep/docker-compose.yaml": strings.Repeat("a: {", 200),
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := run(t, dir)
	if len(res.Graph.Diagnostics) == 0 {
		t.Error("garbage input produced no diagnostics; the gaps must be reported")
	}
	if _, err := schema.Marshal(res.Graph); err != nil {
		t.Errorf("the graph from a garbage repository does not serialize: %v", err)
	}
}

// panicky reproduces the worst case: an extractor that crashes on some input.
type panicky struct{ calls atomic.Int32 }

func (*panicky) Name() string             { return "panicky" }
func (*panicky) Match(scan.FileMeta) bool { return true }
func (p *panicky) Extract(context.Context, *scan.File, scan.Emitter) error {
	p.calls.Add(1)
	panic("extractor bug")
}

// One bad file must degrade into a diagnostic, never take the scan down. A
// tool that crashes on a repository tells the user nothing about the files it
// had already understood.
func TestExtractorPanicBecomesADiagnostic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.yml", "b.yml", "c.yml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x: 1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	defer scan.SilencePanicStacks()()

	p := &panicky{}
	res, err := scan.Run(context.Background(), scan.NewRegistry(p), scan.Options{Root: dir})
	if err != nil {
		t.Fatalf("a panicking extractor failed the whole scan: %v", err)
	}
	if got := p.calls.Load(); got != 3 {
		t.Errorf("extractor ran %d times, want 3: the scan stopped after the first panic", got)
	}
	panics := 0
	for _, d := range res.Graph.Diagnostics {
		if d.Code == "extractor_panic" {
			panics++
		}
	}
	if panics != 3 {
		t.Errorf("got %d panic diagnostics, want 3", panics)
	}
}

// erroring returns an error rather than panicking.
type erroring struct{}

func (erroring) Name() string             { return "erroring" }
func (erroring) Match(scan.FileMeta) bool { return true }
func (erroring) Extract(context.Context, *scan.File, scan.Emitter) error {
	return context.DeadlineExceeded
}

func TestExtractorErrorBecomesADiagnostic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yml"), []byte("x: 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := scan.Run(context.Background(), scan.NewRegistry(erroring{}), scan.Options{Root: dir})
	if err != nil {
		t.Fatalf("scan.Run() = %v", err)
	}
	found := false
	for _, d := range res.Graph.Diagnostics {
		if d.Code == "extract_failed" {
			found = true
		}
	}
	if !found {
		t.Errorf("an extractor error was swallowed; diagnostics: %v", res.Graph.Diagnostics)
	}
}

func TestDebugDumpIsOnlyBuiltWhenAsked(t *testing.T) {
	if res := run(t, fixture("compose-monolith")); res.Intermediate != nil {
		t.Error("the intermediate was retained without being requested")
	}

	res := run(t, fixture("compose-monolith"), func(o *scan.Options) { o.KeepIntermediate = true })
	if res.Intermediate == nil {
		t.Fatal("the intermediate was requested and not produced")
	}

	var hints []resolve.Hint
	for _, f := range res.Intermediate.Files {
		hints = append(hints, f.Hints...)
	}
	if len(hints) == 0 {
		t.Fatal("no hints were recorded; extractor work would be unobservable before the resolver exists")
	}
	// --debug-dump prints these straight to a terminal, so they are redacted
	// at the point of creation rather than only at serialization.
	for _, h := range hints {
		if strings.Contains(h.Raw, "hunter2") {
			t.Errorf("a credential is present in the debug dump: %q", h.Raw)
		}
	}
}

func TestScanRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scan.Run(ctx, extractors.Default(), scan.Options{Root: fixture("compose-monolith")}); err == nil {
		t.Error("scan.Run() = nil, want the cancellation to be returned")
	}
}

// The end-to-end proof that the resolver does its job: a Kubernetes-native
// repository states almost no relationships outright, and before resolution
// produces a set of labelled boxes with nothing between them.
func TestResolverConnectsAKubernetesRepository(t *testing.T) {
	g := run(t, fixture("k8s-microservices")).Graph

	names := map[string]string{}
	for _, n := range g.Nodes {
		names[n.ID] = n.Name
	}
	// Containment is structure, not a relationship the resolver inferred;
	// this test is about what resolution produced.
	found := map[string]schema.Edge{}
	for _, e := range g.Edges {
		if e.Kind == schema.EdgeContains {
			continue
		}
		found[names[e.From]+" -> "+names[e.To]] = e
	}

	want := map[string]struct {
		kind       schema.EdgeKind
		confidence float64
	}{
		// Ingress -> Service -> Deployment: a deterministic chain, resolved
		// through the Service's selector rather than by name matching.
		"public -> api-gateway": {schema.EdgeExposes, schema.ConfReference},
		"public -> checkout":    {schema.EdgeExposes, schema.ConfReference},

		// Environment variables naming other services.
		"api-gateway -> checkout":      {schema.EdgeCalls, schema.ConfHostMatch},
		"api-gateway -> session-cache": {schema.EdgePersistsTo, schema.ConfHostMatch},
		"checkout -> orders-db":        {schema.EdgePersistsTo, schema.ConfHostMatch},

		// A third-party API, synthesized as an external node.
		"checkout -> api.stripe.com": {schema.EdgeCalls, schema.ConfHostMatch},

		// Reached through a mounted ConfigMap, and priced for the extra
		// level of indirection.
		"api-gateway -> analytics.example.com": {schema.EdgeCalls, schema.ConfIndirect},
	}

	for label, expect := range want {
		e, ok := found[label]
		if !ok {
			t.Errorf("missing edge %s", label)
			continue
		}
		if e.Kind != expect.kind {
			t.Errorf("%s is %q, want %q", label, e.Kind, expect.kind)
		}
		if e.Confidence != expect.confidence {
			t.Errorf("%s has confidence %v, want %v", label, e.Confidence, expect.confidence)
		}
		if len(e.Evidence) == 0 {
			t.Errorf("%s carries no evidence", label)
		}
	}
	if len(found) != len(want) {
		t.Errorf("got %d edges, want %d: %v", len(found), len(want), keysOf(found))
	}
}

// References that genuinely name nothing are reported rather than invented.
func TestUnresolvableReferencesAreReported(t *testing.T) {
	g := run(t, fixture("k8s-microservices")).Graph

	reported := map[string]bool{}
	for _, d := range g.Diagnostics {
		if d.Code == "unresolved_reference" {
			for _, name := range []string{"search-service", "user-service", "broker"} {
				if strings.Contains(d.Message, name) {
					reported[name] = true
				}
			}
		}
	}
	for _, name := range []string{"search-service", "user-service", "broker"} {
		if !reported[name] {
			t.Errorf("%q matches no component and was dropped without a diagnostic", name)
		}
	}
}

// The multi-environment fixture is the ambiguity case: three namespaces
// declare the same names, so picking one would be wrong two times in three.
func TestAmbiguityAcrossEnvironmentsIsRefused(t *testing.T) {
	g := run(t, fixture("multi-environment")).Graph

	for _, e := range g.Edges {
		if e.Kind != schema.EdgeContains {
			t.Errorf("an ambiguous reference produced edge %s -> %s", e.From, e.To)
		}
	}
	found := false
	for _, d := range g.Diagnostics {
		if d.Code == "ambiguous_reference" {
			found = true
			for _, env := range []string{"dev", "staging", "prod"} {
				if !strings.Contains(d.Message, "db."+env) {
					t.Errorf("the diagnostic does not name db.%s: %s", env, d.Message)
				}
			}
		}
	}
	if !found {
		t.Error("the ambiguous reference was not reported")
	}
}

func keysOf(m map[string]schema.Edge) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
