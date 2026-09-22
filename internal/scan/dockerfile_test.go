package scan_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// These check what only a whole scan can show: that reading a Dockerfile
// changes the graph in the ways it was added for. The extractor is tested on
// its own, and the resolver fold is tested on its own; what is easy to get
// wrong is the seam between them, where a component exists but nothing can
// reach it.

func componentNames(g schema.Graph) []string {
	out := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out
}

func edgeNames(g schema.Graph) []string {
	names := map[string]string{}
	for _, n := range g.Nodes {
		names[n.ID] = n.Name
	}
	out := make([]string, 0, len(g.Edges))
	for _, e := range g.Edges {
		out = append(out, names[e.From]+" -> "+names[e.To])
	}
	sort.Strings(out)
	return out
}

func hasEdge(g schema.Graph, want string) bool {
	for _, e := range edgeNames(g) {
		if e == want {
			return true
		}
	}
	return false
}

func nodeNamed(t *testing.T, g schema.Graph, name string) schema.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no component named %q; found %v", name, componentNames(g))
	return schema.Node{}
}

// The case the extractor was added for. Nothing here declares a cluster, a
// Compose project or a chart: CI builds the images and a platform runs them.
func TestRepositoryWithOnlyDockerfilesHasAnArchitecture(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"api/Dockerfile": "FROM golang:1.22 AS build\nRUN go build -o /api\n\n" +
			"FROM scratch\nCOPY --from=build /api /api\n" +
			"ENV DATABASE_URL=postgres://orders-db:5432/orders\nEXPOSE 8080\n",
		"web/Dockerfile": "FROM node:20-alpine AS build\nRUN npm run build\n\n" +
			"FROM nginx:1.27\nCOPY --from=build /dist /usr/share/nginx/html\n" +
			"ENV API_URL=http://api:8080\nEXPOSE 80\n",
		"orders-db/Dockerfile": "FROM postgres:16\nEXPOSE 5432\n",
	}))

	want := []string{"api", "orders-db", "web"}
	if got := componentNames(g); len(got) != len(want) {
		t.Fatalf("components = %v, want %v", got, want)
	}
	for _, edge := range []string{"api -> orders-db", "web -> api"} {
		if !hasEdge(g, edge) {
			t.Errorf("no %q; found %v", edge, edgeNames(g))
		}
	}
	if k := nodeNamed(t, g, "orders-db").Kind; k != schema.KindDatastore {
		t.Errorf("orders-db is a %q, want datastore", k)
	}
	if tech := nodeNamed(t, g, "web").Tech; tech == nil || tech.Language != "javascript" {
		t.Errorf("web tech = %+v, want javascript from the builder stage", tech)
	}
}

// The seam the extractor was partly added to close: a .env file is not a
// component and binds to whatever lives in its directory. Without a Dockerfile
// that directory holds nothing, and the reference is reported instead of drawn.
func TestDockerfileGivesAnEnvFileSomethingToBindTo(t *testing.T) {
	files := map[string]string{
		"svc/.env.example":     "DATABASE_URL=postgres://orders-db:5432/orders\n",
		"orders-db/Dockerfile": "FROM postgres:16\n",
	}
	without := scanRoot(t, writeFiles(t, files))
	if hasEdge(without, "svc -> orders-db") {
		t.Fatal("the premise is wrong: the edge exists with no Dockerfile in svc/")
	}
	if len(diagnosticsWithCode(without, "env_file_unowned")) == 0 {
		t.Errorf("a .env file owned by nothing was not reported: %+v", without.Diagnostics)
	}

	files["svc/Dockerfile"] = "FROM node:20-alpine\nEXPOSE 3000\n"
	with := scanRoot(t, writeFiles(t, files))
	if !hasEdge(with, "svc -> orders-db") {
		t.Errorf("the .env reference did not reach the graph; found %v", edgeNames(with))
	}
	if len(diagnosticsWithCode(with, "env_file_unowned")) != 0 {
		t.Errorf("the .env file is still owned by nothing: %+v", with.Diagnostics)
	}
}

// A Dockerfile beside a manifest is two files describing one component, and
// the whole pipeline has to agree about that or the diagram grows a twin.
func TestDockerfileAndManifestAreOneComponentEndToEnd(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"services/checkout/go.mod":     "module github.com/acme/checkout\n\ngo 1.22\n",
		"services/checkout/Dockerfile": "FROM golang:1.22-alpine\nEXPOSE 8080\n",
	}))

	if len(g.Nodes) != 1 {
		t.Fatalf("components = %v, want one", componentNames(g))
	}
	n := g.Nodes[0]
	if n.Name != "github.com/acme/checkout" {
		t.Errorf("Name = %q, want the declared module name", n.Name)
	}
	if n.Attrs["baseImage"] != "golang:1.22-alpine" {
		t.Errorf("the Dockerfile's base image was lost: %+v", n.Attrs)
	}
	if ports, _ := n.Attrs["ports"].([]int); len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("the exposed port was lost: %+v", n.Attrs)
	}
}

// The regression the fold exists to prevent: a Compose build context needs
// exactly one component in the directory it points at, so a repository that
// gained a Dockerfile must not lose the link between container and code.
func TestBuildContextSurvivesADockerfileEndToEnd(t *testing.T) {
	files := map[string]string{
		"docker-compose.yml": "services:\n  checkout:\n    build: ./services/checkout\n" +
			"    environment:\n      DATABASE_URL: postgres://db:5432/app\n" +
			"  db:\n    image: postgres:16\n",
		"services/checkout/go.mod": "module github.com/acme/checkout\n\ngo 1.22\n",
	}
	without := scanRoot(t, writeFiles(t, files))

	files["services/checkout/Dockerfile"] = "FROM golang:1.22-alpine\nEXPOSE 8080\n"
	with := scanRoot(t, writeFiles(t, files))

	if len(with.Nodes) != len(without.Nodes) {
		t.Errorf("adding a Dockerfile changed the component count from %d to %d:\n  %v\n  %v",
			len(without.Nodes), len(with.Nodes), componentNames(without), componentNames(with))
	}
	if !hasEdge(with, "checkout -> db") {
		t.Errorf("the relationship was lost; found %v", edgeNames(with))
	}
	if tech := nodeNamed(t, with, "checkout").Tech; tech == nil || tech.Language != "go" {
		t.Errorf("the container did not learn what it runs: %+v", tech)
	}
}

// A directory whose name describes its role names no component, end to end.
func TestGenericDirectoryNamesDoNotReachTheGraph(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"services/cartservice/src/Dockerfile": "FROM mcr.microsoft.com/dotnet/aspnet:8.0\n",
		"ops/images/exporter/Dockerfile":      "FROM golang:1.22\n",
	}))

	got := componentNames(g)
	for _, name := range got {
		for _, generic := range []string{"src", "image", "images", "docker"} {
			if strings.EqualFold(name, generic) {
				t.Errorf("a component is named %q; components: %v", name, got)
			}
		}
	}
	nodeNamed(t, g, "cartservice")
	nodeNamed(t, g, "exporter")
}

// Two Dockerfiles in one directory are a variant of one service, which is what
// Dockerfile beside Dockerfile.debug almost always means.
func TestVariantDockerfilesAreOneComponentEndToEnd(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"worker/Dockerfile":       "FROM python:3.12-slim\nENV REDIS_URL=redis://cache:6379/0\n",
		"worker/Dockerfile.debug": "FROM python:3.12-slim\nEXPOSE 5678\n",
		"cache/Dockerfile":        "FROM redis:7-alpine\n",
	}))

	if len(g.Nodes) != 2 {
		t.Fatalf("components = %v, want worker and cache", componentNames(g))
	}
	if !hasEdge(g, "worker -> cache") {
		t.Errorf("no worker -> cache; found %v", edgeNames(g))
	}
}
