package k8s_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/k8s"
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
	t.Fatalf("no node named %q; got %d nodes", name, len(c.nodes))
	return schema.Node{}
}

func (c *capture) alias(t *testing.T, name string) resolve.Alias {
	t.Helper()
	for _, a := range c.aliases {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no alias named %q", name)
	return resolve.Alias{}
}

func (c *capture) hasDiag(code string) bool {
	for _, d := range c.diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	ext := ".yaml"
	if strings.HasSuffix(name, ".yml") {
		ext = ".yml"
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{Path: path, Dir: dir, Name: name, Ext: ext, Size: int64(len(content))},
		Content:  []byte(content),
	}
	if err := k8s.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatchClaimsYAMLButNotCompose(t *testing.T) {
	e := k8s.New()
	// Match cannot open the file, so every YAML is claimed and Extract
	// returns quietly for the ones that turn out not to be manifests.
	for _, name := range []string{"deployment.yaml", "svc.yml", "anything.yaml"} {
		ext := ".yaml"
		if strings.HasSuffix(name, ".yml") {
			ext = ".yml"
		}
		if !e.Match(scan.FileMeta{Name: name, Ext: ext}) {
			t.Errorf("Match(%q) = false, want true", name)
		}
	}
	// Compose files are another extractor's; claiming them would produce a
	// second, wrong reading of the same file.
	for _, name := range []string{"docker-compose.yml", "compose.yaml", "docker-compose.prod.yml"} {
		ext := ".yaml"
		if strings.HasSuffix(name, ".yml") {
			ext = ".yml"
		}
		if e.Match(scan.FileMeta{Name: name, Ext: ext}) {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
	for _, name := range []string{"main.go", "README.md", "config.json"} {
		if e.Match(scan.FileMeta{Name: name, Ext: name[strings.LastIndex(name, "."):]}) {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
}

func TestNonManifestYAMLIsSilentlyIgnored(t *testing.T) {
	c := extract(t, "config.yaml", "foo: bar\nlist:\n  - 1\n  - 2\n")
	if len(c.nodes) != 0 || len(c.diags) != 0 {
		t.Errorf("ordinary YAML produced %d nodes and %d diagnostics; most YAML in a repository is not a manifest",
			len(c.nodes), len(c.diags))
	}
}

func TestDeployment(t *testing.T) {
	c := extract(t, "deploy/api.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  replicas: 3
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
        tier: backend
    spec:
      serviceAccountName: api-sa
      containers:
        - name: api
          image: ghcr.io/acme/api:1.0
          ports:
            - containerPort: 8080
`)

	api := c.node(t, "api")
	if api.Kind != schema.KindService {
		t.Errorf("kind = %q, want service", api.Kind)
	}
	if api.Namespace != "prod" {
		t.Errorf("namespace = %q, want prod", api.Namespace)
	}
	if api.ID != "service:k8s/prod/api" {
		t.Errorf("id = %q", api.ID)
	}
	if api.Attrs["replicas"] != 3 {
		t.Errorf("replicas = %v, want 3", api.Attrs["replicas"])
	}
	if api.Attrs["workload"] != "Deployment" {
		t.Errorf("workload = %v", api.Attrs["workload"])
	}
	if api.Attrs["serviceAccount"] != "api-sa" {
		t.Errorf("serviceAccount = %v", api.Attrs["serviceAccount"])
	}
	// The pod labels have to reach the resolver: they are what a Service
	// selector is joined against.
	labels, _ := api.Attrs["labels"].(map[string]string)
	if labels["app"] != "api" || labels["tier"] != "backend" {
		t.Errorf("labels = %v, want the pod template's labels", api.Attrs["labels"])
	}
	if api.Sources[0].Line == 0 {
		t.Error("source has no line number")
	}
}

func TestNamespaceDefaultsWhenUnset(t *testing.T) {
	c := extract(t, "api.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          image: acme/api
`)
	if ns := c.node(t, "api").Namespace; ns != k8s.DefaultNamespace {
		t.Errorf("namespace = %q, want %q", ns, k8s.DefaultNamespace)
	}
}

func TestWorkloadKindComesFromTheImages(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     schema.NodeKind
	}{
		{
			name:     "a postgres container makes it a datastore",
			manifest: workload("db", "postgres:16"),
			want:     schema.KindDatastore,
		},
		{
			name:     "a rabbitmq container makes it a queue",
			manifest: workload("broker", "rabbitmq:3"),
			want:     schema.KindQueue,
		},
		{
			name:     "a first-party image is a service",
			manifest: workload("api", "ghcr.io/acme/api:1.0"),
			want:     schema.KindService,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := extract(t, "x.yaml", tt.manifest)
			if got := c.nodes[0].Kind; got != tt.want {
				t.Errorf("kind = %q, want %q", got, tt.want)
			}
		})
	}
}

// Sidecars are near-universal, and a pod with a Postgres container in it is a
// database however many log shippers ride alongside.
func TestSidecarsDoNotObscureTheWorkloadKind(t *testing.T) {
	c := extract(t, "db.yaml", `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  serviceName: db
  template:
    spec:
      containers:
        - {name: exporter, image: prom/postgres-exporter:0.15}
        - {name: postgres, image: postgres:16}
        - {name: fluentbit, image: fluent/fluent-bit:3.0}
`)
	if got := c.node(t, "db").Kind; got != schema.KindDatastore {
		t.Errorf("kind = %q, want datastore", got)
	}
}

func TestAllControllerKinds(t *testing.T) {
	kinds := map[string]string{
		"Deployment":  "spec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: acme/x}\n",
		"StatefulSet": "spec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: acme/x}\n",
		"DaemonSet":   "spec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: acme/x}\n",
		"Job":         "spec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: acme/x}\n",
		"CronJob":     "spec:\n  schedule: \"0 * * * *\"\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - {name: c, image: acme/x}\n",
		"Pod":         "spec:\n  containers:\n    - {name: c, image: acme/x}\n",
	}
	for kind, spec := range kinds {
		t.Run(kind, func(t *testing.T) {
			c := extract(t, "x.yaml", "apiVersion: apps/v1\nkind: "+kind+"\nmetadata: {name: thing}\n"+spec)
			n := c.node(t, "thing")
			if n.Attrs["workload"] != kind {
				t.Errorf("workload = %v, want %q", n.Attrs["workload"], kind)
			}
			if n.Attrs["image"] != "acme/x" {
				t.Errorf("image = %v; the containers were not found at this kind's nesting depth", n.Attrs["image"])
			}
		})
	}
}

// A Service is a DNS name for a workload, not a component of its own.
// Modeling it as a node would double the node count and turn every dependency
// into a two-hop path.
func TestServiceBecomesAnAliasNotANode(t *testing.T) {
	c := extract(t, "svc.yaml", `
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: prod
spec:
  selector:
    app: api
  ports:
    - port: 8080
    - port: 9090
`)

	if len(c.nodes) != 0 {
		t.Errorf("a Service produced %d nodes; it should contribute an alias only", len(c.nodes))
	}
	a := c.alias(t, "api")
	if a.Selector["app"] != "api" {
		t.Errorf("selector = %v", a.Selector)
	}
	if len(a.Ports) != 2 || a.Ports[0] != 8080 || a.Ports[1] != 9090 {
		t.Errorf("ports = %v, want [8080 9090]", a.Ports)
	}
	// Every form is emitted because which one a repository writes down
	// varies by file.
	want := []string{"api.prod.svc.cluster.local", "api.prod.svc", "api.prod", "api"}
	if strings.Join(a.DNS, ",") != strings.Join(want, ",") {
		t.Errorf("DNS = %v, want %v", a.DNS, want)
	}
}

func TestExternalNameServiceIsRecorded(t *testing.T) {
	c := extract(t, "svc.yaml", `
apiVersion: v1
kind: Service
metadata: {name: warehouse, namespace: prod}
spec:
  type: ExternalName
  externalName: warehouse.legacy.internal
`)
	a := c.alias(t, "warehouse")
	if a.External != "warehouse.legacy.internal" {
		t.Errorf("External = %q; a CNAME out of the cluster is a real architectural fact", a.External)
	}
}

func TestSelectorlessServiceIsReported(t *testing.T) {
	c := extract(t, "svc.yaml", `
apiVersion: v1
kind: Service
metadata: {name: external-db, namespace: prod}
spec:
  ports: [{port: 5432}]
`)
	// Its backend is manually managed Endpoints, which are not in the
	// repository; a silent gap would be worse than saying so.
	if !c.hasDiag("k8s_service_without_selector") {
		t.Error("a Service with no selector was not reported")
	}
}

func TestIngressRoutesBecomeHints(t *testing.T) {
	c := extract(t, "ing.yaml", `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: public, namespace: prod}
spec:
  ingressClassName: nginx
  tls:
    - hosts: [shop.example.com]
  rules:
    - host: shop.example.com
      http:
        paths:
          - path: /api
            backend: {service: {name: api, port: {number: 8080}}}
          - path: /checkout
            backend: {service: {name: checkout, port: {number: 8080}}}
`)

	ing := c.node(t, "public")
	if ing.Kind != schema.KindCloudResource {
		t.Errorf("kind = %q, want cloud_resource", ing.Kind)
	}
	if ing.Attrs["tls"] != true {
		t.Errorf("tls = %v, want true", ing.Attrs["tls"])
	}
	if len(c.hints) != 2 {
		t.Fatalf("got %d hints, want one per backend", len(c.hints))
	}
	for _, h := range c.hints {
		if h.SuggestedEdge != schema.EdgeExposes {
			t.Errorf("suggested edge = %q, want exposes", h.SuggestedEdge)
		}
		if h.Kind != resolve.HintSelector {
			t.Errorf("hint kind = %q, want selector", h.Kind)
		}
	}
}

// Plenty of committed manifests still use the v1beta1 backend spelling.
func TestIngressBetaBackendSpelling(t *testing.T) {
	c := extract(t, "ing.yaml", `
apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata: {name: old, namespace: prod}
spec:
  rules:
    - http:
        paths:
          - path: /
            backend:
              serviceName: legacy-api
              servicePort: 80
`)
	if len(c.hints) != 1 {
		t.Fatalf("got %d hints, want 1", len(c.hints))
	}
	if c.hints[0].Raw != "legacy-api" {
		t.Errorf("backend = %q, want legacy-api", c.hints[0].Raw)
	}
}

func TestEnvironmentReferencesBecomeHints(t *testing.T) {
	c := extract(t, "api.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: prod}
spec:
  template:
    spec:
      containers:
        - name: api
          image: acme/api
          env:
            - name: DATABASE_URL
              value: postgres://app:s3cr3t@db:5432/orders
            - name: LOG_LEVEL
              value: debug
            - name: JWT_KEY
              valueFrom:
                secretKeyRef: {name: secrets, key: jwt}
            - name: ENDPOINTS_FROM_CONFIG
              valueFrom:
                configMapKeyRef: {name: endpoints, key: search}
`)

	var connString, configUse int
	for _, h := range c.hints {
		switch h.Kind {
		case resolve.HintConnString:
			connString++
			if strings.Contains(h.Raw, "s3cr3t") {
				t.Errorf("a password survived into a hint: %q", h.Raw)
			}
		case resolve.HintConfigValue:
			configUse++
		}
	}
	if connString != 1 {
		t.Errorf("got %d connection-string hints, want 1", connString)
	}
	// A Secret's contents are not in the repository and must not be; a
	// ConfigMap's are, so only the ConfigMap reference is followed.
	if configUse != 1 {
		t.Errorf("got %d config-map hints, want 1 (the Secret reference must not produce one)", configUse)
	}
}

func TestConfigMapValuesAreAttachedForLaterJoining(t *testing.T) {
	c := extract(t, "cm.yaml", `
apiVersion: v1
kind: ConfigMap
metadata: {name: endpoints, namespace: prod}
data:
  SEARCH_HOST: search-service
  ANALYTICS_URL: https://analytics.example.com/ingest
  FEATURE_FLAGS: "a,b,c"
`)

	if len(c.hints) != 2 {
		t.Fatalf("got %d hints, want 2 (the feature-flag list is not a reference)", len(c.hints))
	}
	want := k8s.ConfigMapRef("prod", "endpoints")
	for _, h := range c.hints {
		if h.FromNode != want {
			t.Errorf("hint attached to %q, want the synthetic ConfigMap identifier %q", h.FromNode, want)
		}
		if h.Kind != resolve.HintConfigValue {
			t.Errorf("hint kind = %q, want config_value", h.Kind)
		}
	}
}

func TestStatefulSetGoverningServiceIsAnAlias(t *testing.T) {
	c := extract(t, "db.yaml", `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: orders-db, namespace: prod}
spec:
  serviceName: orders-db-headless
  template:
    spec:
      containers:
        - {name: pg, image: postgres:16}
`)
	a := c.alias(t, "orders-db-headless")
	if a.TargetName != "orders-db" {
		t.Errorf("TargetName = %q, want orders-db", a.TargetName)
	}
}

func TestMultiDocumentFile(t *testing.T) {
	c := extract(t, "all.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: prod}
spec:
  template:
    spec:
      containers: [{name: api, image: acme/api}]
---
apiVersion: v1
kind: Service
metadata: {name: api, namespace: prod}
spec:
  selector: {app: api}
  ports: [{port: 8080}]
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: prod}
spec:
  serviceName: db
  template:
    spec:
      containers: [{name: pg, image: postgres:16}]
`)
	if len(c.nodes) != 2 {
		t.Errorf("got %d nodes, want 2 (the Service is an alias)", len(c.nodes))
	}
	if len(c.aliases) != 2 {
		t.Errorf("got %d aliases, want 2", len(c.aliases))
	}
}

// Kustomize overlays are full of files that name a Deployment purely to
// override its replica count. Reading one as a component invents a service
// with no image, once per environment.
func TestPatchFragmentsAreNotComponents(t *testing.T) {
	c := extract(t, "overlays/prod/replicas.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: prod}
spec:
  replicas: 6
`)
	if len(c.nodes) != 0 {
		t.Errorf("a patch fragment produced %d nodes, want 0", len(c.nodes))
	}
	if !c.hasDiag("k8s_patch_fragment") {
		t.Error("the patch fragment was skipped without saying so")
	}
}

func TestHelmTemplateIsReportedNotParsed(t *testing.T) {
	c := extract(t, "charts/api/templates/deployment.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "api.fullname" . }}
spec:
  replicas: {{ .Values.replicaCount }}
`)
	if len(c.nodes) != 0 {
		t.Errorf("an unrendered Helm template produced %d nodes", len(c.nodes))
	}
	if !c.hasDiag(k8s.DiagHelmUnrendered) {
		t.Error("an unrendered Helm template produced no diagnostic; the user would see a sparse graph with no explanation")
	}
	for _, d := range c.diags {
		if d.Code == k8s.DiagHelmUnrendered && d.Path != "charts/api/templates" {
			t.Errorf("diagnostic path = %q, want the chart's templates directory", d.Path)
		}
	}
}

func TestKustomizationIsReported(t *testing.T) {
	// Many committed kustomization.yaml files omit apiVersion and kind, so
	// recognizing them by name is the only reliable route.
	c := extract(t, "overlays/prod/kustomization.yaml", "resources:\n  - ../../base\nnamespace: prod\n")
	if !c.hasDiag("kustomize_unrendered") {
		t.Error("a Kustomization was not reported")
	}
}

func TestMalformedDocumentIsReported(t *testing.T) {
	c := extract(t, "bad.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: [unclosed\n")
	if !c.hasDiag("k8s_parse_failed") {
		t.Errorf("a malformed manifest produced no diagnostic; diags: %v", c.diags)
	}
}

func workload(name, image string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: " + name + "}\n" +
		"spec:\n  template:\n    spec:\n      containers:\n        - {name: c, image: " + image + "}\n"
}
