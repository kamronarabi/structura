package scan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Some gaps are discovered once per file but are really one fact about a
// directory. A Helm chart with eighteen templates is not eighteen problems;
// it is one chart that was not rendered. Reporting it eighteen times buries
// every other diagnostic in the list, and the list is the first thing a user
// reads when the graph looks thinner than they expected.
//
// Aggregation happens after the merge rather than inside the extractor,
// because an extractor sees one file and cannot know there were seventeen
// others.

// aggregator builds the collapsed message from the number of occurrences and
// the distinct detail each one carried.
//
// The details matter as much as the count. "3 resource types were skipped"
// tells a user something is missing; "aws_waf_rule, aws_cognito_user_pool and
// aws_kinesis_analytics_application were skipped" tells them what to ask for.
type aggregator func(count int, details []string) string

var aggregators = map[string]aggregator{
	"helm_unrendered": func(n int, _ []string) string {
		if n == 1 {
			return "a Helm template was not rendered; components declared only in this chart are missing from the graph"
		}
		return fmt.Sprintf(
			"%d Helm templates not rendered; components declared only in this chart are missing from the graph", n)
	},
	"kustomize_unrendered": func(n int, _ []string) string {
		if n == 1 {
			return "Kustomize overlay not applied; the manifests it lists were read directly, so its patches, name prefixes, and namespace are not reflected"
		}
		return fmt.Sprintf(
			"%d Kustomize overlays not applied; the manifests they list were read directly, so their patches, name prefixes, and namespaces are not reflected", n)
	},
	"k8s_patch_fragment": func(n int, _ []string) string {
		if n == 1 {
			return "a manifest declaring no pod template was read as a patch rather than a component; its overrides are not reflected"
		}
		return fmt.Sprintf(
			"%d manifests declaring no pod template were read as patches rather than components; their overrides are not reflected", n)
	},
	"terraform_unrecognized_resource": func(_ int, details []string) string {
		return fmt.Sprintf(
			"%s not in the recognized resource set and %s skipped: %s",
			countNoun(len(details), "1 resource type is", "%d resource types are"),
			plural(len(details), "was", "were"),
			list(details))
	},
	"terraform_unresolved_reference": func(n int, details []string) string {
		return fmt.Sprintf(
			"%s a value supplied at apply time, so edges through %s may be missing: %s",
			countNoun(n, "1 reference points at", "%d references point at"),
			plural(n, "it", "them"),
			list(details))
	},
	"terraform_unexpanded": func(_ int, details []string) string {
		return fmt.Sprintf(
			"%s count or for_each, which is not expanded; the graph shows one node each however many instances deploy: %s",
			countNoun(len(details), "1 resource uses", "%d resources use"),
			list(details))
	},
}

func countNoun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// list renders distinct details, capped so that a pathological file cannot
// produce a diagnostic nobody will read.
func list(details []string) string {
	const limit = 8
	if len(details) <= limit {
		return strings.Join(details, ", ")
	}
	return strings.Join(details[:limit], ", ") +
		fmt.Sprintf(" and %d more", len(details)-limit)
}

// aggregateDiagnostics collapses repeated diagnostics that share a code and a
// path into a single entry.
func aggregateDiagnostics(in []schema.Diagnostic) []schema.Diagnostic {
	type key struct{ code, path string }

	counts := map[key]int{}
	details := map[key][]string{}
	seenDetail := map[key]map[string]bool{}

	for _, d := range in {
		if _, ok := aggregators[d.Code]; !ok {
			continue
		}
		k := key{d.Code, d.Path}
		counts[k]++
		if seenDetail[k] == nil {
			seenDetail[k] = map[string]bool{}
		}
		if d.Message != "" && !seenDetail[k][d.Message] {
			seenDetail[k][d.Message] = true
			details[k] = append(details[k], d.Message)
		}
	}
	if len(counts) == 0 {
		return in
	}
	for k := range details {
		sort.Strings(details[k])
	}

	out := make([]schema.Diagnostic, 0, len(in))
	emitted := map[key]bool{}
	for _, d := range in {
		format, aggregated := aggregators[d.Code]
		if !aggregated {
			out = append(out, d)
			continue
		}
		k := key{d.Code, d.Path}
		if emitted[k] {
			continue
		}
		emitted[k] = true
		d.Message = format(counts[k], details[k])
		// The line number of whichever file happened to be first is
		// meaningless once the entry covers a whole directory.
		d.Line = 0
		out = append(out, d)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Code < out[j].Code
	})
	return out
}
