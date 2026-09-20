package graphio_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kamronarabi/structura/internal/graphio"
	"github.com/kamronarabi/structura/pkg/schema"
)

func sampleGraph(t *testing.T) schema.Graph {
	t.Helper()
	b := schema.NewBuilder()
	b.AddNode(schema.Node{
		ID:   schema.NewNodeID(schema.KindService, "compose", "app", "web"),
		Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "web", Confidence: 1,
	})
	g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	g := sampleGraph(t)

	path, err := graphio.Save(root, g)
	if err != nil {
		t.Fatal(err)
	}
	if want := graphio.Path(root); path != want {
		t.Errorf("Save() returned %q, want %q", path, want)
	}

	loaded, err := graphio.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContentHash != g.ContentHash {
		t.Errorf("content hash changed across a round trip:\n %s\n %s", g.ContentHash, loaded.ContentHash)
	}
	if len(loaded.Nodes) != len(g.Nodes) {
		t.Errorf("got %d nodes after loading, want %d", len(loaded.Nodes), len(g.Nodes))
	}
}

func TestLoadReportsAnUnscannedRepository(t *testing.T) {
	_, err := graphio.Load(t.TempDir())
	if !errors.Is(err, graphio.ErrNotFound) {
		t.Errorf("Load() = %v, want ErrNotFound so the caller can offer to scan", err)
	}
}

func TestLoadReportsACorruptGraph(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, graphio.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphio.Path(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := graphio.Load(root)
	if err == nil {
		t.Fatal("Load() = nil for a corrupt file")
	}
	if errors.Is(err, graphio.ErrNotFound) {
		t.Error("a corrupt graph was reported as a missing one; the two need different remedies")
	}
}

// A scan interrupted halfway through must leave the previous graph intact
// rather than a truncated one. The MCP server reads this file on demand,
// possibly while a scan is running.
func TestSaveIsAtomic(t *testing.T) {
	root := t.TempDir()
	if _, err := graphio.Save(root, sampleGraph(t)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(graphio.Path(root))
	if err != nil {
		t.Fatal(err)
	}

	bigger := sampleGraph(t)
	bigger.Stats.FilesScanned = 999
	bigger.Normalize()
	if _, err := graphio.Save(root, bigger); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(graphio.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Error("the second save did not take effect")
	}

	// No temporary files may be left behind for the user to find in a
	// directory they are expected to commit.
	entries, err := os.ReadDir(filepath.Join(root, graphio.Dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != graphio.GraphName {
			t.Errorf("Save left %q behind", e.Name())
		}
	}
}

func TestSavedGraphIsReadableByOtherTools(t *testing.T) {
	root := t.TempDir()
	if _, err := graphio.Save(root, sampleGraph(t)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(graphio.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	// graph.json is meant to be committed and read by other tooling, so it
	// must not inherit the 0600 that CreateTemp gives us.
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("graph.json permissions = %o, want 644", perm)
	}
}
