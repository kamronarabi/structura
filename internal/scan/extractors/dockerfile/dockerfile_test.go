package dockerfile_test

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/dockerfile"
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

func (c *capture) codes() []string {
	var out []string
	for _, d := range c.diags {
		out = append(out, d.Code)
	}
	return out
}

func (c *capture) node(t *testing.T) schema.Node {
	t.Helper()
	if len(c.nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(c.nodes), c.nodes)
	}
	return c.nodes[0]
}

func extract(t *testing.T, filePath, content string) *capture {
	t.Helper()
	c := &capture{}
	dir := path.Dir(filePath)
	if dir == "." {
		dir = ""
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{
			Path: filePath, Dir: dir, Name: path.Base(filePath),
			Ext:  strings.ToLower(path.Ext(filePath)),
			Size: int64(len(content)),
		},
		Content: []byte(content),
	}
	if err := dockerfile.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatch(t *testing.T) {
	e := dockerfile.New()
	yes := []struct{ name, ext string }{
		{"Dockerfile", ""}, {"dockerfile", ""}, {"Containerfile", ""},
		{"Dockerfile.dev", ".dev"}, {"Dockerfile.debug", ".debug"},
		{"api.dockerfile", ".dockerfile"},
	}
	for _, f := range yes {
		if !e.Match(scan.FileMeta{Name: f.name, Ext: f.ext}) {
			t.Errorf("Match(%q) = false, want true", f.name)
		}
	}
	no := []struct{ name, ext string }{
		{".dockerignore", ""}, {"docker-compose.yml", ".yml"},
		{"Dockerfile.md", ".md"}, {"README.md", ".md"},
		{"docker-bake.hcl", ".hcl"}, {"Dockerfile.yaml", ".yaml"},
	}
	for _, f := range no {
		if e.Match(scan.FileMeta{Name: f.name, Ext: f.ext}) {
			t.Errorf("Match(%q) = true, want false", f.name)
		}
	}
}

// The case the extractor exists for: a directory with a Dockerfile and nothing
// else becomes a component, where before it was a file nobody read.
func TestDockerfileMakesTheDirectoryAComponent(t *testing.T) {
	c := extract(t, "services/api/Dockerfile", "FROM golang:1.22-alpine\nEXPOSE 8080\n")

	n := c.node(t)
	if n.Name != "api" {
		t.Errorf("Name = %q, want api", n.Name)
	}
	if n.Kind != schema.KindService {
		t.Errorf("Kind = %q, want service", n.Kind)
	}
	if n.Attrs["directory"] != "services/api" {
		t.Errorf("directory = %v, want services/api", n.Attrs["directory"])
	}
	if n.Tech == nil || n.Tech.Language != "go" {
		t.Errorf("Tech = %+v, want language go", n.Tech)
	}
	ports, _ := n.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]", n.Attrs["ports"])
	}
	// A Dockerfile proves something builds here, not that it is deployed.
	if n.Confidence != schema.ConfHostMatch {
		t.Errorf("Confidence = %v, want %v", n.Confidence, schema.ConfHostMatch)
	}
}

// The single most common Dockerfile in a modern repository: the language is in
// the builder stage and the final stage is a runtime that names none.
func TestLanguageComesFromTheBuilderWhenTheRuntimeNamesNone(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		language string
		base     string
	}{
		{
			name:     "go binary into scratch",
			content:  "FROM golang:1.22 AS build\nRUN go build\n\nFROM scratch\nCOPY --from=build /app /app\n",
			language: "go",
			base:     "scratch",
		},
		{
			name:     "react bundle into nginx",
			content:  "FROM node:20-alpine AS builder\nRUN npm run build\n\nFROM nginx:alpine\nCOPY --from=builder /dist /usr/share/nginx/html\n",
			language: "javascript",
			base:     "nginx:alpine",
		},
		{
			name:     "java into a jre",
			content:  "FROM maven:3.9 AS build\n\nFROM eclipse-temurin:17-jre-focal\n",
			language: "java",
			base:     "eclipse-temurin:17-jre-focal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := extract(t, "Dockerfile", tt.content).node(t)
			if n.Tech == nil || n.Tech.Language != tt.language {
				t.Errorf("Tech = %+v, want language %s", n.Tech, tt.language)
			}
			if n.Attrs["baseImage"] != tt.base {
				t.Errorf("baseImage = %v, want %s", n.Attrs["baseImage"], tt.base)
			}
			if n.Attrs["buildStages"] != 2 {
				t.Errorf("buildStages = %v, want 2", n.Attrs["buildStages"])
			}
		})
	}
}

// A final stage built on an earlier one says nothing about a runtime until the
// reference is followed.
func TestStageReferencesAreFollowedToARealImage(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"FROM python:3.12-slim AS base",
		"FROM base AS deps",
		"RUN pip install -r requirements.txt",
		"FROM deps",
		"CMD [\"python\", \"app.py\"]",
	}, "\n")).node(t)

	if n.Attrs["baseImage"] != "python:3.12-slim" {
		t.Errorf("baseImage = %v, want the image the stage chain lands on", n.Attrs["baseImage"])
	}
	if n.Tech == nil || n.Tech.Language != "python" {
		t.Errorf("Tech = %+v, want language python", n.Tech)
	}
}

// A stage named after itself is a file that would not build, and must not spin
// the resolution loop.
func TestSelfReferentialStageTerminates(t *testing.T) {
	c := extract(t, "Dockerfile", "FROM build AS build\n")
	if len(c.nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(c.nodes))
	}
}

// A third of the Dockerfiles in the wild carry a flag on the FROM line.
func TestPlatformFlagIsNotMistakenForTheImage(t *testing.T) {
	n := extract(t, "Dockerfile",
		"FROM --platform=$BUILDPLATFORM golang:1.22 AS build\nFROM --platform=linux/amd64 alpine:3.20\n").node(t)

	if n.Attrs["baseImage"] != "alpine:3.20" {
		t.Errorf("baseImage = %v, want alpine:3.20", n.Attrs["baseImage"])
	}
	if n.Tech == nil || n.Tech.Language != "go" {
		t.Errorf("Tech = %+v, want language go from the builder stage", n.Tech)
	}
}

// A Dockerfile starting FROM postgres is shipping a Postgres, not a service
// that happens to be written in one.
func TestDatastoreBaseMakesADatastore(t *testing.T) {
	n := extract(t, "db/Dockerfile", "FROM postgres:16\nCOPY init.sql /docker-entrypoint-initdb.d/\n").node(t)

	if n.Kind != schema.KindDatastore {
		t.Errorf("Kind = %q, want datastore", n.Kind)
	}
	if n.Tech == nil || n.Tech.Framework != "postgresql" {
		t.Errorf("Tech = %+v, want framework postgresql", n.Tech)
	}
}

// The other way round: a web server base is how a bundle is served, not a
// piece of infrastructure this repository ships.
func TestWebServerBaseStaysAService(t *testing.T) {
	n := extract(t, "web/Dockerfile", "FROM nginx:1.27\nCOPY dist /usr/share/nginx/html\n").node(t)

	if n.Kind != schema.KindService {
		t.Errorf("Kind = %q, want service", n.Kind)
	}
	if n.Tech == nil || n.Tech.Framework != "nginx" {
		t.Errorf("Tech = %+v, want framework nginx", n.Tech)
	}
}

// Build stages are not components: a three-stage build is one thing that ships.
func TestBuildStagesAreNotDrawn(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20 AS deps",
		"FROM node:20 AS builder",
		"FROM node:20 AS runner",
	}, "\n"))

	if len(c.nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: a build is one component", len(c.nodes))
	}
	if c.nodes[0].Attrs["buildStages"] != 3 {
		t.Errorf("buildStages = %v, want 3", c.nodes[0].Attrs["buildStages"])
	}
}

func TestPortsAreCollectedAcrossTheFile(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20 AS dev",
		"EXPOSE $PORT 9229 9230",
		"FROM node:20",
		"EXPOSE 3000/tcp",
		"EXPOSE 3000",
		"EXPOSE notaport",
	}, "\n")).node(t)

	ports, _ := n.Attrs["ports"].([]int)
	want := []int{3000, 9229, 9230}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

// A baked-in endpoint is the same reference a .env file would carry.
func TestEnvReferencesBecomeHints(t *testing.T) {
	c := extract(t, "api/Dockerfile", strings.Join([]string{
		"FROM golang:1.22",
		"ENV DATABASE_URL=postgres://orders-db:5432/orders",
		"ENV NODE_ENV production",
		"ARG API_BASE_URL=https://api.example.com",
	}, "\n"))

	if len(c.hints) != 2 {
		t.Fatalf("hints = %d, want 2: %+v", len(c.hints), c.hints)
	}
	var sawDB, sawAPI bool
	for _, h := range c.hints {
		switch h.Tokens[0] {
		case "orders-db":
			sawDB = true
			if h.SuggestedEdge != schema.EdgePersistsTo {
				t.Errorf("DATABASE_URL edge = %q, want persists_to", h.SuggestedEdge)
			}
			if h.FromNode == "" {
				t.Error("the hint is not attached to the component the Dockerfile builds")
			}
		case "api.example.com":
			sawAPI = true
		}
	}
	if !sawDB || !sawAPI {
		t.Errorf("hints = %+v, want the database and the API", c.hints)
	}
}

// A value still holding a build argument names nothing yet. Whoever runs the
// build decides what it becomes.
func TestUninterpolatedValuesAreNotReferences(t *testing.T) {
	c := extract(t, "Dockerfile", strings.Join([]string{
		"FROM node:20",
		"ARG DB_HOST",
		"ENV DATABASE_URL=postgres://$DB_HOST:5432/app",
		"ENV API_URL ${API_ENDPOINT}",
	}, "\n"))

	if len(c.hints) != 0 {
		t.Errorf("hints = %+v, want none", c.hints)
	}
}

// A line continuation is how most real Dockerfiles are written.
func TestContinuationsAndCommentsAreHandled(t *testing.T) {
	n := extract(t, "Dockerfile", strings.Join([]string{
		"# syntax=docker/dockerfile:1",
		"",
		"from \\",
		"  # an explanation nobody deleted",
		"  golang:1.22-alpine AS build",
		"RUN go build \\",
		"    -o /app",
		"EXPOSE \\",
		"  8080",
	}, "\n")).node(t)

	if n.Tech == nil || n.Tech.Language != "go" {
		t.Errorf("Tech = %+v, want language go: a lowercase FROM is still a FROM", n.Tech)
	}
	ports, _ := n.Attrs["ports"].([]int)
	if len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080]", n.Attrs["ports"])
	}
}

// A file named like a Dockerfile with no FROM builds nothing, and saying so is
// better than drawing a component with no runtime.
func TestFileWithNoBaseImageIsReported(t *testing.T) {
	c := extract(t, "Dockerfile.fragment", "RUN echo hello\nCOPY . .\n")

	if len(c.nodes) != 0 {
		t.Errorf("nodes = %+v, want none", c.nodes)
	}
	if len(c.codes()) != 1 || c.codes()[0] != "dockerfile_no_base_image" {
		t.Errorf("diagnostics = %v, want dockerfile_no_base_image", c.codes())
	}
}

// A directory named for its role names no component. Three of them in one
// repository would otherwise put three boxes labelled "image" on the diagram.
func TestGenericDirectoriesAreNamedFromAbove(t *testing.T) {
	tests := map[string]string{
		"src/cartservice/src/Dockerfile":        "cartservice",
		"storage/mysql-galera/image/Dockerfile": "mysql-galera",
		"services/worker/Dockerfile":            "worker",
		"Dockerfile":                            "app",
		"docker/Dockerfile":                     "app",
	}
	for filePath, want := range tests {
		if got := extract(t, filePath, "FROM alpine:3.20\n").node(t).Name; got != want {
			t.Errorf("%s names the component %q, want %q", filePath, got, want)
		}
	}
}

// The name is derived, and the resolver has to be able to tell, or a Dockerfile
// beside a manifest would be able to win the name the manifest declared.
func TestDerivedNameIsRecorded(t *testing.T) {
	n := extract(t, "services/api/Dockerfile", "FROM node:20\n").node(t)
	if n.Attrs["nameFrom"] != "directory" {
		t.Errorf("nameFrom = %v, want directory", n.Attrs["nameFrom"])
	}
}

// An image CI pushes is conventionally named after the directory, which is how
// a workload running that image finds this component. A directory named for its
// role names no image, and claiming the name would make it ambiguous.
func TestAliasIsEmittedOnlyForANamingDirectory(t *testing.T) {
	if got := extract(t, "services/api/Dockerfile", "FROM node:20\n").aliases; len(got) != 1 {
		t.Errorf("aliases = %+v, want one for services/api", got)
	} else if got[0].Name != "api" {
		t.Errorf("alias name = %q, want api", got[0].Name)
	}
	if got := extract(t, "a/image/Dockerfile", "FROM node:20\n").aliases; len(got) != 0 {
		t.Errorf("aliases = %+v, want none: \"image\" routes nowhere", got)
	}
}

// An extractor never panics: it runs on untrusted input.
func TestMalformedInputIsSurvived(t *testing.T) {
	for _, content := range []string{
		"", "FROM\n", "FROM \n", "   ", "\x00\x01\x02", "FROM a AS\n",
		"ENV\n", "ENV =value\n", "ENV KEY=\"unterminated\n", "EXPOSE\n",
		"FROM alpine AS a\nFROM a AS b\nFROM b AS a\nFROM a\n",
		strings.Repeat("FROM alpine \\\n", 500),
	} {
		extract(t, "Dockerfile", content)
	}
}

// Dockerfile beside Dockerfile.debug is the overwhelmingly common reason for a
// second one: a variant of the same service. Sharing an identifier is what
// makes them collapse into one component rather than two boxes.
func TestVariantDockerfilesShareAnIdentifier(t *testing.T) {
	plain := extract(t, "src/cartservice/Dockerfile",
		"FROM mcr.microsoft.com/dotnet/sdk:8.0\n").node(t)
	debug := extract(t, "src/cartservice/Dockerfile.debug",
		"FROM mcr.microsoft.com/dotnet/sdk:8.0\nENV DEBUG=1\n").node(t)

	if plain.ID != debug.ID {
		t.Errorf("two Dockerfiles in one directory got different identifiers:\n  %s\n  %s",
			plain.ID, debug.ID)
	}
	if plain.Attrs["dockerfile"] != "Dockerfile" || debug.Attrs["dockerfile"] != "Dockerfile.debug" {
		t.Errorf("the file each came from was not recorded: %v / %v",
			plain.Attrs["dockerfile"], debug.Attrs["dockerfile"])
	}
}
