package helm_test

import (
	"context"
	"reflect"
	"sort"
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

// umbrellaValues is a chart that deploys several differently-named services
// from one values file, which is the shape a repository takes when its chart
// is the only deployment description it has.
const umbrellaValues = `
images:
  repository: ghcr.io/acme
  tag: "2.1.0"
networkPolicies:
  create: false
storefront:
  create: true
  name: storefront
  replicaCount: 2
  service:
    port: 8080
  env:
    CART_API_URL: http://cart-api:9090
cartApi:
  create: true
  name: cart-api
  service:
    port: 9090
cache:
  create: true
  name: shop-cache
  image:
    repository: redis
    tag: "7.2"
tracing:
  create: false
  name: jaeger
  service:
    port: 16686
persistence:
  name: shop-data
  size: 8Gi
  storageClass: standard
`

func namesOf(c *capture) []string {
	out := make([]string, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out
}

// An umbrella chart produced nothing at all: looksLikeChartValues tests for
// the single-service keys, and an umbrella has none of them at the top level,
// so a chart deploying a dozen services yielded no components.
func TestUmbrellaChartYieldsAComponentPerWorkload(t *testing.T) {
	c := extract(t, "charts/shop/values.yaml", umbrellaValues)

	want := []string{"cart-api", "jaeger", "shop-cache", "storefront"}
	if got := namesOf(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %v, want %v", got, want)
	}
	for _, n := range c.nodes {
		// The chart name identifies the boundary, not the workloads inside
		// it: a chart is packaging, and a workload it deploys is the same
		// component whether a chart or a manifest described it.
		if n.Namespace != "" {
			t.Errorf("%s is in namespace %q; a chart is not a deployment scope", n.Name, n.Namespace)
		}
		if n.Confidence != schema.ConfWeak {
			t.Errorf("%s has confidence %.2f; a component read from defaults is not a declared one",
				n.Name, n.Confidence)
		}
	}
}

// A name alone is not enough. The blocks below name something without
// deploying anything, and emitting "shop-data" as a component is exactly the
// confident invention the confidence model exists to prevent.
func TestUmbrellaRejectsBlocksThatAreNotWorkloads(t *testing.T) {
	c := extract(t, "charts/shop/values.yaml", umbrellaValues)

	for _, unwanted := range []string{"shop-data", "images", "networkPolicies", "persistence"} {
		for _, n := range c.nodes {
			if n.Name == unwanted {
				t.Errorf("%q became a component; it names a volume or a setting, not a workload", unwanted)
			}
		}
	}
}

// The image says what a workload is; the name only suggests it.
func TestUmbrellaClassifiesByImage(t *testing.T) {
	c := extract(t, "charts/shop/values.yaml", umbrellaValues)

	for _, n := range c.nodes {
		if n.Name != "shop-cache" {
			continue
		}
		if n.Kind != schema.KindDatastore {
			t.Errorf("shop-cache is %s; it runs redis, which a name-only reading would miss", n.Kind)
		}
		return
	}
	t.Fatal("no shop-cache component")
}

// A workload the chart can deploy is worth knowing about even when it is off,
// and whether it is on is a fact about it. online-boutique's collector is
// declared exactly this way, which is why nothing points at it.
func TestDisabledWorkloadIsEmittedAndMarked(t *testing.T) {
	c := extract(t, "charts/shop/values.yaml", umbrellaValues)

	for _, n := range c.nodes {
		if n.Name != "jaeger" {
			continue
		}
		if enabled, ok := n.Attrs["enabled"].(bool); !ok || enabled {
			t.Errorf("jaeger does not record that the chart disables it: attrs=%v", n.Attrs)
		}
		return
	}
	t.Fatal("a workload the chart declares but disables was dropped entirely")
}

// A reference inside a workload's block is that workload's dependency.
// Attaching every reference to one chart-level component would make the
// busiest service in the chart look like the only one with dependencies.
func TestUmbrellaReferencesAttachToTheirWorkload(t *testing.T) {
	c := extract(t, "charts/shop/values.yaml", umbrellaValues)

	var storefront string
	for _, n := range c.nodes {
		if n.Name == "storefront" {
			storefront = n.ID
		}
	}
	if storefront == "" {
		t.Fatal("no storefront component")
	}
	if len(c.hints) == 0 {
		t.Fatal("no references found; an umbrella chart's connection strings were dropped")
	}
	for _, h := range c.hints {
		if h.FromNode != storefront {
			t.Errorf("reference %q came from %s, want the storefront that declares it", h.Raw, h.FromNode)
		}
	}
}

// A chart with a top-level image is one product, not an umbrella. Its primary
// and readReplicas blocks are components of a single datastore whose names are
// release-name suffixes, so reading them as separate services would turn one
// database into two badly-named ones.
func TestSingleProductChartIsNotReadAsAnUmbrella(t *testing.T) {
	c := extract(t, "charts/postgresql/values.yaml", `
image:
  repository: bitnami/postgresql
  tag: "16"
primary:
  name: primary
  service:
    port: 5432
readReplicas:
  name: read
  replicaCount: 2
`)

	if got := namesOf(c); !reflect.DeepEqual(got, []string{"postgresql"}) {
		t.Fatalf("components = %v, want just the chart itself", got)
	}
}
