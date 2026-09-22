package project_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/project"
	"github.com/kamronarabi/structura/pkg/schema"
)

func node(id string, kind schema.NodeKind, name string, paths ...string) schema.Node {
	n := schema.Node{ID: id, Kind: kind, Name: name}
	for _, p := range paths {
		n.Sources = append(n.Sources, schema.Source{Extractor: "test", Path: p, Line: 1})
	}
	return n
}

func trees(s project.Suspect) string { return strings.Join(s.Trees, " + ") }

// The collision this exists to surface: two sample stacks under one directory,
// each declaring a "prod" namespace, merged into one boundary containing
// members from both.
func TestTwoStacksSharingOnlyANamespaceAreReported(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/shop/deploy/api.yaml", "samples/admin/deploy/api.yaml"),
		node("container:@prod/checkout", schema.KindService, "checkout", "samples/shop/deploy/api.yaml"),
		node("container:@prod/orders-db", schema.KindDatastore, "orders-db", "samples/shop/deploy/api.yaml"),
		node("container:@prod/reports", schema.KindService, "reports", "samples/admin/deploy/api.yaml"),
	}

	got := project.Suspects(nodes, project.Discover(nil))
	if len(got) != 1 {
		t.Fatalf("suspects = %d, want 1: %+v", len(got), got)
	}
	if trees(got[0]) != "samples/admin + samples/shop" {
		t.Errorf("Trees = %v, want the two sample directories", got[0].Trees)
	}
	if len(got[0].Shared) != 1 || got[0].Shared[0] != "prod" {
		t.Errorf("Shared = %v, want [prod]", got[0].Shared)
	}
}

// A single project routinely declares one component from several top-level
// directories. That is the layout the graph is built for.
func TestDivergenceAtTheRepositoryRootIsNotSuspicious(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"base/api.yaml", "overlays/prod/api.yaml"),
		node("container:@prod/api", schema.KindService, "api",
			"base/api.yaml", "overlays/prod/api.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: base/ beside overlays/ is one project", got)
	}
}

// An overlay nested below the root re-declares the base's actual Deployment.
// Sharing a real component is what says two trees are one system, and it has
// to count even though that component is one of the ones that collided.
func TestOverlaysSharingARealComponentAreNotSuspicious(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"apps/shop/base/api.yaml", "apps/shop/overlays/prod/api.yaml"),
		node("container:@prod/api", schema.KindService, "api",
			"apps/shop/base/api.yaml", "apps/shop/overlays/prod/api.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: the trees share a Deployment", got)
	}
}

// Two projects both calling Stripe have said nothing about being one project.
func TestASharedExternalIsNotEvidenceOfOneProject(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/a/deploy/app.yaml", "samples/b/deploy/app.yaml"),
		node("context:api.stripe.com", schema.KindExternal, "api.stripe.com",
			"samples/a/deploy/app.yaml", "samples/b/deploy/app.yaml"),
		node("container:@prod/one", schema.KindService, "one", "samples/a/deploy/app.yaml"),
		node("container:@prod/two", schema.KindService, "two", "samples/b/deploy/app.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 1 {
		t.Errorf("suspects = %+v, want one: a shared external is not a shared component", got)
	}
}

// Once the answer is written down, the question is not asked again.
func TestDeclaredProjectsAreNotReported(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/shop/deploy/api.yaml", "samples/admin/deploy/api.yaml"),
	}

	// Declared as one project: the person has said these belong together.
	if got := project.Suspects(nodes, project.Discover([]string{"samples"})); len(got) != 0 {
		t.Errorf("suspects = %+v, want none when both trees are in one declared project", got)
	}
}

// A component declared once, or declared twice in the same directory, is not
// a collision at all.
func TestSingleDeclarationIsNotSuspicious(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/shop/deploy/a.yaml", "samples/shop/deploy/b.yaml"),
		node("container:@prod/api", schema.KindService, "api", "samples/shop/deploy/a.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: one directory declared it twice", got)
	}
}

func TestNoSourcesIsNotSuspicious(t *testing.T) {
	nodes := []schema.Node{
		node("container:@prod/api", schema.KindService, "api"),
		node("container:@prod/web", schema.KindService, "web"),
	}
	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none", got)
	}
}

// Several collisions between one pair of trees are one question, not several.
func TestCollisionsBetweenOnePairAreGroupedTogether(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/a/deploy/x.yaml", "samples/b/deploy/x.yaml"),
		node("context:@staging/staging", schema.KindBoundary, "staging",
			"samples/a/deploy/y.yaml", "samples/b/deploy/y.yaml"),
		node("container:@prod/one", schema.KindService, "one", "samples/a/deploy/x.yaml"),
		node("container:@prod/two", schema.KindService, "two", "samples/b/deploy/x.yaml"),
	}

	got := project.Suspects(nodes, project.Discover(nil))
	if len(got) != 1 {
		t.Fatalf("suspects = %d, want 1 covering both collisions: %+v", len(got), got)
	}
	if len(got[0].Shared) != 2 {
		t.Errorf("Shared = %v, want both collided components named", got[0].Shared)
	}
}

// Two scans of one tree must agree, so the report cannot depend on map order.
func TestSuspectsAreDeterministic(t *testing.T) {
	nodes := []schema.Node{
		node("context:@prod/prod", schema.KindBoundary, "prod",
			"samples/c/x.yaml", "samples/a/x.yaml", "samples/b/x.yaml"),
		node("container:@prod/one", schema.KindService, "one", "samples/a/x.yaml"),
		node("container:@prod/two", schema.KindService, "two", "samples/b/x.yaml"),
		node("container:@prod/three", schema.KindService, "three", "samples/c/x.yaml"),
	}

	var first string
	for i := 0; i < 8; i++ {
		var b strings.Builder
		for _, s := range project.Suspects(nodes, project.Discover(nil)) {
			b.WriteString(trees(s) + "|" + strings.Join(s.Shared, ",") + "\n")
		}
		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatalf("Suspects() is not deterministic:\n%s\n---\n%s", first, b.String())
		}
	}
}

func described(id string, kind schema.NodeKind, name, path string, attrs schema.Attrs) schema.Node {
	n := node(id, kind, name, path)
	n.Attrs = attrs
	return n
}

// The shape a repository of sample stacks takes: every stack sits at the
// repository root, so nothing about the layout separates them, and the only
// thing that does is that they build "web" from different directories.
func TestTreesThatBuildOneNameFromDifferentDirectoriesAreReported(t *testing.T) {
	nodes := []schema.Node{
		described("container:web", schema.KindService, "web", "angular/compose.yaml",
			schema.Attrs{"buildContext": "angular/angular"}),
		described("container:web", schema.KindService, "web", "apache-php/compose.yaml",
			schema.Attrs{"buildContext": "apache-php/app"}),
	}

	got := project.Suspects(nodes, project.Discover(nil))
	if len(got) != 1 {
		t.Fatalf("suspects = %d, want 1: %+v", len(got), got)
	}
	if trees(got[0]) != "angular + apache-php" {
		t.Errorf("Trees = %v, want the two stack directories", got[0].Trees)
	}
	if got[0].Ground != project.GroundDescribedDifferently {
		t.Errorf("Ground = %q, want %q", got[0].Ground, project.GroundDescribedDifferently)
	}
}

// The false positive this has to avoid, and the reason the image is compared
// by name alone: a Helm chart writes a bare repository into values.yaml and
// the manifest beside it writes a registry path and a tag, for one build.
// Anything finer reports every chart-beside-manifests repository as two
// projects, which is most of them.
func TestTwoSpellingsOfOneImageAreNotADisagreement(t *testing.T) {
	nodes := []schema.Node{
		described("container:adservice", schema.KindService, "adservice", "helm-chart/values.yaml",
			schema.Attrs{"image": "adservice"}),
		described("container:adservice", schema.KindService, "adservice", "kustomize/base/adservice.yaml",
			schema.Attrs{"image": "us-central1-docker.pkg.dev/ci/demo/adservice:v0.10.7"}),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: those are one image written two ways", got)
	}
}

// A file that named no image has not contradicted one that did. An overlay
// patching a replica count says nothing about what the container runs, and
// treating silence as disagreement would report every overlay.
func TestSilenceIsNotDisagreement(t *testing.T) {
	nodes := []schema.Node{
		described("container:api", schema.KindService, "api", "base/api.yaml",
			schema.Attrs{"image": "acme/api:1.2"}),
		described("container:api", schema.KindService, "api", "overlays/prod/api.yaml", nil),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: the overlay named no image", got)
	}
}

// One tree's own account of a component is assembled from several files --
// the Compose file names an image, the Dockerfile beside it names a build
// directory -- and comparing those to each other would report a repository
// against itself.
func TestOneTreeDescribingItselfInSeveralFilesIsNotADisagreement(t *testing.T) {
	nodes := []schema.Node{
		described("container:web", schema.KindService, "web", "stack/compose.yaml",
			schema.Attrs{"image": "nginx"}),
		described("container:web", schema.KindService, "web", "stack/app/Dockerfile",
			schema.Attrs{"directory": "stack/app"}),
		// A second tree, so there is a collision to judge at all.
		described("container:web", schema.KindService, "web", "other/compose.yaml",
			schema.Attrs{"image": "nginx"}),
		node("container:only-here", schema.KindService, "only-here", "other/compose.yaml"),
		node("container:stack-only", schema.KindService, "stack-only", "stack/compose.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: both trees say nginx", got)
	}
}

// Fifty sample stacks that all declare "db" are one question about fifty
// directories, not one question for each of the twelve hundred pairs.
func TestOneQuestionPerNameNotPerPairOfDirectories(t *testing.T) {
	var nodes []schema.Node
	for _, stack := range []string{"a", "b", "c", "d", "e"} {
		nodes = append(nodes, described("container:db", schema.KindDatastore, "db",
			stack+"/compose.yaml", schema.Attrs{"image": stack + "-database"}))
	}

	got := project.Suspects(nodes, project.Discover(nil))
	if len(got) != 1 {
		t.Fatalf("suspects = %d, want 1 naming all five directories: %+v", len(got), got)
	}
	if len(got[0].Trees) != 5 {
		t.Errorf("Trees = %v, want all five", got[0].Trees)
	}
}

// Two files in one directory disagreeing is a different problem with a
// different remedy. Declaring projects cannot fix it, so this must not
// suggest it.
func TestADisagreementInsideOneTreeIsNotAProjectQuestion(t *testing.T) {
	nodes := []schema.Node{
		described("container:api", schema.KindService, "api", "deploy/api.yaml",
			schema.Attrs{"image": "acme/api"}),
		described("container:api", schema.KindService, "api", "deploy/api-canary.yaml",
			schema.Attrs{"image": "acme/api-next"}),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: one directory declared both", got)
	}
}

// Evidence about the component stands on its own, so unlike the weaker test
// it needs no restriction on where the trees sit -- which is the whole point,
// because sample stacks sit at the repository root.
func TestDisagreementAtTheRepositoryRootIsStillReported(t *testing.T) {
	nodes := []schema.Node{
		described("container:db", schema.KindDatastore, "db", "shop/compose.yaml",
			schema.Attrs{"image": "postgres:16"}),
		described("container:db", schema.KindDatastore, "db", "blog/compose.yaml",
			schema.Attrs{"image": "mysql:8"}),
		// Both trees also declare a service called "backend", so the weaker
		// test could not fire here even without the root restriction.
		node("container:backend", schema.KindService, "backend", "shop/compose.yaml", "blog/compose.yaml"),
	}

	got := project.Suspects(nodes, project.Discover(nil))
	if len(got) != 1 {
		t.Fatalf("suspects = %d, want 1: %+v", len(got), got)
	}
	if len(got[0].Shared) != 1 || got[0].Shared[0] != "db" {
		t.Errorf("Shared = %v, want [db]: backend is not what they disagree about", got[0].Shared)
	}
}

// Once the answer is written down, the question is not asked again -- for
// this ground as much as for the other one.
func TestDeclaredProjectsAreNotReportedForDisagreements(t *testing.T) {
	nodes := []schema.Node{
		described("container:web", schema.KindService, "web", "samples/a/compose.yaml",
			schema.Attrs{"buildContext": "samples/a/app"}),
		described("container:web", schema.KindService, "web", "samples/b/compose.yaml",
			schema.Attrs{"buildContext": "samples/b/app"}),
	}

	if got := project.Suspects(nodes, project.Discover([]string{"samples"})); len(got) != 0 {
		t.Errorf("suspects = %+v, want none when both trees are in one declared project", got)
	}
}
