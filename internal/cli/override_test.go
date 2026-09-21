package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/cli"
	"github.com/kamronarabi/structura/pkg/schema"
)

// These check that a correction written in the file a person actually edits
// reaches the graph. The override package is tested directly; what is easy to
// get wrong is the wiring -- a config key that never binds fails silently and
// looks exactly like a rule that did nothing.

const overrideCompose = `services:
  web:
    image: web:1
    environment:
      API_URL: http://api:8080
  api:
    image: api:1
    environment:
      STRIPE_API_URL: https://api.stripe.com
  cache:
    image: redis:7
`

func repoWithConfig(t *testing.T, config string) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docker-compose.yml", overrideCompose)
	if config != "" {
		write(".structura.yaml", config)
	}
	return root
}

func scanGraph(t *testing.T, root string) schema.Graph {
	t.Helper()
	var out, errOut strings.Builder
	code := cli.Execute(context.Background(),
		[]string{"scan", "--root", root, "--output", "-"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("scan exited %d: %s", code, errOut.String())
	}
	var g schema.Graph
	if err := json.Unmarshal([]byte(out.String()), &g); err != nil {
		t.Fatalf("unmarshaling graph: %v\n%s", err, out.String())
	}
	return g
}

func edgeBetween(g schema.Graph, fromName, toName string) (schema.Edge, bool) {
	names := map[string]string{}
	for _, n := range g.Nodes {
		names[n.ID] = n.Name
	}
	for _, e := range g.Edges {
		if names[e.From] == fromName && names[e.To] == toName {
			return e, true
		}
	}
	return schema.Edge{}, false
}

func hasDiag(g schema.Graph, code string) bool {
	for _, d := range g.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

// The case a configuration scan cannot reach: a dependency written in code.
func TestConfigDeclaredRelationshipReachesTheGraph(t *testing.T) {
	root := repoWithConfig(t, `
relationships:
  - from: api
    to: cache
    kind: persists_to
    protocol: redis
    note: redis client built in api/internal/cache.go
`)
	g := scanGraph(t, root)

	e, ok := edgeBetween(g, "api", "cache")
	if !ok {
		t.Fatalf("the declared relationship never reached the graph; edges: %+v", g.Edges)
	}
	if e.Confidence != schema.ConfDeclared {
		t.Errorf("Confidence = %v, want %v", e.Confidence, schema.ConfDeclared)
	}
	if len(e.Evidence) == 0 || !strings.Contains(e.Evidence[0].Detail, "internal/cache.go") {
		t.Errorf("the reason did not survive into the graph: %+v", e.Evidence)
	}
}

func TestConfigRemovedRelationshipLeavesTheGraph(t *testing.T) {
	root := repoWithConfig(t, `
relationships:
  - from: api
    to: api.stripe.com
    remove: true
`)
	g := scanGraph(t, root)

	if _, ok := edgeBetween(g, "api", "api.stripe.com"); ok {
		t.Error("the relationship was not removed")
	}
	for _, n := range g.Nodes {
		if n.Name == "api.stripe.com" {
			t.Error("the orphaned external system survived")
		}
	}
	if _, ok := edgeBetween(g, "web", "api"); !ok {
		t.Error("an unrelated relationship was removed")
	}
}

// A rule that did nothing has to say so, in the graph, where the person
// looking for its effect will be.
func TestStaleConfigRuleIsReportedInTheGraph(t *testing.T) {
	root := repoWithConfig(t, `
relationships:
  - from: api
    to: a-service-that-never-existed
    kind: calls
`)
	g := scanGraph(t, root)

	if !hasDiag(g, "override_unresolved") {
		t.Errorf("a stale rule was applied silently; diagnostics: %+v", g.Diagnostics)
	}
}

func TestConfigMergeReachesTheGraph(t *testing.T) {
	root := repoWithConfig(t, `
components:
  - same: [api, cache]
    note: the cache runs in-process
`)
	g := scanGraph(t, root)

	for _, n := range g.Nodes {
		if n.Name == "cache" {
			t.Error("the folded component survived as its own box")
		}
	}
}

// Without corrections the architecture must be exactly what it was, or this
// becomes a feature everybody pays for.
//
// The content hash is not the comparison: a config file is a file, so its
// presence moves FilesScanned and the hash with it. What must not move is the
// architecture.
func TestEmptyRulesChangeNothing(t *testing.T) {
	plain := architecture(t, scanGraph(t, repoWithConfig(t, "")))
	empty := architecture(t, scanGraph(t, repoWithConfig(t, "relationships: []\ncomponents: []\n")))

	if plain != empty {
		t.Errorf("empty rules changed the architecture:\n%s\n---\n%s", plain, empty)
	}
}

// architecture renders the part of a graph that empty rules must not touch.
func architecture(t *testing.T, g schema.Graph) string {
	t.Helper()
	data, err := json.Marshal(struct {
		Nodes []schema.Node `json:"nodes"`
		Edges []schema.Edge `json:"edges"`
	}{g.Nodes, g.Edges})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Two scans of one tree must agree, corrections included.
func TestCorrectedGraphIsDeterministic(t *testing.T) {
	root := repoWithConfig(t, `
relationships:
  - from: api
    to: cache
    kind: persists_to
  - from: api
    to: api.stripe.com
    remove: true
`)
	first := scanGraph(t, root)
	second := scanGraph(t, root)

	if first.ContentHash != second.ContentHash {
		t.Errorf("two scans disagreed:\n  %s\n  %s", first.ContentHash, second.ContentHash)
	}
}

// A correction must not smuggle an absolute path into a committed graph.
func TestDeclaredEvidenceCarriesNoAbsolutePath(t *testing.T) {
	root := repoWithConfig(t, `
relationships:
  - from: api
    to: cache
    kind: persists_to
`)
	g := scanGraph(t, root)

	e, ok := edgeBetween(g, "api", "cache")
	if !ok {
		t.Fatal("no declared edge")
	}
	for _, ev := range e.Evidence {
		if filepath.IsAbs(ev.Path) || strings.Contains(ev.Path, root) {
			t.Errorf("evidence path leaks the scan location: %q", ev.Path)
		}
	}
}
