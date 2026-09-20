// Package helm extracts what a Helm chart states about itself without
// rendering it.
//
// Rendering templates is out of scope for Phase 1 (see k8s/helm.go for why),
// but a chart is not opaque. Chart.yaml declares the chart's dependencies —
// a chart that depends on bitnami/postgresql has a Postgres in it, stated
// outright — and values.yaml carries the image, the replica count, the
// service port, and often the connection strings, because those are exactly
// the things a chart parameterizes.
//
// That is a real slice of the architecture, and reading it is the difference
// between a Helm-deployed repository producing an empty graph and producing a
// sparse but honest one. Everything here is marked with the confidence it
// deserves and a diagnostic saying where it came from.
package helm

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/yamlpos"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "helm"

// Extractor reads Helm chart metadata and values.
type Extractor struct{}

// New returns a Helm extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// Match implements scan.Extractor.
func (*Extractor) Match(f scan.FileMeta) bool {
	if f.Ext != ".yaml" && f.Ext != ".yml" {
		return false
	}
	return classify.IsHelmChartFile(f.Name) || classify.IsHelmValuesFile(f.Name)
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	if classify.IsHelmChartFile(f.Name) {
		return e.extractChart(f, emit)
	}
	return e.extractValues(f, emit)
}

type chartMeta struct {
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	AppVersion   string `yaml:"appVersion"`
	Description  string `yaml:"description"`
	Type         string `yaml:"type"`
	Dependencies []struct {
		Name       string `yaml:"name"`
		Version    string `yaml:"version"`
		Repository string `yaml:"repository"`
		Alias      string `yaml:"alias"`
		Condition  string `yaml:"condition"`
	} `yaml:"dependencies"`
}

func (e *Extractor) extractChart(f *scan.File, emit scan.Emitter) error {
	var chart chartMeta
	if err := yaml.Unmarshal(f.Content, &chart); err != nil {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityWarn, Code: "helm_chart_parse_failed",
			Path: f.Path, Message: fmt.Sprintf("could not read Chart.yaml: %v", err),
		})
		return nil
	}
	if chart.Name == "" {
		// A Chart.yaml with no name is not a chart.
		return nil
	}

	pos := yamlpos.New(f.Content)
	line := pos.Line("name")
	src := schema.Source{Extractor: Name, Path: f.Path, Line: line}

	boundaryID := schema.NewNodeID(schema.KindBoundary, Name, chart.Name, chart.Name)
	attrs := schema.Attrs{"chartVersion": chart.Version, "packaging": "helm"}
	if chart.AppVersion != "" {
		attrs["appVersion"] = chart.AppVersion
	}
	emit.Node(schema.Node{
		ID: boundaryID, Kind: schema.KindBoundary, Layer: schema.LayerContext,
		Name: chart.Name, Namespace: chart.Name,
		Attrs: attrs, Sources: []schema.Source{src},
		Confidence: schema.ConfDeclared,
	})

	for _, dep := range chart.Dependencies {
		name := dep.Name
		if dep.Alias != "" {
			name = dep.Alias
		}
		if name == "" {
			continue
		}
		// A subchart's name is the product name by convention — the
		// postgresql subchart deploys Postgres — which is the same signal an
		// image reference carries.
		kind, tech, _ := classify.ImageKind(dep.Name)
		depID := schema.NewNodeID(kind, Name, chart.Name, name)

		depAttrs := schema.Attrs{"chart": dep.Name, "packaging": "helm-subchart"}
		if dep.Version != "" {
			depAttrs["chartVersion"] = dep.Version
		}
		if dep.Repository != "" {
			depAttrs["chartRepository"] = dep.Repository
		}
		if dep.Condition != "" {
			// A conditional dependency may be switched off by values. The
			// node is still emitted — a chart that can deploy Postgres is
			// worth knowing about — but the condition travels with it.
			depAttrs["condition"] = dep.Condition
		}

		emit.Node(schema.Node{
			ID: depID, Kind: kind, Layer: schema.LayerContainer,
			Name: name, Namespace: chart.Name,
			Tech:    &schema.Tech{Runtime: "kubernetes", Framework: tech},
			Attrs:   depAttrs,
			Sources: []schema.Source{src},
			// Declared in the chart, but whether it is actually deployed
			// depends on values this extractor is not rendering.
			Confidence: schema.ConfReference,
		})
		emit.Edge(schema.Edge{
			From: boundaryID, To: depID, Kind: schema.EdgeContains,
			Confidence: schema.ConfDeclared,
			Evidence: []schema.Evidence{{
				Extractor: Name, Path: f.Path, Line: line,
				Rule:   "helm_chart_dependency",
				Detail: fmt.Sprintf("chart %q declares a dependency on subchart %q", chart.Name, dep.Name),
			}},
		})
		// The subchart's release name is how the parent's templates address
		// it, and is what connection strings in values.yaml point at.
		emit.Alias(resolve.Alias{
			Name: chart.Name + "-" + name, Namespace: chart.Name,
			DNS:        []string{chart.Name + "-" + name, name},
			TargetName: name,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: line,
				Rule:   "helm_subchart_release_name",
				Detail: fmt.Sprintf("subchart %q is conventionally released as %s-%s", dep.Name, chart.Name, name),
			},
		})
	}
	return nil
}

func (e *Extractor) extractValues(f *scan.File, emit scan.Emitter) error {
	var values map[string]any
	if err := yaml.Unmarshal(f.Content, &values); err != nil {
		// values.yaml is a common enough filename outside Helm that a parse
		// failure here says nothing about a chart being broken, so it is
		// deliberately not reported.
		return nil //nolint:nilerr // a non-chart values.yaml is not an error
	}
	if len(values) == 0 {
		return nil
	}
	// The filename alone is not evidence of a chart. Requiring a recognizably
	// Helm-shaped key avoids inventing a service from some unrelated
	// configuration file that happens to be called values.yaml.
	if !looksLikeChartValues(values) {
		return nil
	}

	chartName := path.Base(f.Dir)
	if chartName == "" || chartName == "." {
		chartName = "chart"
	}
	pos := yamlpos.New(f.Content)
	src := schema.Source{Extractor: Name, Path: f.Path, Line: pos.Line("image", "repository")}

	image := chartImage(values)
	kind, tech, _ := classify.ImageKind(image)
	id := schema.NewNodeID(kind, Name, chartName, chartName)

	attrs := schema.Attrs{"packaging": "helm"}
	if image != "" {
		attrs["image"] = image
	}
	if n, ok := intValue(values, "replicaCount"); ok {
		attrs["replicas"] = n
	}
	if port, ok := intValue(values, "service", "port"); ok {
		attrs["ports"] = []int{port}
	}

	emit.Node(schema.Node{
		ID: id, Kind: kind, Layer: schema.LayerContainer,
		Name: chartName, Namespace: chartName,
		Tech:  &schema.Tech{Runtime: "kubernetes", Framework: tech},
		Attrs: attrs, Sources: []schema.Source{src},
		// This node was inferred from a chart's default values, not read
		// from a rendered manifest. The real name comes from the release
		// name at install time, which is not in the repository.
		Confidence: schema.ConfWeak,
	})
	emit.Diag(schema.Diagnostic{
		Severity: schema.SeverityInfo,
		Code:     "helm_values_inferred",
		Path:     f.Path,
		Message: fmt.Sprintf(
			"component %q was inferred from chart default values rather than rendered templates; its name and settings may differ at install time",
			chartName),
	})

	// Connection strings and endpoints are among the most commonly
	// parameterized values in a chart, so walking the whole tree for them is
	// worth more than reading a fixed set of keys.
	for _, ref := range walkReferences(values, nil) {
		redacted, _ := resolve.RedactValue(ref.key, ref.value)
		emit.Hint(resolve.Hint{
			FromNode:      id,
			Kind:          ref.parsed.Kind,
			Raw:           redacted,
			Tokens:        ref.parsed.Tokens,
			Port:          ref.parsed.Port,
			Protocol:      ref.parsed.Protocol,
			SuggestedEdge: ref.parsed.Edge,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: pos.Line(strings.Split(ref.path, ".")...),
				Rule:   "helm_values_reference",
				Detail: fmt.Sprintf("values %s=%s", ref.path, redacted),
			},
		})
	}
	return nil
}

// looksLikeChartValues checks for keys that only a Helm chart's values file
// conventionally has.
func looksLikeChartValues(values map[string]any) bool {
	for _, key := range []string{"image", "replicaCount", "service", "ingress", "resources", "nameOverride", "fullnameOverride"} {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

func chartImage(values map[string]any) string {
	image, ok := values["image"].(map[string]any)
	if !ok {
		return ""
	}
	repo, _ := image["repository"].(string)
	if repo == "" {
		return ""
	}
	if tag := stringOf(image["tag"]); tag != "" {
		return repo + ":" + tag
	}
	return repo
}

type valueRef struct {
	path   string
	key    string
	value  string
	parsed resolve.Reference
}

// walkReferences descends the values tree looking for strings that name
// another component.
func walkReferences(node any, trail []string) []valueRef {
	// A chart's values can nest arbitrarily via subchart overrides, but not
	// deeply; the cap is a guard against a pathological file, not a real
	// structural limit.
	if len(trail) > 12 {
		return nil
	}

	var out []valueRef
	switch v := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, walkReferences(v[k], append(trail, k))...)
		}
	case []any:
		for i, item := range v {
			out = append(out, walkReferences(item, append(trail, strconv.Itoa(i)))...)
		}
	case string:
		if len(trail) == 0 {
			return nil
		}
		key := trail[len(trail)-1]
		if parsed, ok := resolve.ParseValue(key, v); ok {
			out = append(out, valueRef{
				path: strings.Join(trail, "."), key: key, value: v, parsed: parsed,
			})
		}
	}
	return out
}

func intValue(values map[string]any, keys ...string) (n int, ok bool) {
	cur := any(values)
	for i, key := range keys {
		m, isMap := cur.(map[string]any)
		if !isMap {
			return 0, false
		}
		next, present := m[key]
		if !present {
			return 0, false
		}
		if i == len(keys)-1 {
			switch typed := next.(type) {
			case int:
				return typed, true
			case float64:
				return int(typed), true
			case string:
				parsed, err := strconv.Atoi(typed)
				return parsed, err == nil
			default:
				return 0, false
			}
		}
		cur = next
	}
	return 0, false
}

func stringOf(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
