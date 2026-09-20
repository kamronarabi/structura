package manifest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/manifest"
	"github.com/kamronarabi/structura/pkg/schema"
)

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

func (c *capture) tokens() []string {
	var out []string
	for _, h := range c.hints {
		out = append(out, h.Tokens...)
	}
	return out
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i:]
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{Path: path, Dir: dir, Name: name, Ext: ext, Size: int64(len(content))},
		Content:  []byte(content),
	}
	if err := manifest.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatch(t *testing.T) {
	e := manifest.New()
	yes := []struct{ name, ext string }{
		{"go.mod", ".mod"}, {"package.json", ".json"},
		{"requirements.txt", ".txt"}, {"requirements-dev.txt", ".txt"},
		{"pyproject.toml", ".toml"},
	}
	for _, tt := range yes {
		if !e.Match(scan.FileMeta{Name: tt.name, Ext: tt.ext}) {
			t.Errorf("Match(%q) = false, want true", tt.name)
		}
	}
	no := []struct{ name, ext string }{
		{"go.sum", ".sum"}, {"package-lock.json", ".json"},
		{"Cargo.toml", ".toml"}, {"README.md", ".md"},
	}
	for _, tt := range no {
		if e.Match(scan.FileMeta{Name: tt.name, Ext: tt.ext}) {
			t.Errorf("Match(%q) = true, want false", tt.name)
		}
	}
}

func TestGoMod(t *testing.T) {
	c := extract(t, "services/gateway/go.mod", `
module github.com/acme/shop/gateway

go 1.25

require (
	github.com/gin-gonic/gin v1.10.0
	github.com/redis/go-redis/v9 v9.7.0
	github.com/stripe/stripe-go/v79 v79.12.0
)

require github.com/bytedance/sonic v1.12.0 // indirect
`)

	if len(c.nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(c.nodes))
	}
	n := c.nodes[0]
	if n.Name != "github.com/acme/shop/gateway" {
		t.Errorf("name = %q", n.Name)
	}
	if n.Tech.Language != "go" {
		t.Errorf("language = %q", n.Tech.Language)
	}
	if n.Tech.Framework != "gin" {
		t.Errorf("framework = %q, want gin", n.Tech.Framework)
	}
	// The directory is what a Compose build context points at, which is how
	// the resolver connects this to the container that runs it.
	if n.Attrs["directory"] != "services/gateway" {
		t.Errorf("directory = %v", n.Attrs["directory"])
	}
	// Indirect requirements are the transitive closure, not choices this
	// codebase made.
	if n.Attrs["dependencyCount"] != 3 {
		t.Errorf("dependencyCount = %v, want 3 with the indirect one excluded", n.Attrs["dependencyCount"])
	}

	tokens := c.tokens()
	for _, want := range []string{"redis", "stripe"} {
		if !contains(tokens, want) {
			t.Errorf("no hint for %q; tokens = %v", want, tokens)
		}
	}
	// A web framework is not a component, so it labels the service rather
	// than producing a node of its own.
	if contains(tokens, "gin") {
		t.Errorf("gin produced an infrastructure hint; tokens = %v", tokens)
	}
}

func TestGoModAtRepositoryRootGetsAUsableID(t *testing.T) {
	c := extract(t, "go.mod", "module github.com/acme/thing\n\ngo 1.25\n")
	if len(c.nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(c.nodes))
	}
	// "." normalizes away to nothing, which would leave the node with a
	// meaningless identifier.
	if strings.HasSuffix(c.nodes[0].ID, "/unknown") {
		t.Errorf("id = %q; a root-level module needs a real identifier", c.nodes[0].ID)
	}
}

func TestPackageJSON(t *testing.T) {
	c := extract(t, "web/package.json", `{
  "name": "@acme/shop-web",
  "version": "3.1.0",
  "engines": {"node": ">=22"},
  "dependencies": {
    "next": "15.0.3",
    "react": "19.0.0",
    "ioredis": "5.4.1"
  },
  "devDependencies": {"typescript": "5.7.2", "vitest": "2.1.5"}
}`)

	n := c.nodes[0]
	if n.Name != "@acme/shop-web" {
		t.Errorf("name = %q", n.Name)
	}
	if n.Tech.Language != "typescript" {
		t.Errorf("language = %q, want typescript from the devDependency", n.Tech.Language)
	}
	if n.Tech.Framework != "nextjs" {
		t.Errorf("framework = %q, want nextjs", n.Tech.Framework)
	}
	// Dev dependencies are build and test tooling, not runtime architecture.
	if n.Attrs["dependencyCount"] != 3 {
		t.Errorf("dependencyCount = %v, want 3", n.Attrs["dependencyCount"])
	}
	if !contains(c.tokens(), "redis") {
		t.Errorf("ioredis produced no hint; tokens = %v", c.tokens())
	}
}

// Dependencies come out of a JSON map, whose iteration order is random.
// Without sorting, a package.json listing both next and react picks a
// different framework between runs.
func TestFrameworkChoiceIsDeterministic(t *testing.T) {
	src := `{"name":"w","dependencies":{"next":"1","react":"1","vue":"1","express":"1","koa":"1"}}`
	first := extract(t, "web/package.json", src).nodes[0].Tech.Framework
	for range 30 {
		if got := extract(t, "web/package.json", src).nodes[0].Tech.Framework; got != first {
			t.Fatalf("framework varied between runs: %q then %q", first, got)
		}
	}
}

func TestPyprojectPEP621(t *testing.T) {
	c := extract(t, "svc/pyproject.toml", `
[project]
name = "recommendations"
version = "0.4.0"
requires-python = ">=3.11"
dependencies = [
  "fastapi>=0.115",
  "psycopg[binary]>=3.2",
  "kafka-python>=2.0",
  "numpy>=2.0",
]
`)
	n := c.nodes[0]
	if n.Name != "recommendations" {
		t.Errorf("name = %q", n.Name)
	}
	if n.Tech.Framework != "fastapi" {
		t.Errorf("framework = %q, want fastapi", n.Tech.Framework)
	}
	tokens := c.tokens()
	for _, want := range []string{"postgres", "kafka"} {
		if !contains(tokens, want) {
			t.Errorf("no hint for %q; tokens = %v", want, tokens)
		}
	}
}

func TestPyprojectPoetry(t *testing.T) {
	c := extract(t, "svc/pyproject.toml", `
[tool.poetry]
name = "billing"
version = "1.0.0"

[tool.poetry.dependencies]
python = "^3.11"
django = "^5.0"
psycopg2 = "^2.9"
`)
	if c.nodes[0].Name != "billing" {
		t.Errorf("name = %q", c.nodes[0].Name)
	}
	if c.nodes[0].Tech.Framework != "django" {
		t.Errorf("framework = %q", c.nodes[0].Tech.Framework)
	}
}

func TestRequirementsTxt(t *testing.T) {
	c := extract(t, "worker/requirements.txt", `
# Runtime
celery[redis]==5.4.0
psycopg2-binary>=2.9,<3
boto3
requests ; python_version >= '3.9'
-r base.txt
--index-url https://pypi.org/simple

flask==3.0.*
`)
	if len(c.nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(c.nodes))
	}
	// requirements.txt names no package, so the directory is the only
	// identity available.
	if c.nodes[0].Name != "worker" {
		t.Errorf("name = %q, want the directory name", c.nodes[0].Name)
	}
	if c.nodes[0].Tech.Framework != "flask" {
		t.Errorf("framework = %q, want flask", c.nodes[0].Tech.Framework)
	}
	tokens := c.tokens()
	if !contains(tokens, "aws") {
		t.Errorf("boto3 produced no hint; tokens = %v", tokens)
	}
}

// A library hint says what kind of thing is on the other end, never which
// instance, so it can only ever corroborate an edge.
func TestLibraryHintsCarryTechnologyNotHostnames(t *testing.T) {
	c := extract(t, "go.mod", "module x\n\ngo 1.25\n\nrequire github.com/lib/pq v1.10.9\n")
	if len(c.hints) != 1 {
		t.Fatalf("got %d hints, want 1", len(c.hints))
	}
	h := c.hints[0]
	if h.Kind != resolve.HintLibrary {
		t.Errorf("kind = %q, want library", h.Kind)
	}
	if h.Kind.BaseConfidence() != schema.ConfWeak {
		t.Errorf("base confidence = %v, want %v", h.Kind.BaseConfidence(), schema.ConfWeak)
	}
	if h.Tokens[0] != "postgres" {
		t.Errorf("token = %q, want the technology name", h.Tokens[0])
	}
}

func TestDuplicateTechnologiesProduceOneHint(t *testing.T) {
	c := extract(t, "go.mod", `module x

go 1.25

require (
	github.com/redis/go-redis/v9 v9.7.0
	github.com/go-redis/redis/v8 v8.11.5
	github.com/gomodule/redigo v1.9.2
)
`)
	if len(c.hints) != 1 {
		t.Errorf("got %d hints for three Redis clients, want 1", len(c.hints))
	}
}

func TestMalformedManifestIsReported(t *testing.T) {
	c := extract(t, "package.json", "{not json")
	if len(c.diags) == 0 {
		t.Error("a malformed package.json produced no diagnostic")
	}
	if len(c.nodes) != 0 {
		t.Error("a malformed package.json produced nodes")
	}
}

func TestWorkspaceRootIsIgnored(t *testing.T) {
	c := extract(t, "package.json", `{"private": true, "workspaces": ["packages/*"]}`)
	if len(c.nodes) != 0 {
		t.Errorf("a nameless workspace root produced %d nodes", len(c.nodes))
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
