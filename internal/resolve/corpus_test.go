//go:build corpus

// Package resolve's corpus test scores the resolver against real
// repositories.
//
// It is behind a build tag because it needs repositories cloned from the
// network, which makes it too slow and too fragile for the default suite.
// Run it with `make corpus && make corpus-test`.
//
// Synthetic fixtures cannot answer the question this does. The same person
// wrote their input and their expectation, so they measure whether the
// extractors do what their author intended — not whether the resolver copes
// with what people actually commit.
package resolve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// PrecisionGate is the threshold CI enforces.
//
// Precision and recall are not equally important here. A missing edge is a
// visible gap — the diagram looks sparse and the user notices. A wrong edge is
// invisible and actively harmful: it flows into the MCP server, the model
// reasons on top of it, and the bottleneck profiler produces confident
// recommendations about a dependency that does not exist. So precision is
// gated and recall is reported as a trend to improve.
const PrecisionGate = 0.90

type expectation struct {
	Repo    string `yaml:"repo"`
	Source  string `yaml:"source"`
	Edges   []pair `yaml:"edges"`
	Allowed []pair `yaml:"allowed"`
	// Oracle, when present, lets the expectations themselves be checked
	// against the application source. See oracle_test.go.
	Oracle *oracleSpec `yaml:"oracle"`
}

type pair struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

func (p pair) String() string { return p.From + " -> " + p.To }

// matches supports "*" on either side, for whole classes of correct-but-not-
// required edges such as "everything logs to log-server".
func (p pair) matches(from, to string) bool {
	return (p.From == "*" || p.From == from) && (p.To == "*" || p.To == to)
}

func TestResolverPrecisionOnCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "corpus-expected", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no expectation files found")
	}
	sort.Strings(files)

	var report strings.Builder
	report.WriteString("\n")
	fmt.Fprintf(&report, "%-22s %-11s %-11s %s\n", "REPO", "PRECISION", "RECALL", "DETAIL")
	report.WriteString(strings.Repeat("-", 92) + "\n")

	var failures []string
	perRule := map[string]*ruleScore{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var want expectation
		if err := yaml.Unmarshal(data, &want); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		root := filepath.Join("..", "..", "testdata", "corpus", want.Repo)
		if _, err := os.Stat(root); err != nil {
			t.Logf("%s not cloned; run `make corpus`", want.Repo)
			continue
		}

		score := scoreRepo(t, root, want)
		for name, rs := range score.byRule {
			if perRule[name] == nil {
				perRule[name] = &ruleScore{}
			}
			perRule[name].found += rs.found
			perRule[name].wrong += rs.wrong
		}
		fmt.Fprintf(&report, "%-22s %-11.2f %-11.2f %d found, %d wrong, %d missed\n",
			want.Repo, score.precision, score.recall,
			score.found, len(score.falsePositives), len(score.missed))

		if score.precision < PrecisionGate {
			failures = append(failures, fmt.Sprintf(
				"%s: precision %.2f is below the %.2f gate\n  false positives:\n    %s",
				want.Repo, score.precision, PrecisionGate,
				strings.Join(score.falsePositives, "\n    ")))
		}
		if len(score.missed) > 0 {
			report.WriteString("  missed: " + strings.Join(score.missed, ", ") + "\n")
		}
		if len(score.falsePositives) > 0 {
			report.WriteString("  wrong:  " + strings.Join(score.falsePositives, ", ") + "\n")
		}
	}

	writeRuleReport(&report, perRule)
	t.Log(report.String())
	for _, f := range failures {
		t.Error(f)
	}
}

// writeRuleReport prints what each rule's edges were worth, next to the
// confidence its edges carry.
//
// Those constants are the project's central claim: an edge says how much it
// should be believed. But they are hand-set priors, and a prior presented
// with two decimal places reads as a measured frequency. This is the counting
// that would tell them apart. It is reported rather than gated, because a
// handful of repositories cannot establish a rate -- what it can do is
// attribute a wrong edge to the rule that produced it the moment one appears.
func writeRuleReport(report *strings.Builder, perRule map[string]*ruleScore) {
	if len(perRule) == 0 {
		return
	}
	names := make([]string, 0, len(perRule))
	for n := range perRule {
		names = append(names, n)
	}
	sort.Strings(names)

	report.WriteString("\nPER-RULE (all repositories)\n")
	fmt.Fprintf(report, "  %-28s %-9s %-7s %s\n", "RULE", "PRECISION", "EDGES", "WRONG")
	report.WriteString("  " + strings.Repeat("-", 60) + "\n")
	for _, n := range names {
		r := perRule[n]
		fmt.Fprintf(report, "  %-28s %-9.2f %-7d %d\n", n, r.precision(), r.found, r.wrong)
	}
}

type score struct {
	precision, recall float64
	found             int
	falsePositives    []string
	missed            []string
	// byRule attributes each found edge to the rules that produced it, so a
	// wrong edge names the rule to fix. It is also the only way to check the
	// confidence constants against anything: each is a claim about how often
	// its rule is right, and until now nothing counted.
	byRule map[string]*ruleScore
}

type ruleScore struct{ found, wrong int }

func (r *ruleScore) precision() float64 {
	if r.found == 0 {
		return 1
	}
	return float64(r.found-r.wrong) / float64(r.found)
}

func scoreRepo(t *testing.T, root string, want expectation) score {
	t.Helper()

	res, err := scan.Run(context.Background(), extractors.Default(), scan.Options{Root: root})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}

	names := map[string]string{}
	for _, n := range res.Graph.Nodes {
		names[n.ID] = n.Name
	}

	// Containment is structure, not a relationship anyone would draw.
	type found struct{ from, to string }
	seen := map[found]bool{}
	rules := map[found]map[string]bool{}
	for _, e := range res.Graph.Edges {
		if e.Kind == schema.EdgeContains {
			continue
		}
		f := found{names[e.From], names[e.To]}
		seen[f] = true
		if rules[f] == nil {
			rules[f] = map[string]bool{}
		}
		for _, ev := range e.Evidence {
			rules[f][ev.Rule] = true
		}
	}

	var s score
	s.found = len(seen)
	s.byRule = map[string]*ruleScore{}
	rule := func(name string) *ruleScore {
		if s.byRule[name] == nil {
			s.byRule[name] = &ruleScore{}
		}
		return s.byRule[name]
	}

	matchedExpected := map[string]bool{}
	for f := range seen {
		isExpected := false
		for _, want := range want.Edges {
			if want.matches(f.from, f.to) {
				isExpected = true
				matchedExpected[want.String()] = true
			}
		}
		for r := range rules[f] {
			rule(r).found++
		}
		if isExpected {
			continue
		}
		isAllowed := false
		for _, allow := range want.Allowed {
			if allow.matches(f.from, f.to) {
				isAllowed = true
				break
			}
		}
		if !isAllowed {
			s.falsePositives = append(s.falsePositives, f.from+" -> "+f.to)
			for r := range rules[f] {
				rule(r).wrong++
			}
		}
	}

	for _, want := range want.Edges {
		if !matchedExpected[want.String()] {
			s.missed = append(s.missed, want.String())
		}
	}
	sort.Strings(s.falsePositives)
	sort.Strings(s.missed)

	correct := s.found - len(s.falsePositives)
	if s.found > 0 {
		s.precision = float64(correct) / float64(s.found)
	} else {
		s.precision = 1
	}
	if len(want.Edges) > 0 {
		s.recall = float64(len(want.Edges)-len(s.missed)) / float64(len(want.Edges))
	} else {
		s.recall = 1
	}
	return s
}
