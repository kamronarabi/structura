package scan

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// yamlOnly exercises the rule that Match sees metadata only.
type yamlOnly struct{}

func (yamlOnly) Name() string                                  { return "yaml" }
func (yamlOnly) Match(f FileMeta) bool                         { return f.Ext == ".yaml" || f.Ext == ".yml" }
func (yamlOnly) Extract(context.Context, *File, Emitter) error { return nil }

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func walkPaths(t *testing.T, dir string, reg *Registry, opts WalkOptions) ([]string, WalkResult) {
	t.Helper()
	res, err := Walk(context.Background(), dir, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(res.Files))
	for i, f := range res.Files {
		paths[i] = f.Path
	}
	return paths, res
}

func TestWalkReturnsOnlyMatchedFiles(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"docker-compose.yml": "services: {}",
		"README.md":          "# hi",
		"src/main.go":        "package main",
		"deploy/api.yaml":    "kind: Deployment",
	})

	paths, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	want := []string{"deploy/api.yaml", "docker-compose.yml"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("matched files = %v, want %v", paths, want)
	}
	// Everything walked counts toward filesScanned, not just what parsed.
	if res.Scanned != 4 {
		t.Errorf("Scanned = %d, want 4", res.Scanned)
	}
}

func TestWalkSkipsDependencyAndBuildDirectories(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"docker-compose.yml":                  "services: {}",
		"node_modules/pkg/docker-compose.yml": "services: {}",
		"vendor/lib/deploy.yaml":              "kind: Deployment",
		".git/config.yaml":                    "x: 1",
		"target/out.yaml":                     "x: 1",
		"sub/__pycache__/thing.yaml":          "x: 1",
		".structura/graph.yaml":               "x: 1",
	})

	paths, _ := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	if len(paths) != 1 || paths[0] != "docker-compose.yml" {
		t.Errorf("matched files = %v, want only docker-compose.yml", paths)
	}
}

func TestStructuraignoreCanReincludeWhatGitignoreExcluded(t *testing.T) {
	// Deployment manifests are routinely generated into a build directory
	// that a developer keeps out of git. They are still the only place the
	// deployed topology is written down.
	dir := writeTree(t, map[string]string{
		".gitignore":         "generated/\n",
		".structuraignore":   "!generated/\n",
		"generated/api.yaml": "kind: Deployment",
		"docker-compose.yml": "services: {}",
	})

	paths, _ := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	if len(paths) != 2 {
		t.Errorf("matched files = %v, want the generated manifest to be re-included", paths)
	}
}

func TestWalkSkipsOversizedFiles(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"small.yaml": "a: 1",
		"huge.yaml":  strings.Repeat("x", 4096),
	})

	paths, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{MaxFileSize: 1024})

	if len(paths) != 1 || paths[0] != "small.yaml" {
		t.Errorf("matched files = %v, want only small.yaml", paths)
	}
	if !hasDiagnostic(res, "file_too_large") {
		t.Error("skipping an oversized file was not reported")
	}
}

func TestOversizedUnmatchedFilesAreNotReported(t *testing.T) {
	// A large binary that no extractor wanted is not a gap in the graph, and
	// reporting every one of them would bury the diagnostic that matters.
	dir := writeTree(t, map[string]string{
		"blob.bin": strings.Repeat("x", 4096),
	})

	_, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{MaxFileSize: 1024})

	if hasDiagnostic(res, "file_too_large") {
		t.Error("an oversized file no extractor claimed was reported as a gap")
	}
}

func TestWalkRefusesSymlinksEscapingRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	outside := writeTree(t, map[string]string{"secret.yaml": "password: hunter2"})
	dir := writeTree(t, map[string]string{"docker-compose.yml": "services: {}"})

	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(dir, "leak.yaml")); err != nil {
		t.Fatal(err)
	}

	paths, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	for _, p := range paths {
		if p == "leak.yaml" {
			t.Fatal("a symlink pointing outside the repository was followed")
		}
	}
	if !hasDiagnostic(res, "symlink_escapes_root") {
		t.Error("following was refused but nothing was reported")
	}
}

func TestWalkFollowsSymlinkedFilesInsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := writeTree(t, map[string]string{"real/api.yaml": "kind: Deployment"})
	if err := os.Symlink(filepath.Join(dir, "real", "api.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	paths, _ := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	if len(paths) != 2 {
		t.Errorf("matched files = %v, want the in-repo symlink to be followed", paths)
	}
}

func TestWalkSkipsSymlinkedDirectoriesByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := writeTree(t, map[string]string{"real/api.yaml": "kind: Deployment"})
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}

	paths, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})

	// Following would report the same manifest twice under two paths, and
	// the graph would gain a duplicate of every node in it.
	if len(paths) != 1 || paths[0] != "real/api.yaml" {
		t.Errorf("matched files = %v, want the aliased directory to be skipped", paths)
	}
	if !hasDiagnostic(res, "symlink_dir_skipped") {
		t.Error("skipping a directory symlink was not reported")
	}

	paths, _ = walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{FollowSymlinkedDirs: true})
	if len(paths) != 2 {
		t.Errorf("matched files = %v, want both paths when following is enabled", paths)
	}
}

func TestWalkIsDeterministic(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"z", "a", "m", "b", "q"} {
		files[n+"/service.yaml"] = "kind: Deployment"
		files[n+".yaml"] = "x: 1"
	}
	dir := writeTree(t, files)

	first, _ := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})
	for i := range 10 {
		again, _ := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})
		if strings.Join(first, ",") != strings.Join(again, ",") {
			t.Fatalf("walk %d returned a different order:\n%v\n%v", i, first, again)
		}
	}
	if !isSorted(first) {
		t.Errorf("walk output is not sorted by path: %v", first)
	}
}

func TestWalkOfEmptyDirectory(t *testing.T) {
	paths, res := walkPaths(t, t.TempDir(), NewRegistry(yamlOnly{}), WalkOptions{})
	if len(paths) != 0 {
		t.Errorf("matched files = %v, want none", paths)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", res.Scanned)
	}
}

func TestFileMetaFields(t *testing.T) {
	dir := writeTree(t, map[string]string{"deploy/prod/api.YAML": "kind: Deployment"})

	_, res := walkPaths(t, dir, NewRegistry(yamlOnly{}), WalkOptions{})
	if len(res.Files) != 1 {
		t.Fatalf("matched %d files, want 1 (extension matching must be case-insensitive)", len(res.Files))
	}
	f := res.Files[0]
	if f.Path != "deploy/prod/api.YAML" {
		t.Errorf("Path = %q", f.Path)
	}
	if f.Dir != "deploy/prod" {
		t.Errorf("Dir = %q, want deploy/prod", f.Dir)
	}
	if f.Name != "api.YAML" {
		t.Errorf("Name = %q", f.Name)
	}
	if f.Ext != ".yaml" {
		t.Errorf("Ext = %q, want the lowercased extension", f.Ext)
	}
	// Paths are slash-separated on every platform, including Windows, so
	// that a graph built on one machine is diffable against one built on
	// another.
	if strings.ContainsRune(f.Path, '\\') {
		t.Errorf("Path %q contains a backslash", f.Path)
	}
}

func TestWalkStopsOnCancelledContext(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.yaml": "x: 1", "b/c.yaml": "x: 1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Walk(ctx, dir, NewRegistry(yamlOnly{}), WalkOptions{}); err == nil {
		t.Error("Walk() = nil, want the cancellation to be returned")
	}
}

func hasDiagnostic(res WalkResult, code string) bool {
	for _, d := range res.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func isSorted(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}
