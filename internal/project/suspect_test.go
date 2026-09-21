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
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"samples/shop/deploy/api.yaml", "samples/admin/deploy/api.yaml"),
		node("service:k8s/prod/checkout", schema.KindService, "checkout", "samples/shop/deploy/api.yaml"),
		node("datastore:k8s/prod/orders-db", schema.KindDatastore, "orders-db", "samples/shop/deploy/api.yaml"),
		node("service:k8s/prod/reports", schema.KindService, "reports", "samples/admin/deploy/api.yaml"),
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
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"base/api.yaml", "overlays/prod/api.yaml"),
		node("service:k8s/prod/api", schema.KindService, "api",
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
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"apps/shop/base/api.yaml", "apps/shop/overlays/prod/api.yaml"),
		node("service:k8s/prod/api", schema.KindService, "api",
			"apps/shop/base/api.yaml", "apps/shop/overlays/prod/api.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: the trees share a Deployment", got)
	}
}

// Two projects both calling Stripe have said nothing about being one project.
func TestASharedExternalIsNotEvidenceOfOneProject(t *testing.T) {
	nodes := []schema.Node{
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"samples/a/deploy/app.yaml", "samples/b/deploy/app.yaml"),
		node("external:resolver/net/api.stripe.com", schema.KindExternal, "api.stripe.com",
			"samples/a/deploy/app.yaml", "samples/b/deploy/app.yaml"),
		node("service:k8s/prod/one", schema.KindService, "one", "samples/a/deploy/app.yaml"),
		node("service:k8s/prod/two", schema.KindService, "two", "samples/b/deploy/app.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 1 {
		t.Errorf("suspects = %+v, want one: a shared external is not a shared component", got)
	}
}

// Once the answer is written down, the question is not asked again.
func TestDeclaredProjectsAreNotReported(t *testing.T) {
	nodes := []schema.Node{
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
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
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"samples/shop/deploy/a.yaml", "samples/shop/deploy/b.yaml"),
		node("service:k8s/prod/api", schema.KindService, "api", "samples/shop/deploy/a.yaml"),
	}

	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none: one directory declared it twice", got)
	}
}

func TestNoSourcesIsNotSuspicious(t *testing.T) {
	nodes := []schema.Node{
		node("service:k8s/prod/api", schema.KindService, "api"),
		node("service:k8s/prod/web", schema.KindService, "web"),
	}
	if got := project.Suspects(nodes, project.Discover(nil)); len(got) != 0 {
		t.Errorf("suspects = %+v, want none", got)
	}
}

// Several collisions between one pair of trees are one question, not several.
func TestCollisionsBetweenOnePairAreGroupedTogether(t *testing.T) {
	nodes := []schema.Node{
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"samples/a/deploy/x.yaml", "samples/b/deploy/x.yaml"),
		node("boundary:k8s/staging/staging", schema.KindBoundary, "staging",
			"samples/a/deploy/y.yaml", "samples/b/deploy/y.yaml"),
		node("service:k8s/prod/one", schema.KindService, "one", "samples/a/deploy/x.yaml"),
		node("service:k8s/prod/two", schema.KindService, "two", "samples/b/deploy/x.yaml"),
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
		node("boundary:k8s/prod/prod", schema.KindBoundary, "prod",
			"samples/c/x.yaml", "samples/a/x.yaml", "samples/b/x.yaml"),
		node("service:k8s/prod/one", schema.KindService, "one", "samples/a/x.yaml"),
		node("service:k8s/prod/two", schema.KindService, "two", "samples/b/x.yaml"),
		node("service:k8s/prod/three", schema.KindService, "three", "samples/c/x.yaml"),
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
