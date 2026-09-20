package classify

import (
	"bytes"
	"path"
	"strings"
)

// IsHelmTemplate reports whether a file is a Helm chart template.
//
// This matters more than it sounds. If a repository deploys to Kubernetes,
// there is a good chance it does so through Helm, and a chart template is
// not valid YAML — "{{ .Values.image.repository }}" is a Go template
// expression sitting where a value belongs. Feeding one to a YAML parser
// produces an error, not a node, so a tool that does not recognize them
// reports a repository full of broken files instead of a chart it chose not
// to render.
func IsHelmTemplate(filePath string, content []byte) bool {
	if !bytes.Contains(content, []byte("{{")) {
		return false
	}
	// Templates live under a chart's templates/ directory. Requiring that as
	// well as the delimiters avoids claiming an ordinary manifest that
	// happens to contain braces in a command or an annotation.
	for _, seg := range strings.Split(path.Dir(filePath), "/") {
		if seg == "templates" {
			return true
		}
	}
	// A file whose very first non-comment content is a template action is a
	// template wherever it lives — _helpers.tpl and partials in particular.
	trimmed := bytes.TrimSpace(content)
	return bytes.HasPrefix(trimmed, []byte("{{"))
}

// HelmChartDir returns the chart directory for a path inside a chart, and
// whether the path is inside one.
func HelmChartDir(filePath string) (dir string, ok bool) {
	segments := strings.Split(filePath, "/")
	for i, seg := range segments {
		if seg == "templates" && i > 0 {
			return strings.Join(segments[:i], "/"), true
		}
	}
	return "", false
}

// IsKustomization reports whether a filename is a Kustomize entry point.
func IsKustomization(name string) bool {
	switch strings.ToLower(name) {
	case "kustomization.yaml", "kustomization.yml", "kustomization":
		return true
	default:
		return false
	}
}

// IsHelmChartFile reports whether a filename is a chart's metadata file.
func IsHelmChartFile(name string) bool {
	lower := strings.ToLower(name)
	return lower == "chart.yaml" || lower == "chart.yml"
}

// IsHelmValuesFile reports whether a filename is a chart's values file,
// including the per-environment variants repositories conventionally keep
// beside it.
func IsHelmValuesFile(name string) bool {
	lower := strings.ToLower(name)
	if lower == "values.yaml" || lower == "values.yml" {
		return true
	}
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return false
	}
	// Both separators are in common use for per-environment values:
	// values-prod.yaml and values.prod.yaml are equally conventional.
	return strings.HasPrefix(lower, "values.") || strings.HasPrefix(lower, "values-")
}
