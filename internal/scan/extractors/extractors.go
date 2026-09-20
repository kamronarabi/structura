// Package extractors is the single place new formats are registered.
//
// Adding support for a format — or, from Phase 2, a language — should be one
// import and one line here. If it ever requires touching the pipeline, the
// walker, or the schema, the Extractor interface has failed at its job.
package extractors

import (
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/compose"
	"github.com/kamronarabi/structura/internal/scan/extractors/helm"
	"github.com/kamronarabi/structura/internal/scan/extractors/k8s"
	"github.com/kamronarabi/structura/internal/scan/extractors/manifest"
	"github.com/kamronarabi/structura/internal/scan/extractors/terraform"
)

// Default returns the extractors a scan runs with.
func Default() *scan.Registry {
	return scan.NewRegistry(
		compose.New(),
		helm.New(),
		k8s.New(),
		manifest.New(),
		terraform.New(),
	)
}
