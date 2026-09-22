package graphio_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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
	// Windows has no Unix permission bits. Go synthesises a mode from the
	// read-only attribute alone, so every writable file reports 0666 and the
	// distinction this test draws does not exist there.
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not modelled on Windows")
	}
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

// ModTime is what the MCP server compares against the newest manifest to
// decide whether the stored graph still describes the repository. Reading it
// wrong in one direction rescans a graph that was fine; in the other it
// serves a graph the repository has moved on from.
func TestModTime(t *testing.T) {
	root := t.TempDir()

	if _, ok := graphio.ModTime(root); ok {
		t.Error("ModTime() reported a time for a repository with no graph")
	}

	if _, err := graphio.Save(root, sampleGraph(t)); err != nil {
		t.Fatal(err)
	}

	// Pinned to an explicit time rather than to "now", because Unix()
	// truncates to the second and two saves in one test are not reliably
	// distinguishable.
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(graphio.Path(root), want, want); err != nil {
		t.Fatal(err)
	}
	got, ok := graphio.ModTime(root)
	if !ok {
		t.Fatal("ModTime() = not ok for a graph that exists")
	}
	if got != want.Unix() {
		t.Errorf("ModTime() = %d, want %d", got, want.Unix())
	}
}

// A repository where .structura is already a file is a repository Save cannot
// write to. It has to say so rather than fail somewhere further along.
func TestSaveReportsAnUnusableOutputDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, graphio.Dir), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := graphio.Save(root, sampleGraph(t)); err == nil {
		t.Error("Save() = nil with a file where .structura should be")
	}
}

// The atomicity claim is about what survives a write that does not finish.
// The round-trip test only ever exercises writes that do, so this one makes
// the write fail with a graph already in place: the previous graph has to
// still be there and still be loadable, and no debris may be left beside it.
func TestAFailedSaveLeavesThePreviousGraphIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not modelled on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	if _, err := graphio.Save(root, sampleGraph(t)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(graphio.Path(root))
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, graphio.Dir)
	// Readable and traversable, but no new files: the temporary file cannot
	// be created, so Save fails before it has anything to rename into place.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restored so that t.TempDir can remove the tree afterwards.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	newer := sampleGraph(t)
	newer.Stats.FilesScanned = 999
	newer.Normalize()
	if _, err := graphio.Save(root, newer); err == nil {
		t.Fatal("Save() = nil into a directory it cannot write to")
	}

	after, err := os.ReadFile(graphio.Path(root))
	if err != nil {
		t.Fatalf("the previous graph is gone after a failed save: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a failed save modified the previous graph")
	}
	loaded, err := graphio.Load(root)
	if err != nil {
		t.Fatalf("the previous graph no longer parses after a failed save: %v", err)
	}
	if loaded.Stats.FilesScanned == 999 {
		t.Error("a failed save installed its graph anyway")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != graphio.GraphName {
			t.Errorf("a failed save left %q behind", e.Name())
		}
	}
}

// Unreadable is not the same as unscanned. ErrNotFound tells the caller to
// offer a scan; anything else is a problem a scan will not fix, and the MCP
// server's staleness check treats them differently.
func TestLoadDistinguishesUnreadableFromUnscanned(t *testing.T) {
	root := t.TempDir()
	// A directory where the graph should be: present, so not ErrNotFound,
	// and unreadable as a file.
	if err := os.MkdirAll(graphio.Path(root), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := graphio.Load(root)
	if err == nil {
		t.Fatal("Load() = nil for a directory where graph.json should be")
	}
	if errors.Is(err, graphio.ErrNotFound) {
		t.Error("an unreadable graph was reported as an unscanned repository")
	}
}

// The previous test fails at the first step, before a temporary file exists.
// This one fails at the last: the temporary file is written and then cannot
// be renamed into place, which is the only path where the deferred cleanup
// has anything to do. Without it the scan leaves a .graph-*.json behind on
// every attempt, in a directory the user is expected to commit.
func TestAFailedRenameLeavesNoTemporaryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename onto a non-empty directory behaves differently on Windows")
	}
	root := t.TempDir()
	// A non-empty directory where graph.json belongs. Everything up to the
	// rename succeeds; the rename cannot replace it.
	if err := os.MkdirAll(filepath.Join(graphio.Path(root), "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := graphio.Save(root, sampleGraph(t)); err == nil {
		t.Fatal("Save() = nil renaming onto a non-empty directory")
	}

	entries, err := os.ReadDir(filepath.Join(root, graphio.Dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != graphio.GraphName {
			t.Errorf("Save left %q behind after a failed rename", e.Name())
		}
	}
}
