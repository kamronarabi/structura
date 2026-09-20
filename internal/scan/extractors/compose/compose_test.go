package compose_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/compose"
	"github.com/kamronarabi/structura/pkg/schema"
)

// capture collects one extractor run.
type capture struct {
	nodes   []schema.Node
	edges   []schema.Edge
	hints   []resolve.Hint
	aliases []resolve.Alias
	diags   []schema.Diagnostic
}

func (c *capture) Node(n schema.Node)       { c.nodes = append(c.nodes, n) }
func (c *capture) Edge(e schema.Edge)       { c.edges = append(c.edges, e) }
func (c *capture) Hint(h resolve.Hint)      { c.hints = append(c.hints, h) }
func (c *capture) Alias(a resolve.Alias)    { c.aliases = append(c.aliases, a) }
func (c *capture) Diag(d schema.Diagnostic) { c.diags = append(c.diags, d) }

func (c *capture) node(t *testing.T, name string) schema.Node {
	t.Helper()
	for _, n := range c.nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node named %q; got %v", name, c.names())
	return schema.Node{}
}

func (c *capture) names() []string {
	out := make([]string, len(c.nodes))
	for i, n := range c.nodes {
		out[i] = n.Name
	}
	return out
}

func (c *capture) hasEdge(from, to string, kind schema.EdgeKind) bool {
	for _, e := range c.edges {
		if strings.HasSuffix(e.From, "/"+from) && strings.HasSuffix(e.To, "/"+to) && e.Kind == kind {
			return true
		}
	}
	return false
}

func (c *capture) hintsFor(service string) []resolve.Hint {
	var out []resolve.Hint
	for _, h := range c.hints {
		if strings.HasSuffix(h.FromNode, "/"+service) {
			out = append(out, h)
		}
	}
	return out
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	f := &scan.File{
		FileMeta: scan.FileMeta{
			Path: path, Dir: dirOf(path), Name: baseOf(path),
			Ext: ".yml", Size: int64(len(content)),
		},
		Content: []byte(content),
	}
	if err := compose.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

func baseOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func TestMatch(t *testing.T) {
	e := compose.New()
	yes := []string{
		"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
		"docker-compose.prod.yml", "compose.override.yaml", "DOCKER-COMPOSE.YML",
	}
	for _, name := range yes {
		ext := ".yml"
		if strings.HasSuffix(strings.ToLower(name), ".yaml") {
			ext = ".yaml"
		}
		if !e.Match(scan.FileMeta{Name: name, Ext: ext}) {
			t.Errorf("Match(%q) = false, want true", name)
		}
	}
	no := []string{"deployment.yaml", "values.yaml", "compose.txt", "README.md", "composer.json"}
	for _, name := range no {
		ext := ""
		if i := strings.LastIndex(name, "."); i >= 0 {
			ext = name[i:]
		}
		if e.Match(scan.FileMeta{Name: name, Ext: ext}) {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
}

func TestServiceClassification(t *testing.T) {
	c := extract(t, "docker-compose.yml", `
name: shop
services:
  api:
    image: ghcr.io/acme/api:1.0
    ports: ["8080:8080"]
  db:
    image: postgres:16
  cache:
    image: redis:7-alpine
  broker:
    image: rabbitmq:3-management
  gateway:
    image: nginx:1.27
  builder:
    build: ./builder
    image: postgres:16
`)

	want := map[string]schema.NodeKind{
		"api":     schema.KindService,
		"db":      schema.KindDatastore,
		"cache":   schema.KindDatastore,
		"broker":  schema.KindQueue,
		"gateway": schema.KindCloudResource,
		// A build stanza means the image is first-party, whatever the name
		// it happens to be tagged with suggests.
		"builder": schema.KindService,
	}
	for name, kind := range want {
		if got := c.node(t, name).Kind; got != kind {
			t.Errorf("%s classified as %q, want %q", name, got, kind)
		}
	}
}

func TestDependsOnBecomesAnEdge(t *testing.T) {
	c := extract(t, "docker-compose.yml", `
name: shop
services:
  api:
    image: acme/api
    depends_on: [db, cache]
  db:
    image: postgres:16
  cache:
    image: redis:7
`)

	if !c.hasEdge("api", "db", schema.EdgeDependsOn) {
		t.Error("api -> db depends_on edge is missing")
	}
	if !c.hasEdge("api", "cache", schema.EdgeDependsOn) {
		t.Error("api -> cache depends_on edge is missing")
	}
	for _, e := range c.edges {
		if e.Kind == schema.EdgeDependsOn && e.Confidence != schema.ConfDeclared {
			t.Errorf("a declared dependency has confidence %v, want %v", e.Confidence, schema.ConfDeclared)
		}
		if e.Kind == schema.EdgeDependsOn && len(e.Evidence) == 0 {
			t.Error("a depends_on edge carries no evidence")
		}
	}
}

// An extractor may only emit an edge it can justify from this file alone.
// A depends_on naming a service defined in another Compose file becomes a
// hint, because emitting the edge would point it at a node that does not
// exist — which the builder rejects, failing the entire scan.
func TestDependsOnOutsideThisFileBecomesAHint(t *testing.T) {
	c := extract(t, "docker-compose.override.yml", `
name: shop
services:
  api:
    image: acme/api
    depends_on: [db-defined-elsewhere]
`)

	for _, e := range c.edges {
		if e.Kind == schema.EdgeDependsOn {
			t.Errorf("emitted a depends_on edge to a node this file never declared: %s", e.To)
		}
	}
	found := false
	for _, h := range c.hints {
		if h.Kind == resolve.HintDependsOn && h.Tokens[0] == "db-defined-elsewhere" {
			found = true
		}
	}
	if !found {
		t.Errorf("the unresolvable dependency was dropped instead of becoming a hint; hints: %v", c.hints)
	}
}

func TestEnvironmentReferencesBecomeHints(t *testing.T) {
	c := extract(t, "docker-compose.yml", `
name: shop
services:
  api:
    image: acme/api
    environment:
      DATABASE_URL: postgres://app:pw@db:5432/orders
      LOG_LEVEL: debug
      APP_ENV: production
      STRIPE_URL: https://api.stripe.com
`)

	hints := c.hintsFor("api")
	if len(hints) != 2 {
		t.Fatalf("got %d hints, want 2 (the database and the external API, not the two settings): %v",
			len(hints), hints)
	}

	var sawDB, sawStripe bool
	for _, h := range hints {
		switch h.Tokens[0] {
		case "db":
			sawDB = true
			if h.SuggestedEdge != schema.EdgePersistsTo {
				t.Errorf("DATABASE_URL suggested %q, want persists_to", h.SuggestedEdge)
			}
			// The hint is redacted at the point it is created, not only at
			// serialization: --debug-dump prints these.
			if strings.Contains(h.Raw, "pw@") {
				t.Errorf("a password survived into a hint: %q", h.Raw)
			}
			if !strings.Contains(h.Source.Detail, resolve.Mask) {
				t.Errorf("evidence detail was not redacted: %q", h.Source.Detail)
			}
		case "api.stripe.com":
			sawStripe = true
		}
	}
	if !sawDB || !sawStripe {
		t.Errorf("expected hints for db and api.stripe.com, got %v", hints)
	}
}

// Interpolation runs against an empty environment, so a scan can never copy a
// value out of the developer's shell into a file that gets committed.
func TestInterpolationCannotReachTheProcessEnvironment(t *testing.T) {
	t.Setenv("DB_PASSWORD", "leaked-from-the-shell")
	t.Setenv("TAG", "leaked-tag")

	c := extract(t, "docker-compose.yml", `
name: shop
services:
  api:
    image: acme/api:${TAG:-1.0.0}
    environment:
      DATABASE_URL: postgres://app:${DB_PASSWORD}@db:5432/orders
`)

	api := c.node(t, "api")
	image, _ := api.Attrs["image"].(string)
	if strings.Contains(image, "leaked-tag") {
		t.Errorf("a shell variable reached the graph: image = %q", image)
	}
	// The declared default is still honored; only the shell is off limits.
	if image != "acme/api:1.0.0" {
		t.Errorf("image = %q, want the compose-declared default 1.0.0", image)
	}
	for _, h := range c.hints {
		if strings.Contains(h.Raw, "leaked-from-the-shell") {
			t.Errorf("a shell variable reached a hint: %q", h.Raw)
		}
	}
}

func TestAttributesAndProvenance(t *testing.T) {
	c := extract(t, "deploy/docker-compose.yml", `
name: shop
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - "8080:8080"
      - "9090:9090"
    deploy:
      replicas: 4
    restart: always
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./seed:/docker-entrypoint-initdb.d
volumes:
  pgdata: {}
`)

	api := c.node(t, "api")
	if api.Attrs["image"] != "ghcr.io/acme/api:1.2.3" {
		t.Errorf("image = %v", api.Attrs["image"])
	}
	// The container port is the service's own property; the published host
	// port is a local convenience that changes per environment.
	ports, _ := api.Attrs["ports"].([]int)
	if len(ports) != 2 || ports[0] != 8080 || ports[1] != 9090 {
		t.Errorf("ports = %v, want [8080 9090]", api.Attrs["ports"])
	}
	if api.Attrs["replicas"] != 4 {
		t.Errorf("replicas = %v, want 4", api.Attrs["replicas"])
	}

	db := c.node(t, "db")
	vols, _ := db.Attrs["volumes"].([]string)
	// A bind mount is a development convenience and says nothing about the
	// deployed architecture; a named volume means the service holds state.
	if len(vols) != 1 || vols[0] != "pgdata" {
		t.Errorf("volumes = %v, want only the named volume", db.Attrs["volumes"])
	}

	if len(api.Sources) != 1 {
		t.Fatalf("sources = %v, want exactly one", api.Sources)
	}
	src := api.Sources[0]
	if src.Path != "deploy/docker-compose.yml" || src.Extractor != compose.Name {
		t.Errorf("source = %+v", src)
	}
	if src.Line == 0 {
		t.Error("source has no line number; evidence must point at a location")
	}
}

func TestProjectBoundaryContainsEveryService(t *testing.T) {
	c := extract(t, "docker-compose.yml", `
name: shop
services:
  api: {image: acme/api}
  db: {image: postgres:16}
`)

	var boundary *schema.Node
	for i := range c.nodes {
		if c.nodes[i].Kind == schema.KindBoundary {
			boundary = &c.nodes[i]
		}
	}
	if boundary == nil {
		t.Fatal("no boundary node was emitted for the Compose project")
	}
	if boundary.Name != "shop" {
		t.Errorf("boundary name = %q, want the project name", boundary.Name)
	}
	contains := 0
	for _, e := range c.edges {
		if e.Kind == schema.EdgeContains && e.From == boundary.ID {
			contains++
		}
	}
	if contains != 2 {
		t.Errorf("boundary contains %d services, want 2", contains)
	}
}

func TestProjectNameFallsBackToTheDirectory(t *testing.T) {
	c := extract(t, "services/checkout/docker-compose.yml", `
services:
  api: {image: acme/api}
`)
	api := c.node(t, "api")
	if api.Namespace != "checkout" {
		t.Errorf("namespace = %q, want the directory name when no project name is declared", api.Namespace)
	}
}

// A file that will not parse is a reported gap, never a failed scan: the
// other nine hundred files in the repository were understood fine.
func TestMalformedFileBecomesADiagnostic(t *testing.T) {
	for _, content := range []string{
		"services:\n  api:\n   image: [unclosed\n",
		"this is not yaml at all: [{",
		"",
		"services: \"a string where a mapping belongs\"\n",
	} {
		c := extract(t, "docker-compose.yml", content)
		if len(c.diags) == 0 && len(c.nodes) == 0 {
			t.Errorf("input %q produced neither nodes nor a diagnostic", content)
		}
		for _, d := range c.diags {
			if d.Path != "docker-compose.yml" {
				t.Errorf("diagnostic does not name the file: %+v", d)
			}
		}
	}
}
