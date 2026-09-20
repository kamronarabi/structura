package k8s

import (
	"fmt"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Phase 1 does not render Helm charts.
//
// Rendering properly means executing Helm's template engine over
// user-controlled templates, which is a large dependency and a meaningful
// amount of trust to extend to a file in a repository being scanned. The
// alternative — shelling out to a helm binary — breaks the zero-dependency
// promise the distribution rests on.
//
// What matters is that the resulting gap is stated precisely. A user whose
// chart was skipped needs to know their node set is incomplete and why;
// silently producing a sparse graph would leave them believing Structura had
// read their deployment and found three services.

func isHelmTemplate(f *scan.File) bool {
	return classify.IsHelmTemplate(f.Path, f.Content)
}

// reportHelmTemplate records one unrendered template. The pipeline collapses
// these per chart, so a chart with eighteen templates produces one
// diagnostic naming the directory and the count rather than eighteen lines
// the user has to read past.
func reportHelmTemplate(f *scan.File, emit scan.Emitter) {
	dir := f.Dir
	if chartDir, ok := classify.HelmChartDir(f.Path); ok {
		dir = chartDir + "/templates"
	}
	emit.Diag(schema.Diagnostic{
		Severity: schema.SeverityWarn,
		Code:     DiagHelmUnrendered,
		Path:     dir,
		Message:  "Helm template not rendered; the node set from this chart may be incomplete",
	})
}

// DiagHelmUnrendered marks a Helm template that was detected but not
// rendered. The pipeline aggregates diagnostics carrying this code.
const DiagHelmUnrendered = "helm_unrendered"

// reportKustomization notes what a Kustomization declares that this scan did
// not apply.
//
// The resources a kustomization lists are ordinary manifests elsewhere in the
// repository, and the walker reads them directly, so a plain base costs
// nothing. Overlays are different: patches and name prefixes change what
// actually gets deployed, and reporting the difference is more useful than
// pretending the base is the deployed truth.
func reportKustomization(f *scan.File, emit scan.Emitter, _ *manifest) {
	emit.Diag(schema.Diagnostic{
		Severity: schema.SeverityInfo,
		Code:     "kustomize_unrendered",
		Path:     f.Path,
		Message: fmt.Sprintf(
			"Kustomize overlay not applied; the manifests it lists were read directly, so patches, name prefixes, and replacements in %s are not reflected",
			f.Name),
	})
}
