package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// twoStacks writes a repository holding two independent projects that reuse
// every name worth reusing: the same namespace, the same service names, the
// same database name.
func twoStacks(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	for _, stack := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, "samples", stack, "deploy")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
  labels: {app: api}
spec:
  replicas: 1
  template:
    metadata:
      labels: {app: api}
    spec:
      containers:
        - name: api
          image: ` + stack + `/api:1.0
          env:
            - name: DATABASE_URL
              value: postgres://api@orders-db:5432/api
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: orders-db
  namespace: prod
  labels: {app: orders-db}
spec:
  template:
    metadata:
      labels: {app: orders-db}
    spec:
      containers:
        - name: db
          image: postgres:16
`
		if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// twoDistinctStacks writes two independent projects that share a namespace
// name and nothing else, which is what separate systems in one repository
// actually look like: the reused word is an accident, the components are not.
func twoDistinctStacks(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	services := map[string][]string{
		"alpha": {"checkout", "orders-db"},
		"beta":  {"reports", "warehouse-db"},
	}
	for stack, names := range services {
		dir := filepath.Join(root, "samples", stack, "deploy")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var manifest string
		for _, name := range names {
			manifest += `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
  namespace: prod
  labels: {app: ` + name + `}
spec:
  replicas: 1
  template:
    metadata:
      labels: {app: ` + name + `}
    spec:
      containers:
        - name: main
          image: ` + stack + `/` + name + `:1.0
---
`
		}
		if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runProjects(t *testing.T, root string, projects []string) schema.Graph {
	t.Helper()
	res, err := scan.Run(context.Background(), extractors.Default(), scan.Options{
		Root:     root,
		Projects: projects,
	})
	if err != nil {
		t.Fatalf("scan.Run() = %v", err)
	}
	return res.Graph
}

func projectOf(n schema.Node) string {
	if p, ok := n.Attrs["project"].(string); ok && p != "" {
		return p
	}
	return "."
}

func namesOf(g schema.Graph, kind schema.NodeKind) []string {
	var out []string
	for _, n := range g.Nodes {
		if n.Kind == kind {
			out = append(out, n.Name)
		}
	}
	return out
}

// Without a declared boundary the two stacks collapse into one. This is the
// behaviour that shipped, and the test states it so that the fix below has
// something to be a fix of.
func TestUndeclaredProjectsStillMerge(t *testing.T) {
	g := runProjects(t, twoStacks(t), nil)

	boundaries := namesOf(g, schema.KindBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("boundaries = %v, want one: without configuration the repository is one project", boundaries)
	}
	for _, n := range g.Nodes {
		if projectOf(n) != "." {
			t.Errorf("node %s was assigned project %q with no configuration", n.ID, projectOf(n))
		}
		if _, ok := n.Attrs["project"]; ok {
			t.Errorf("node %s carries a project attribute in a single-project repository", n.ID)
		}
	}
}

// Declared, the same repository keeps its two stacks apart: two "prod"
// namespaces, two APIs, two databases, and no relationship between them.
func TestDeclaredProjectsAreKeptApart(t *testing.T) {
	g := runProjects(t, twoStacks(t), []string{"samples/alpha", "samples/beta"})

	if got := namesOf(g, schema.KindBoundary); len(got) != 2 {
		t.Fatalf("boundaries = %v, want two prod namespaces", got)
	}

	byProject := map[string]int{}
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		byProject[projectOf(n)]++
		if ids[n.ID] {
			t.Errorf("duplicate node id %s", n.ID)
		}
		ids[n.ID] = true
	}
	for _, want := range []string{"samples/alpha", "samples/beta"} {
		if byProject[want] == 0 {
			t.Errorf("no components in project %q; got %v", want, byProject)
		}
	}

	byID := map[string]schema.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	for _, e := range g.Edges {
		from, to := byID[e.From], byID[e.To]
		if to.Kind == schema.KindExternal {
			continue
		}
		if projectOf(from) != projectOf(to) {
			t.Errorf("edge crosses projects: %s [%s] -%s-> %s [%s]",
				from.Name, projectOf(from), e.Kind, to.Name, projectOf(to))
		}
	}
}

// Each project's api must reach its own database, or the separation has been
// bought by breaking the graph.
func TestDeclaredProjectsKeepTheirOwnEdges(t *testing.T) {
	g := runProjects(t, twoStacks(t), []string{"samples/alpha", "samples/beta"})

	byID := map[string]schema.Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	found := map[string]bool{}
	for _, e := range g.Edges {
		from, to := byID[e.From], byID[e.To]
		if from.Name == "api" && to.Name == "orders-db" && e.Kind == schema.EdgePersistsTo {
			found[projectOf(from)] = true
		}
	}
	for _, want := range []string{"samples/alpha", "samples/beta"} {
		if !found[want] {
			t.Errorf("project %q lost the edge from its api to its own database", want)
		}
	}
}

// Qualifying an ID must not change the node's namespace: a namespace is what
// the manifest declared, and is what a reader is shown.
func TestQualifiedIDsKeepTheirDeclaredNamespace(t *testing.T) {
	g := runProjects(t, twoStacks(t), []string{"samples/alpha", "samples/beta"})

	for _, n := range g.Nodes {
		if n.Kind == schema.KindExternal {
			continue
		}
		if n.Namespace != "prod" {
			t.Errorf("node %s has namespace %q, want the declared \"prod\"", n.ID, n.Namespace)
		}
		if !strings.Contains(n.ID, "samples-alpha") && !strings.Contains(n.ID, "samples-beta") {
			t.Errorf("node %s was not qualified by its project", n.ID)
		}
	}
}

// Declaring one project must not qualify the rest of the repository, or every
// identifier outside it would churn for no reason.
func TestUnqualifiedProjectKeepsPlainIdentifiers(t *testing.T) {
	g := runProjects(t, twoStacks(t), []string{"samples/alpha"})

	var sawPlain, sawQualified bool
	for _, n := range g.Nodes {
		switch projectOf(n) {
		case "samples/alpha":
			sawQualified = true
			if !strings.Contains(n.ID, "samples-alpha") {
				t.Errorf("node %s in a declared project was not qualified", n.ID)
			}
		case ".":
			sawPlain = true
			if strings.Contains(n.ID, "samples-") {
				t.Errorf("node %s outside every declared project was qualified", n.ID)
			}
		}
	}
	if !sawPlain || !sawQualified {
		t.Fatalf("expected both a qualified and an unqualified project; plain=%v qualified=%v",
			sawPlain, sawQualified)
	}
}

// Two scans of one tree must agree, projects or not.
func TestProjectQualificationIsDeterministic(t *testing.T) {
	root := twoStacks(t)
	projects := []string{"samples/beta", "samples/alpha"}

	first := runProjects(t, root, projects)
	second := runProjects(t, root, []string{"samples/alpha", "samples/beta"})

	if first.ContentHash != second.ContentHash {
		t.Errorf("content hash depends on the order projects were configured:\n  %s\n  %s",
			first.ContentHash, second.ContentHash)
	}
}

// The boundary machinery only helps people who know they need it, so the scan
// has to say so. Nothing about the graph changes; the warning is the whole
// feature.
func TestUndeclaredProjectsAreReported(t *testing.T) {
	g := runProjects(t, twoDistinctStacks(t), nil)

	var found []schema.Diagnostic
	for _, d := range g.Diagnostics {
		if d.Code == "undeclared_projects" {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("undeclared_projects diagnostics = %d, want 1: %+v", len(found), g.Diagnostics)
	}
	if found[0].Severity != schema.SeverityWarn {
		t.Errorf("Severity = %q, want warn: a merged pair of systems is material", found[0].Severity)
	}
	for _, want := range []string{"samples/alpha", "samples/beta", "projects:", ".structura.yaml"} {
		if !strings.Contains(found[0].Message, want) {
			t.Errorf("message does not mention %q: %s", want, found[0].Message)
		}
	}
}

// Once declared, the question is answered and must not be asked again.
func TestDeclaredProjectsSilenceTheWarning(t *testing.T) {
	g := runProjects(t, twoDistinctStacks(t), []string{"samples/alpha", "samples/beta"})

	for _, d := range g.Diagnostics {
		if d.Code == "undeclared_projects" {
			t.Errorf("still warning after the projects were declared: %s", d.Message)
		}
	}
}

// The warning is advisory. Declaring nothing must leave the graph exactly as
// it was, or this stops being a warning and becomes a silent behaviour change.
func TestWarningDoesNotChangeTheGraph(t *testing.T) {
	g := runProjects(t, twoDistinctStacks(t), nil)

	for _, n := range g.Nodes {
		if _, ok := n.Attrs["project"]; ok {
			t.Errorf("node %s was assigned a project by a warning", n.ID)
		}
	}
	if got := namesOf(g, schema.KindBoundary); len(got) != 1 {
		t.Errorf("boundaries = %v, want the one merged boundary: the warning must not split anything", got)
	}
}

// The limit of the warning, pinned so it is a known cost rather than a
// surprise.
//
// Two projects whose components are named identically are indistinguishable
// from one project's base and overlay: both are two trees declaring the same
// components. Nothing in the file tree separates them, so the scan stays
// quiet and merges them, exactly as it did before any of this existed.
//
// This is the failure the warning cannot catch, and it is also the case where
// the merge does most damage. Declaring the projects is the only fix.
func TestIdenticallyNamedStacksAreNotDetected(t *testing.T) {
	g := runProjects(t, twoStacks(t), nil)

	for _, d := range g.Diagnostics {
		if d.Code == "undeclared_projects" {
			t.Errorf("the heuristic changed and now catches identical stacks; "+
				"update the comment on sharesRealComponent: %s", d.Message)
		}
	}
}
