//go:build corpus

package resolve_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kamronarabi/structura/internal/golden"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Four of the seven pinned repositories were cloned by `make corpus` and then
// touched by nothing. Only the three with hand-written expectation files were
// ever scanned, so "measured against real repositories" rested on three, and
// a change that silently altered what the other four produced would have gone
// unseen -- including bitnami-charts, which is the only thing here at a scale
// where cost and pathological input show up.
//
// They cannot be scored for correctness, and pretending otherwise would just
// recreate the problem the oracle test exists to prevent: none of them has an
// architecture to get right. kubernetes-examples is a grab-bag, awesome-compose
// is a hundred unrelated stacks, terraform-aws-vpc is a module library, and
// bitnami-charts yields four real edges from three thousand files. They were
// chosen as robustness and scale cases, and this measures them as such.
//
// What is pinned is the shape of the result: how much was found, of what kinds,
// by which rules, and what the scan could not read. The input is pinned by
// commit, so the only thing that can move these numbers is a change to
// Structura -- which is exactly when someone should look. It measures
// stability, not correctness, and the distinction is the point.
func TestEveryCorpusRepositoryHasAPinnedShape(t *testing.T) {
	for _, repo := range corpusRepos(t) {
		t.Run(repo, func(t *testing.T) {
			root := filepath.Join("..", "..", "testdata", "corpus", repo)
			if _, err := os.Stat(root); err != nil {
				t.Skipf("%s not cloned; run `make corpus`", repo)
			}

			first, err := scan.Run(context.Background(), extractors.Default(), scan.Options{Root: root})
			if err != nil {
				t.Fatalf("scanning %s: %v", repo, err)
			}

			// Determinism is checked here rather than only on the synthetic
			// fixtures, because map iteration and walk order have far more
			// room to show through on three thousand files than on eight.
			second, err := scan.Run(context.Background(), extractors.Default(), scan.Options{Root: root})
			if err != nil {
				t.Fatalf("rescanning %s: %v", repo, err)
			}
			a, err := schema.Marshal(first.Graph.Canonical())
			if err != nil {
				t.Fatal(err)
			}
			b, err := schema.Marshal(second.Graph.Canonical())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Errorf("two scans of %s differ; the output is not deterministic", repo)
			}

			golden.Assert(t, filepath.Join("testdata", "corpus-shape", repo+".txt"),
				shapeReport(repo, first.Graph))
		})
	}
}

// corpusRepos reads the manifest rather than listing the directory, so a
// repository that failed to clone is a skip with a name attached instead of
// silently reducing what is covered.
func corpusRepos(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "corpus.yaml"))
	if err != nil {
		t.Fatalf("reading the corpus manifest: %v", err)
	}
	var manifest struct {
		Repos []struct {
			Name string `yaml:"name"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parsing the corpus manifest: %v", err)
	}
	if len(manifest.Repos) == 0 {
		t.Fatal("the corpus manifest lists no repositories")
	}

	names := make([]string, 0, len(manifest.Repos))
	for _, r := range manifest.Repos {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

// shapeReport renders the summary that gets pinned. Counts rather than
// contents: a diff should say what moved, which a content hash cannot.
func shapeReport(repo string, g schema.Graph) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n", repo)
	fmt.Fprintf(&b, "  files       %d scanned, %d parsed\n", g.Stats.FilesScanned, g.Stats.FilesParsed)
	fmt.Fprintf(&b, "  nodes       %d\n", g.Stats.NodeCount)
	fmt.Fprintf(&b, "  edges       %d\n", g.Stats.EdgeCount)
	fmt.Fprintf(&b, "  connected   %d of %d components\n", g.Stats.Connected, components(g))
	fmt.Fprintf(&b, "  clusters    %d\n", g.Stats.Clusters)

	byKind := map[string]int{}
	byLayer := map[string]int{}
	for _, n := range g.Nodes {
		byKind[string(n.Kind)]++
		byLayer[string(n.Layer)]++
	}
	edgeKind := map[string]int{}
	byRule := map[string]int{}
	for _, e := range g.Edges {
		edgeKind[string(e.Kind)]++
		seen := map[string]bool{}
		for _, ev := range e.Evidence {
			if seen[ev.Rule] {
				continue
			}
			seen[ev.Rule] = true
			byRule[ev.Rule]++
		}
	}
	byCode := map[string]int{}
	for _, d := range g.Diagnostics {
		byCode[string(d.Severity)+" "+d.Code]++
	}

	section(&b, "nodes by kind", byKind)
	section(&b, "nodes by layer", byLayer)
	section(&b, "edges by kind", edgeKind)
	section(&b, "edges by rule", byRule)
	section(&b, "diagnostics", byCode)
	return b.Bytes()
}

func section(b *bytes.Buffer, title string, counts map[string]int) {
	fmt.Fprintf(b, "\n  %s\n", title)
	if len(counts) == 0 {
		fmt.Fprintf(b, "    (none)\n")
		return
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "    %-34s %d\n", k, counts[k])
	}
}

func components(g schema.Graph) int {
	var n int
	for _, node := range g.Nodes {
		if node.Kind != schema.KindBoundary {
			n++
		}
	}
	return n
}
