package resolve_test

import (
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// nodeAt is builder.node plus the file that declared it, which is what the
// project-tree check reads. The plain helper leaves Sources empty, and a node
// with no recorded source is deliberately exempt.
func (b *builder) nodeAt(kind schema.NodeKind, source, namespace, name, declaredAt string, attrs schema.Attrs) string {
	id := b.node(kind, source, namespace, name, attrs)
	last := &b.in.Nodes[len(b.in.Nodes)-1]
	last.Sources = []schema.Source{{Extractor: source, Path: declaredAt, Line: 1}}
	return id
}

func (r result) survives(id string) bool {
	for _, n := range r.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

// A directory called "worker" is not evidence. This is the case that shipped
// broken: a Compose service in one sample stack absorbed the package.json of
// an unrelated stack three directories away, and took its dependencies with
// it.
func TestBasenameJoinRefusedAcrossProjectTrees(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "javascript", "storefront-worker",
		"samples/react-supabase/worker/package.json",
		schema.Attrs{"directory": "samples/react-supabase/worker"})
	deployment := b.nodeAt(schema.KindService, "compose", "shopfront", "worker",
		"samples/compose-monolith/docker-compose.yml",
		schema.Attrs{"image": "worker:latest"})

	r := b.run()

	if !r.survives(code) {
		t.Error("a codebase in another project tree was merged away")
	}
	if !r.survives(deployment) {
		t.Fatal("the deployment disappeared")
	}
	if r.hasDiag("code_joined_to_deployment") {
		t.Error("a cross-project join was reported as made")
	}
	if !r.hasDiag("code_join_crosses_projects") {
		t.Errorf("the refusal was silent; got %v", diagCodes(r))
	}
}

// The layout the basename join exists for: a manifest repository that keeps
// deployment descriptions and source in sibling top-level directories. They
// diverge at the repository root, which is what one project looks like.
func TestBasenameJoinKeptWhenTreesDivergeAtTheRoot(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "checkoutservice",
		"src/checkoutservice/go.mod",
		schema.Attrs{"directory": "src/checkoutservice", "module": "acme/checkoutservice"})
	b.in.Nodes[len(b.in.Nodes)-1].Tech = &schema.Tech{Language: "go"}
	deployment := b.nodeAt(schema.KindService, "k8s", "prod", "checkoutservice",
		"deploy/prod/checkoutservice.yaml",
		schema.Attrs{"image": "checkoutservice:1.0"})

	r := b.run()

	if r.survives(code) {
		t.Error("the codebase was drawn as a second box; this is the join's whole purpose")
	}
	if !r.hasDiag("code_joined_to_deployment") {
		t.Errorf("an inferred join was not reported; got %v", diagCodes(r))
	}
	for _, n := range r.Nodes {
		if n.ID == deployment && (n.Tech == nil || n.Tech.Language != "go") {
			t.Errorf("the language the codebase knew was not carried over: %+v", n.Tech)
		}
	}
}

// A manifest at the repository root governs everything beneath it.
func TestBasenameJoinKeptWhenTheManifestIsAtTheRoot(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "api",
		"services/api/go.mod", schema.Attrs{"directory": "services/api"})
	b.nodeAt(schema.KindService, "k8s", "prod", "api",
		"k8s.yaml", schema.Attrs{"image": "api:1.0"})

	r := b.run()
	if r.survives(code) {
		t.Error("a root manifest did not claim code beneath it")
	}
}

// A deployment declared inside the directory it runs contains its code.
func TestBasenameJoinKeptWhenTheManifestSitsAboveTheCode(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "api",
		"services/api/go.mod", schema.Attrs{"directory": "services/api"})
	b.nodeAt(schema.KindService, "k8s", "prod", "api",
		"services/api/deploy.yaml", schema.Attrs{"image": "api:1.0"})

	r := b.run()
	if r.survives(code) {
		t.Error("a manifest beside the code it deploys did not claim it")
	}
}

// The known cost of the rule, pinned so that it is a decision rather than a
// surprise: a project nested in a directory that also holds its code diverges
// below the root, so the inference is refused and the component is drawn
// twice. A build context still joins it, and two boxes is the safe failure.
func TestNestedProjectLosesTheInferenceAndSaysSo(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "api",
		"packages/a/src/api/go.mod", schema.Attrs{"directory": "packages/a/src/api"})
	b.nodeAt(schema.KindService, "k8s", "prod", "api",
		"packages/a/deploy/api.yaml", schema.Attrs{"image": "api:1.0"})

	r := b.run()
	if !r.survives(code) {
		t.Error("the documented limitation changed; update the comment on sameProjectTree")
	}
	if !r.hasDiag("code_join_crosses_projects") {
		t.Errorf("the refusal was silent; got %v", diagCodes(r))
	}
}

// A build context names the directory outright, so it infers nothing and the
// project-tree rule must not touch it.
func TestBuildContextJoinIgnoresProjectTrees(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "checkout",
		"samples/other/checkout/go.mod", schema.Attrs{"directory": "samples/other/checkout"})
	b.nodeAt(schema.KindService, "compose", "shop", "checkout",
		"samples/shop/docker-compose.yml",
		schema.Attrs{"buildContext": "samples/other/checkout", "image": "shop-checkout"})

	r := b.run()
	if r.survives(code) {
		t.Error("a declared build context was refused by a rule meant for inference")
	}
	if r.hasDiag("code_join_crosses_projects") {
		t.Error("a declared join was reported as a refused inference")
	}
}

// A node with no recorded source cannot be placed in a tree. Refusing on that
// basis would disable the inference rather than narrow it.
func TestJoinAllowedWhenTheDeploymentHasNoRecordedSource(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "api",
		"src/api/go.mod", schema.Attrs{"directory": "src/api"})
	b.node(schema.KindService, "k8s", "prod", "api", schema.Attrs{"image": "api:1.0"})

	r := b.run()
	if r.survives(code) {
		t.Error("a join was refused on a node whose location is unknown")
	}
}

// When a deployment in the right project claims the codebase, the refusal
// elsewhere is not a gap and saying so would be noise.
func TestRefusalIsSilentWhenAnotherProjectClaimedTheCode(t *testing.T) {
	b := &builder{}
	code := b.nodeAt(schema.KindService, "manifest", "go", "api",
		"src/api/go.mod", schema.Attrs{"directory": "src/api"})
	b.nodeAt(schema.KindService, "k8s", "prod", "api",
		"deploy/api.yaml", schema.Attrs{"image": "api:1.0"})

	r := b.run()
	if r.survives(code) {
		t.Fatal("the legitimate join did not happen")
	}
	if r.hasDiag("code_join_crosses_projects") {
		t.Error("a refusal was reported although the codebase was claimed correctly")
	}
}

// A codebase excluded for being in another tree must not count towards the
// uniqueness check, or one unrelated stack would suppress a correct join.
func TestCrossProjectCandidateDoesNotSuppressACorrectJoin(t *testing.T) {
	b := &builder{}
	right := b.nodeAt(schema.KindService, "manifest", "javascript", "shopfront-worker",
		"samples/compose-monolith/worker/package.json",
		schema.Attrs{"directory": "samples/compose-monolith/worker"})
	wrong := b.nodeAt(schema.KindService, "manifest", "javascript", "storefront-worker",
		"samples/react-supabase/worker/package.json",
		schema.Attrs{"directory": "samples/react-supabase/worker"})
	b.nodeAt(schema.KindService, "compose", "shopfront", "worker",
		"samples/compose-monolith/docker-compose.yml",
		schema.Attrs{"image": "worker:latest"})

	r := b.run()
	if r.survives(right) {
		t.Error("the codebase in the same project was not joined")
	}
	if !r.survives(wrong) {
		t.Error("the codebase in another project was merged away")
	}
}

// What the rule does not do, stated so that nobody mistakes it for total.
//
// The check compares the deployment's declaring directory with the code. When
// that directory sits at the top of the repository, every codebase diverges
// from it at the root and none can be excluded on position alone. What
// catches the collision then is the older uniqueness guard: two codebases
// answer to the name, so neither is joined. The result is two boxes rather
// than a wrong one, which is the direction this is allowed to fail in.
func TestRootDeclaredDeploymentFallsBackToTheUniquenessGuard(t *testing.T) {
	b := &builder{}
	first := b.nodeAt(schema.KindService, "manifest", "go", "worker",
		"src/worker/go.mod", schema.Attrs{"directory": "src/worker"})
	second := b.nodeAt(schema.KindService, "manifest", "javascript", "other-worker",
		"samples/elsewhere/worker/package.json",
		schema.Attrs{"directory": "samples/elsewhere/worker"})
	b.nodeAt(schema.KindService, "k8s", "prod", "worker",
		"deploy/worker.yaml", schema.Attrs{"image": "worker:1.0"})

	r := b.run()
	if !r.survives(first) || !r.survives(second) {
		t.Error("an ambiguous basename match was joined to one of two codebases")
	}
	if r.hasDiag("code_joined_to_deployment") {
		t.Error("an ambiguous join was reported as made")
	}
}
