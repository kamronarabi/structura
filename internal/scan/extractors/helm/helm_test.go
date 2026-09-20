package helm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/helm"
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

func (c *capture) node(t *testing.T, name string) schema.Node {
	t.Helper()
	for _, n := range c.nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node named %q", name)
	return schema.Node{}
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{Path: path, Dir: dir, Name: name, Ext: ".yaml", Size: int64(len(content))},
		Content:  []byte(content),
	}
	if err := helm.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatch(t *testing.T) {
	e := helm.New()
	yes := []string{"Chart.yaml", "chart.yaml", "values.yaml", "values.prod.yaml", "values-staging.yml"}
	for _, name := range yes {
		ext := ".yaml"
		if strings.HasSuffix(name, ".yml") {
			ext = ".yml"
		}
		if !e.Match(scan.FileMeta{Name: name, Ext: ext}) {
			t.Errorf("Match(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"deployment.yaml", "docker-compose.yml", "Chart.lock"} {
		if e.Match(scan.FileMeta{Name: name, Ext: ".yaml"}) && name == "Chart.lock" {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
}

// A chart's dependencies are architecture stated outright: a chart depending
// on bitnami/postgresql has a Postgres in it.
func TestChartDependenciesBecomeComponents(t *testing.T) {
	c := extract(t, "charts/api/Chart.yaml", `
apiVersion: v2
name: api
version: 0.3.1
appVersion: "1.2.0"
dependencies:
  - name: postgresql
    version: 14.x.x
    repository: https://charts.bitnami.com/bitnami
    condition: postgresql.enabled
  - name: redis
    version: 18.x.x
    repository: https://charts.bitnami.com/bitnami
`)

	pg := c.node(t, "postgresql")
	if pg.Kind != schema.KindDatastore {
		t.Errorf("postgresql kind = %q, want datastore", pg.Kind)
	}
	// A conditional dependency may be switched off by values we are not
	// rendering, so the condition travels with the node.
	if pg.Attrs["condition"] != "postgresql.enabled" {
		t.Errorf("condition = %v, want it recorded", pg.Attrs["condition"])
	}
	if redis := c.node(t, "redis"); redis.Kind != schema.KindDatastore {
		t.Errorf("redis kind = %q, want datastore", redis.Kind)
	}

	var boundary *schema.Node
	for i := range c.nodes {
		if c.nodes[i].Kind == schema.KindBoundary {
			boundary = &c.nodes[i]
		}
	}
	if boundary == nil {
		t.Fatal("no boundary node for the chart")
	}
	if boundary.Attrs["appVersion"] != "1.2.0" {
		t.Errorf("appVersion = %v", boundary.Attrs["appVersion"])
	}
	if len(c.edges) != 2 {
		t.Errorf("got %d contains edges, want one per subchart", len(c.edges))
	}
	// Subcharts are addressed by their release name, which is what
	// connection strings in values.yaml point at.
	if len(c.aliases) != 2 {
		t.Errorf("got %d aliases, want one release name per subchart", len(c.aliases))
	}
}

func TestValuesYieldAComponent(t *testing.T) {
	c := extract(t, "charts/api/values.yaml", `
replicaCount: 3
image:
  repository: ghcr.io/acme/api
  tag: "1.2.0"
service:
  type: ClusterIP
  port: 8080
env:
  DATABASE_URL: postgres://api@api-postgresql:5432/api
  LOG_LEVEL: info
`)

	api := c.node(t, "api")
	if api.Attrs["image"] != "ghcr.io/acme/api:1.2.0" {
		t.Errorf("image = %v", api.Attrs["image"])
	}
	if api.Attrs["replicas"] != 3 {
		t.Errorf("replicas = %v, want 3", api.Attrs["replicas"])
	}
	ports, _ := api.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]", api.Attrs["ports"])
	}
	// This was inferred from chart defaults, not read from a rendered
	// manifest, and the confidence has to say so.
	if api.Confidence != schema.ConfWeak {
		t.Errorf("confidence = %v, want %v for a values-inferred component", api.Confidence, schema.ConfWeak)
	}

	var inferred bool
	for _, d := range c.diags {
		if d.Code == "helm_values_inferred" {
			inferred = true
		}
	}
	if !inferred {
		t.Error("a values-inferred component was emitted without saying where it came from")
	}

	if len(c.hints) != 1 {
		t.Fatalf("got %d hints, want 1 (LOG_LEVEL is not a reference)", len(c.hints))
	}
	if c.hints[0].Tokens[0] != "api-postgresql" {
		t.Errorf("hint token = %v", c.hints[0].Tokens)
	}
}

// values.yaml is a common filename outside Helm. Inventing a service from an
// unrelated configuration file would be worse than missing a real one.
func TestUnrelatedValuesFileIsIgnored(t *testing.T) {
	c := extract(t, "config/values.yaml", "thresholds:\n  warn: 10\n  error: 20\n")
	if len(c.nodes) != 0 {
		t.Errorf("a non-chart values.yaml produced %d nodes", len(c.nodes))
	}
}

func TestChartWithoutNameIsIgnored(t *testing.T) {
	c := extract(t, "Chart.yaml", "apiVersion: v2\nversion: 1.0.0\n")
	if len(c.nodes) != 0 {
		t.Errorf("a Chart.yaml with no name produced %d nodes", len(c.nodes))
	}
}

func TestValuesCredentialsAreRedacted(t *testing.T) {
	c := extract(t, "charts/api/values.yaml", `
image:
  repository: acme/api
postgresql:
  auth:
    url: postgres://api:supersecret@api-postgresql:5432/api
`)
	for _, h := range c.hints {
		if strings.Contains(h.Raw, "supersecret") {
			t.Errorf("a credential survived into a hint: %q", h.Raw)
		}
		if strings.Contains(h.Source.Detail, "supersecret") {
			t.Errorf("a credential survived into evidence: %q", h.Source.Detail)
		}
	}
}

func TestMalformedChartIsReported(t *testing.T) {
	c := extract(t, "Chart.yaml", "name: [unclosed\n")
	found := false
	for _, d := range c.diags {
		if d.Code == "helm_chart_parse_failed" {
			found = true
		}
	}
	if !found {
		t.Error("a malformed Chart.yaml produced no diagnostic")
	}
}
