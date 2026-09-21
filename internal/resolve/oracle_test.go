//go:build corpus

package resolve_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The expectation files are written by hand, and until now nothing checked
// that. Their README claims they were derived from each project's own
// documentation before looking at Structura's output -- which is the right
// method, but it is a claim about a process, and a process claim is exactly
// what an author cannot verify about themselves.
//
// The bias it invites is not inventing edges. It is scoping: writing down the
// relationships the tool can see and not the ones it cannot, which makes
// recall a measurement of the author's imagination rather than of the
// resolver. It happened: online-boutique's expectation file did not mention
// that the frontend reads PACKAGING_SERVICE_URL, and nothing else here would
// have noticed.
//
// What the check turned up alongside it is the more instructive case. Seven
// services read COLLECTOR_SERVICE_ADDR. Taking that at face value moved seven
// edges into `edges` and recall to 0.71 -- against a collector that
// helm-chart/values.yaml disables, the manifests comment out, and only an
// opt-in Kustomize component deploys. A relationship derived from source is
// evidence that the application can do something, not that any deployment
// described here does.
//
// So this fails on an unaccounted finding, not on a missing edge. Which one it
// is -- a real dependency, or a capability nothing wires up -- is a judgement,
// and it belongs in the expectation file next to the evidence for it. What
// must not happen is the finding being dropped without anyone reaching one.
//
// So this derives the same relationships from a source Structura does not
// read: the application code. A twelve-factor service names its dependencies
// in environment variables, configuration supplies them, and the source
// consumes them -- two independent statements of one fact, in different files
// and different languages. An edge the source states and the expectation file
// does not mention is either a missing expectation or an alias nobody wrote
// down, and both are worth failing over.
//
// This checks the expectations, not the resolver. Whether Structura finds a
// derived edge is the corpus test's business; whether the expectation file
// admits the edge exists is this one's.
type oracleSpec struct {
	// SourceGlob selects the service directories, relative to the repository
	// root. The directory name is the calling service.
	SourceGlob string `yaml:"sourceGlob"`
	// Aliases map an environment-variable stem to the component it names,
	// for the cases where they differ. Each one is a small, checkable claim
	// about the repository, unlike a whole hand-written edge list.
	Aliases map[string]string `yaml:"aliases"`
	// Ignore lists stems that do not name another component: a service's own
	// listen address, a port, a bind host.
	Ignore []string `yaml:"ignore"`
}

// addressVar matches the environment variables a service reads to find
// another one. The suffixes are the conventional ones; the stem is what names
// the dependency.
var addressVar = regexp.MustCompile(`\b([A-Z][A-Z0-9_]*?)_(?:ADDR|ADDRESS|URL|URI|HOST|ENDPOINT)\b`)

var sourceExtensions = map[string]bool{
	".go": true, ".cs": true, ".py": true, ".js": true,
	".ts": true, ".java": true, ".rb": true, ".php": true,
}

// vendored directories hold other people's code, which names its own
// dependencies and not this service's.
var vendored = []string{
	"node_modules", "vendor", "genproto", "/bin/", "/obj/",
	"site-packages", "third_party", "/proto/", "/gen/",
}

func TestExpectationsAgreeWithTheApplicationSource(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "corpus-expected", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var want expectation
		if err := yaml.Unmarshal(data, &want); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		if want.Oracle == nil {
			// Not every repository can be checked this way. sock-shop ships
			// no application source at all, and podinfo is one program rather
			// than a set of services. Saying so is better than pretending the
			// check covers them.
			t.Logf("%s: no oracle configured; the expectations there are unchecked", want.Repo)
			continue
		}

		root := filepath.Join("..", "..", "testdata", "corpus", want.Repo)
		if _, err := os.Stat(root); err != nil {
			t.Logf("%s not cloned; run `make corpus`", want.Repo)
			continue
		}
		checked++

		derived, err := deriveFromSource(root, *want.Oracle)
		if err != nil {
			t.Fatalf("%s: %v", want.Repo, err)
		}
		if len(derived) == 0 {
			t.Errorf("%s: the oracle derived nothing, so it is checking nothing", want.Repo)
			continue
		}

		var unaccounted []string
		for _, d := range derived {
			if accountedFor(d, want) {
				continue
			}
			unaccounted = append(unaccounted, d.String())
		}
		sort.Strings(unaccounted)

		t.Logf("%s: %d relationships derived from application source, %d unaccounted for",
			want.Repo, len(derived), len(unaccounted))
		if len(unaccounted) > 0 {
			t.Errorf("%s: the application source states relationships the expectation file "+
				"does not mention. Each is a missing expectation or an undeclared alias, "+
				"and leaving them out makes recall measure what the tool can see rather "+
				"than what is there:\n    %s",
				want.Repo, strings.Join(unaccounted, "\n    "))
		}
	}

	if checked == 0 {
		t.Skip("no repository had both an oracle and a clone")
	}
}

// accountedFor reports whether the expectation file mentions a relationship
// at all, in either list. Which list it is in is the corpus test's question.
func accountedFor(d pair, want expectation) bool {
	for _, e := range want.Edges {
		if e.matches(d.From, d.To) {
			return true
		}
	}
	for _, a := range want.Allowed {
		if a.matches(d.From, d.To) {
			return true
		}
	}
	return false
}

// deriveFromSource reads each service's own code for the addresses it looks
// up, which is the dependency stated by the consumer rather than by the
// deployment.
func deriveFromSource(root string, spec oracleSpec) ([]pair, error) {
	dirs, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(spec.SourceGlob)))
	if err != nil {
		return nil, fmt.Errorf("bad sourceGlob %q: %w", spec.SourceGlob, err)
	}

	ignored := map[string]bool{}
	for _, s := range spec.Ignore {
		ignored[s] = true
	}

	seen := map[string]bool{}
	var out []pair
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		caller := filepath.Base(dir)

		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !sourceExtensions[strings.ToLower(filepath.Ext(p))] {
				return nil //nolint:nilerr // an unreadable file is not a reason to abandon the walk
			}
			slashed := filepath.ToSlash(p)
			for _, v := range vendored {
				if strings.Contains(slashed, v) {
					return nil
				}
			}
			content, err := os.ReadFile(p) //nolint:gosec // a corpus path under testdata
			if err != nil {
				return nil //nolint:nilerr // an unreadable file is not a reason to abandon the walk
			}
			for _, m := range addressVar.FindAllSubmatch(content, -1) {
				stem := string(m[1])
				if ignored[stem] {
					continue
				}
				callee, ok := spec.Aliases[stem]
				if !ok {
					// The convention: PRODUCT_CATALOG_SERVICE_ADDR names
					// productcatalogservice. Where it does not hold, the
					// expectation file declares an alias.
					callee = strings.ToLower(strings.ReplaceAll(stem, "_", ""))
				}
				if callee == "" || callee == caller {
					continue
				}
				p := pair{From: caller, To: callee}
				if seen[p.String()] {
					continue
				}
				seen[p.String()] = true
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}
