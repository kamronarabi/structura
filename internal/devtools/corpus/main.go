// Command corpus clones the pinned real-world repositories used by the slow
// resolver tests.
//
// They are not committed and not vendored: they are large, they are other
// people's code, and pinning them by commit gives reproducibility without the
// weight. Cloning is shallow and single-commit.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type manifest struct {
	Repos []struct {
		Name  string `yaml:"name"`
		URL   string `yaml:"url"`
		Ref   string `yaml:"ref"`
		Shape string `yaml:"shape"`
		Note  string `yaml:"note"`
	} `yaml:"repos"`
}

func main() {
	config := flag.String("config", "testdata/corpus.yaml", "corpus manifest")
	dest := flag.String("dest", "testdata/corpus", "where to clone")
	only := flag.String("only", "", "clone just this repository")
	flag.Parse()

	if err := run(*config, *dest, *only); err != nil {
		fmt.Fprintf(os.Stderr, "corpus: %v\n", err)
		os.Exit(1)
	}
}

func run(config, dest, only string) error {
	data, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parsing %s: %w", config, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	var failed int
	for _, repo := range m.Repos {
		if only != "" && repo.Name != only {
			continue
		}
		target := filepath.Join(dest, repo.Name)
		if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
			fmt.Printf("  %-22s already present\n", repo.Name)
			continue
		}
		fmt.Printf("  %-22s cloning %s\n", repo.Name, repo.Shape)
		if err := clone(repo.URL, repo.Ref, target); err != nil {
			// One unreachable or moved repository must not stop the rest:
			// this is a developer convenience, not a build step.
			fmt.Fprintf(os.Stderr, "  %-22s FAILED: %v\n", repo.Name, err)
			failed++
			continue
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d of %d repositories could not be cloned\n", failed, len(m.Repos))
	}
	return nil
}

// clone fetches exactly one commit, which is far cheaper than a full history
// and is all a fixture needs.
func clone(url, ref, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", url},
		{"fetch", "--quiet", "--depth", "1", "origin", ref},
		{"checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = target
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(target)
			return fmt.Errorf("git %s: %w\n%s", args[0], err, out)
		}
	}
	return nil
}
