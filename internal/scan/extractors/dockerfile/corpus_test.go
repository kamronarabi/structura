//go:build corpus

// The invariants this file checks cannot be established on fixtures, because
// the same person wrote the fixtures and the expectations. These run over the
// Dockerfiles in real repositories -- sixty-eight of them across seven
// projects, written by people who had never heard of this extractor -- and
// assert the properties that have to hold whatever is in the file.
//
// Run with `make corpus && go test -tags corpus ./internal/scan/...`.
package dockerfile_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/dockerfile"
	"github.com/kamronarabi/structura/pkg/schema"
)

// corpusDockerfiles returns every file in the cloned corpus that this
// extractor claims.
func corpusDockerfiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "testdata", "corpus")
	if _, err := os.Stat(root); err != nil {
		t.Skip("corpus not cloned; run `make corpus`")
	}
	e := dockerfile.New()

	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not this test's subject
		}
		name := d.Name()
		if e.Match(scan.FileMeta{Name: name, Ext: strings.ToLower(filepath.Ext(name))}) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the corpus: %v", err)
	}
	if len(out) == 0 {
		t.Skip("corpus holds no Dockerfiles")
	}
	sort.Strings(out)
	return out
}

func extractCorpusFile(t *testing.T, root, p string) *capture {
	t.Helper()
	content, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		t.Fatalf("%s is not under %s", p, root)
	}
	rel = filepath.ToSlash(rel)
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		dir = ""
	}
	c := &capture{}
	f := &scan.File{
		FileMeta: scan.FileMeta{
			Path: rel, Dir: dir, Name: filepath.Base(rel),
			Ext: strings.ToLower(filepath.Ext(rel)), Size: int64(len(content)),
		},
		Content: content,
	}
	if err := dockerfile.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract(%s) = %v", rel, err)
	}
	return c
}

// Every real Dockerfile has to produce exactly one component, or say why not.
// Silence would mean a file was read and quietly contributed nothing.
func TestEveryCorpusDockerfileYieldsAComponentOrAReason(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "testdata", "corpus")
	for _, p := range corpusDockerfiles(t) {
		c := extractCorpusFile(t, root, p)
		switch {
		case len(c.nodes) == 1:
		case len(c.nodes) == 0 && len(c.diags) > 0:
		default:
			t.Errorf("%s: %d nodes and %d diagnostics, want one component or a reason",
				p, len(c.nodes), len(c.diags))
		}
	}
}

// The properties a wrong answer would be built from. A name that is a
// directory's role, a port outside the range, or a base image that is really a
// stage label are each a thing a reader would take on trust.
func TestCorpusComponentsAreWellFormed(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "testdata", "corpus")
	for _, p := range corpusDockerfiles(t) {
		for _, n := range extractCorpusFile(t, root, p).nodes {
			if strings.TrimSpace(n.Name) == "" {
				t.Errorf("%s: a component with no name", p)
			}
			if !n.Kind.Valid() {
				t.Errorf("%s: Kind = %q, which is not a valid node kind", p, n.Kind)
			}
			if n.Attrs["directory"] == nil {
				t.Errorf("%s: no directory, so nothing can bind to it", p)
			}
			if n.Attrs["nameFrom"] != "directory" {
				t.Errorf("%s: nameFrom = %v; the resolver needs to know the name is derived",
					p, n.Attrs["nameFrom"])
			}
			// A base that is a stage label means the stage chain was not
			// followed, and the language and kind were read off a word.
			base, _ := n.Attrs["baseImage"].(string)
			if base == "" {
				t.Errorf("%s: no base image", p)
			}
			for _, label := range []string{"builder", "build", "base", "development", "dev", "final", "runtime"} {
				if strings.EqualFold(base, label) {
					t.Errorf("%s: baseImage = %q, which is a stage label, not an image", p, base)
				}
			}
			for _, port := range portsOf(n) {
				if port <= 0 || port > 65535 {
					t.Errorf("%s: port %d is not a port", p, port)
				}
			}
		}
	}
}

func portsOf(n schema.Node) []int {
	ports, _ := n.Attrs["ports"].([]int)
	return ports
}

// Reading the same file twice must give the same answer, because the whole
// cache and golden apparatus above this assumes it.
func TestCorpusExtractionIsDeterministic(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "testdata", "corpus")
	for _, p := range corpusDockerfiles(t) {
		first := extractCorpusFile(t, root, p)
		second := extractCorpusFile(t, root, p)
		if len(first.nodes) != len(second.nodes) {
			t.Fatalf("%s: two readings disagreed on how many components", p)
		}
		for i := range first.nodes {
			a, b := first.nodes[i], second.nodes[i]
			if a.ID != b.ID || a.Name != b.Name || a.Kind != b.Kind {
				t.Errorf("%s: two readings disagreed:\n  %+v\n  %+v", p, a, b)
			}
			if len(portsOf(a)) != len(portsOf(b)) {
				t.Errorf("%s: two readings disagreed on ports", p)
			}
		}
	}
}

// A component named after a directory that describes its role is a box a
// reader cannot place, and three of them in one repository all say "image".
func TestNoCorpusComponentIsNamedForADirectorysRole(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "testdata", "corpus")
	generic := map[string]bool{
		"src": true, "image": true, "images": true, "docker": true,
		"build": true, "deploy": true, "app": true, "dist": true, "bin": true,
	}
	for _, p := range corpusDockerfiles(t) {
		for _, n := range extractCorpusFile(t, root, p).nodes {
			if generic[strings.ToLower(n.Name)] {
				t.Errorf("%s: named %q, which describes a directory rather than a component",
					p, n.Name)
			}
		}
	}
}
