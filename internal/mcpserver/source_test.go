package mcpserver_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kamronarabi/structura/internal/graphio"
	"github.com/kamronarabi/structura/internal/mcpserver"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// storedGraph scans a fixture once so that .structura/graph.json exists, then
// rewrites the schema version in place and backdates nothing else, so the
// only thing a second load can react to is the version.
func storedGraph(t *testing.T, fixtureName, version string) string {
	t.Helper()
	root := fixture(t, fixtureName)

	if _, err := newSource(root).Graph(context.Background()); err != nil {
		t.Fatalf("priming the stored graph: %v", err)
	}

	path := graphio.Path(root)
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the stored graph: %v", err)
	}
	was := `"schemaVersion": "` + schema.Version + `"`
	if !strings.Contains(string(data), was) {
		t.Fatalf("stored graph does not declare %s", was)
	}
	rewritten := strings.Replace(string(data), was, `"schemaVersion": "`+version+`"`, 1)
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("rewriting the stored graph: %v", err)
	}

	// Staleness is checked against file modification times first. Without
	// this the fixture's own manifests could look newer than the graph and
	// force a rescan for a reason that has nothing to do with the version.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("dating the stored graph: %v", err)
	}
	return root
}

func newSource(root string) *mcpserver.Source {
	return mcpserver.NewSource(root, extractors.Default(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func storedVersion(t *testing.T, root string) string {
	t.Helper()
	g, err := graphio.Load(root)
	if err != nil {
		t.Fatalf("reading the stored graph: %v", err)
	}
	return g.SchemaVersion
}

// A graph written by a newer build must be served, not replaced. graph.json
// is committed, so rescanning it here would commit the removal of everything
// this build cannot represent -- and the diff would look like the
// architecture changed.
func TestANewerStoredGraphIsServedRatherThanOverwritten(t *testing.T) {
	root := storedGraph(t, "compose-monolith", "0.99.0")

	g, err := newSource(root).Graph(context.Background())
	if err != nil {
		t.Fatalf("loading the graph: %v", err)
	}
	if g.SchemaVersion != "0.99.0" {
		t.Errorf("served schema %s; the newer stored graph was discarded", g.SchemaVersion)
	}
	if got := storedVersion(t, root); got != "0.99.0" {
		t.Errorf("stored graph is now schema %s; this build overwrote a newer one", got)
	}
}

// The other direction is the ordinary upgrade path: an older graph is missing
// whatever this build learned to see since, so it is rebuilt.
func TestAnOlderStoredGraphIsRebuilt(t *testing.T) {
	root := storedGraph(t, "compose-monolith", "0.0.1")

	g, err := newSource(root).Graph(context.Background())
	if err != nil {
		t.Fatalf("loading the graph: %v", err)
	}
	if g.SchemaVersion != schema.Version {
		t.Errorf("served schema %s, want %s", g.SchemaVersion, schema.Version)
	}
	if got := storedVersion(t, root); got != schema.Version {
		t.Errorf("stored graph left at schema %s, want %s", got, schema.Version)
	}
}

// A different major says nothing reliable about the contents, so the only
// honest answer is a fresh scan.
func TestAnIncompatibleStoredGraphIsRebuilt(t *testing.T) {
	root := storedGraph(t, "compose-monolith", "7.0.0")

	g, err := newSource(root).Graph(context.Background())
	if err != nil {
		t.Fatalf("loading the graph: %v", err)
	}
	if g.SchemaVersion != schema.Version {
		t.Errorf("served schema %s, want %s", g.SchemaVersion, schema.Version)
	}
}

// A patch differs in wording only, so it must not invalidate the cache: a
// clarification to a doc comment should not make every graph in the world
// rescan.
func TestAPatchDifferenceDoesNotForceARescan(t *testing.T) {
	major, minor, _ := strings.Cut(schema.Version, ".")
	minor, _, _ = strings.Cut(minor, ".")
	patched := major + "." + minor + ".99"

	root := storedGraph(t, "compose-monolith", patched)

	g, err := newSource(root).Graph(context.Background())
	if err != nil {
		t.Fatalf("loading the graph: %v", err)
	}
	if g.SchemaVersion != patched {
		t.Errorf("served schema %s; a patch difference triggered a rescan", g.SchemaVersion)
	}
}
