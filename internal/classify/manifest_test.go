package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"
)

// These five predicates decide which extractor claims a file, and nothing
// tested them. A wrong answer here is not a wrong edge -- it is a file that
// goes to the wrong parser or to none, and the failure surfaces as an absence
// rather than as an error.

func TestIsHelmTemplate(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    bool
	}{
		{
			name:    "a template under templates/",
			path:    "charts/api/templates/deployment.yaml",
			content: "image: {{ .Values.image.repository }}\n",
			want:    true,
		},
		{
			name:    "a chart at the repository root",
			path:    "templates/deployment.yaml",
			content: "image: {{ .Values.image.repository }}\n",
			want:    true,
		},
		{
			// The guard that earns this function its keep. A Kubernetes
			// manifest may legitimately contain braces in a command, an
			// annotation, or a Prometheus rule, and sending it to the Helm
			// path would lose a real node.
			name:    "braces in an ordinary manifest outside a chart",
			path:    "deploy/prod/api.yaml",
			content: "metadata:\n  annotations:\n    summary: \"{{ $labels.instance }} is down\"\n",
			want:    false,
		},
		{
			// _helpers.tpl and partials are templates wherever they sit, and
			// they open with a template action rather than YAML.
			name:    "a partial that opens with a template action",
			path:    "charts/api/_helpers.tpl",
			content: "{{/* Expand the name of the chart. */}}\n{{- define \"api.name\" -}}\n",
			want:    true,
		},
		{
			name:    "no template delimiters at all",
			path:    "charts/api/templates/NOTES.txt",
			content: "Thanks for installing.\n",
			want:    false,
		},
		{
			name:    "an empty file",
			path:    "charts/api/templates/empty.yaml",
			content: "",
			want:    false,
		},
		{
			// "templates" has to be a whole path segment. A directory named
			// templates-old or gotemplates is not a chart.
			name:    "a directory whose name merely contains templates",
			path:    "web/gotemplates/page.yaml",
			content: "title: {{ .Title }}\n",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify.IsHelmTemplate(tt.path, []byte(tt.content)); got != tt.want {
				t.Errorf("IsHelmTemplate(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestHelmChartDir(t *testing.T) {
	tests := []struct {
		path    string
		wantDir string
		wantOK  bool
	}{
		{"charts/api/templates/deployment.yaml", "charts/api", true},
		{"deploy/helm/web/templates/svc.yaml", "deploy/helm/web", true},
		{"charts/api/values.yaml", "", false},
		{"deploy/prod/api.yaml", "", false},
		{
			// A chart at the repository root has no directory above
			// templates/, and reporting "" would make the caller build
			// "/templates" -- an absolute path into nothing. Refusing lets
			// the caller fall back to the file's own directory, which is
			// already the right answer. This is deliberate, not a miss.
			path: "templates/deployment.yaml", wantDir: "", wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			dir, ok := classify.HelmChartDir(tt.path)
			if dir != tt.wantDir || ok != tt.wantOK {
				t.Errorf("HelmChartDir(%q) = (%q, %v), want (%q, %v)",
					tt.path, dir, ok, tt.wantDir, tt.wantOK)
			}
		})
	}
}

func TestIsKustomization(t *testing.T) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "kustomization", "Kustomization.yaml", "KUSTOMIZATION.YML"} {
		if !classify.IsKustomization(name) {
			t.Errorf("IsKustomization(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"kustomize.yaml", "kustomization.json", "my-kustomization.yaml", "", "values.yaml"} {
		if classify.IsKustomization(name) {
			t.Errorf("IsKustomization(%q) = true, want false", name)
		}
	}
}

func TestIsHelmChartFile(t *testing.T) {
	for _, name := range []string{"Chart.yaml", "chart.yaml", "Chart.yml", "CHART.YAML"} {
		if !classify.IsHelmChartFile(name) {
			t.Errorf("IsHelmChartFile(%q) = false, want true", name)
		}
	}
	// Chart.lock records resolved dependency versions and is not the chart
	// metadata; charts.yaml is not a Helm file at all.
	for _, name := range []string{"Chart.lock", "charts.yaml", "Chart.json", ""} {
		if classify.IsHelmChartFile(name) {
			t.Errorf("IsHelmChartFile(%q) = true, want false", name)
		}
	}
}

func TestIsHelmValuesFile(t *testing.T) {
	// Both separators are conventional for per-environment values, and a
	// repository that uses one for staging and the other for production
	// should not have half its values files ignored.
	for _, name := range []string{
		"values.yaml", "values.yml", "VALUES.YAML",
		"values-prod.yaml", "values.prod.yaml",
		"values-staging.yml", "values.eu-west-1.yaml",
	} {
		if !classify.IsHelmValuesFile(name) {
			t.Errorf("IsHelmValuesFile(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		// The JSON Schema beside values.yaml describes the values; it does
		// not contain any. bitnami ships one per chart, so reading them as
		// values would be 25 wrong answers in one repository.
		"values.schema.json",
		"myvalues.yaml", // not a values file
		// The separator is required. Without it "values" is a prefix that
		// swallows any file beginning with those six letters, and a chart
		// that keeps a valuesets.yaml would have it parsed as values.
		"valuesets.yaml",
		"values2.yaml",
		// Underscore is not among the conventional separators. Pinned as the
		// current answer rather than asserted to be the right one.
		"values_prod.yaml",
		"values",          // no extension
		"values.yaml.bak", // not YAML any more
		"",
	} {
		if classify.IsHelmValuesFile(name) {
			t.Errorf("IsHelmValuesFile(%q) = true, want false", name)
		}
	}
}
