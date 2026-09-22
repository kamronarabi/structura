package resolve_test

import (
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// codebase adds a component that a file in some directory describes. derived
// marks a name the file did not declare, the way a Dockerfile's does. The
// namespace is the language, which is what both extractors put there.
func (b *builder) codebase(source, namespace, name, dir string, derived bool, attrs schema.Attrs) string {
	if attrs == nil {
		attrs = schema.Attrs{}
	}
	attrs["directory"] = dir
	if derived {
		attrs["nameFrom"] = "directory"
	}
	id := b.node(schema.KindService, source, namespace, name, attrs)
	n := &b.in.Nodes[len(b.in.Nodes)-1]
	n.Tech = &schema.Tech{Language: namespace}
	n.Sources = []schema.Source{{Extractor: source, Path: dir + "/" + source, Line: 1}}
	return id
}

func (r result) hasNode(name string) bool {
	for _, n := range r.Nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// A go.mod and a Dockerfile in one directory are the module and the image
// built from it, which is one component.
func TestManifestAndDockerfileInOneDirectoryAreOneComponent(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/checkout", "services/checkout", false, nil)
	b.codebase("dockerfile", "go", "checkout", "services/checkout", true,
		schema.Attrs{"baseImage": "golang:1.22", "ports": []int{8080}})

	r := b.run()
	if len(r.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(r.Nodes), r.Nodes)
	}
	n := r.Nodes[0]
	// The declared name wins: a Dockerfile can only be named after its
	// directory, so its name is a placeholder.
	if n.Name != "acme/checkout" {
		t.Errorf("Name = %q, want the declared module name", n.Name)
	}
	if n.Attrs["baseImage"] != "golang:1.22" {
		t.Errorf("the Dockerfile's base image was lost: %+v", n.Attrs)
	}
	if ports, _ := n.Attrs["ports"].([]int); len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("the exposed port was lost: %+v", n.Attrs)
	}
	// The name the survivor declared must not be overwritten by the note
	// that the other node's was derived.
	if _, ok := n.Attrs["nameFrom"]; ok {
		t.Errorf("a declared name was marked as derived: %+v", n.Attrs)
	}
	if len(n.Sources) != 2 {
		t.Errorf("Sources = %+v, want both files recorded", n.Sources)
	}
}

// The regression this fold exists to prevent. A build context requires exactly
// one component in the directory it points at, so a repository that gained a
// Dockerfile would have lost the link between its container and its code.
func TestBuildContextStillJoinsWhenADockerfileIsPresent(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/checkout", "services/checkout", false, nil)
	b.codebase("dockerfile", "go", "checkout", "services/checkout", true, nil)
	b.node(schema.KindService, "compose", "shop", "checkout", schema.Attrs{
		"image":        "checkout:1",
		"buildContext": "services/checkout",
	})

	r := b.run()
	if len(r.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(r.Nodes), r.Nodes)
	}
	if r.Nodes[0].Tech == nil || r.Nodes[0].Tech.Language != "go" {
		t.Errorf("the container did not learn what it runs: %+v", r.Nodes[0].Tech)
	}
}

// Only the directory decides. Dockerfile beside Dockerfile.debug is a variant
// of one service, and when the two disagree about the runtime they do not even
// share an identifier -- but they still share a directory, which is what says
// they are one thing.
func TestTwoDerivedNamesInOneDirectoryCollapse(t *testing.T) {
	b := &builder{}
	b.codebase("dockerfile", "csharp", "cartservice", "src/cartservice/src", true,
		schema.Attrs{"dockerfile": "Dockerfile"})
	b.codebase("dockerfile", "container", "cartservice", "src/cartservice/src", true,
		schema.Attrs{"baseImage": "alpine:3.20"})

	r := b.run()
	if len(r.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: %+v", len(r.Nodes), r.Nodes)
	}
}

// A directory in a different tree is a different directory, whatever it is
// called. This fold never reaches across one.
func TestSameNameInDifferentDirectoriesIsNotFolded(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "worker", "a/worker", false, nil)
	b.codebase("dockerfile", "go", "worker", "b/worker", true, nil)

	r := b.run()
	if len(r.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2: two directories are two components", len(r.Nodes))
	}
}

// A directory declaring two modules does not say which of them the image is
// built from, and moving one codebase's dependencies onto the other's box is
// worse than drawing two.
func TestAmbiguousDirectoryIsReportedRatherThanGuessed(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/api", "app", false, nil)
	b.codebase("manifest", "javascript", "acme-web", "app", false, nil)
	b.codebase("dockerfile", "go", "app", "app", true, nil)

	r := b.run()
	if len(r.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3: nothing should have been guessed", len(r.Nodes))
	}
	if !r.hasDiag("directory_declares_several_components") {
		t.Errorf("the ambiguity was resolved silently: %+v", r.Diagnostics)
	}
}

// Two manifests declaring two modules in one directory may well be two things,
// and were drawn as two before this fold existed.
func TestTwoDeclaredNamesAreLeftAlone(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/api", "app", false, nil)
	b.codebase("manifest", "javascript", "acme-web", "app", false, nil)

	r := b.run()
	if len(r.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2", len(r.Nodes))
	}
	if r.hasDiag("directory_declares_several_components") {
		t.Error("a directory with no derived name was reported as ambiguous")
	}
}

// A deployment has an image and is the thing being merged into, never the
// thing merged away -- even when it names the directory it was built from.
func TestADeploymentIsNotFoldedAwayByItsOwnBuildDirectory(t *testing.T) {
	b := &builder{}
	b.codebase("dockerfile", "go", "api", "api", true, nil)
	b.node(schema.KindService, "compose", "shop", "api", schema.Attrs{
		"image": "api:1", "directory": "api",
	})

	r := b.run()
	if !r.hasNode("api") {
		t.Fatalf("the component disappeared: %+v", r.Nodes)
	}
	for _, n := range r.Nodes {
		if n.Attrs["image"] == nil && len(r.Nodes) == 1 {
			t.Error("the deployment was folded into the codebase rather than the other way round")
		}
	}
}

// Hints carried by a file that names no component bind to whatever survived
// the fold, not to the node that was absorbed.
func TestDirectoryHintsBindToTheSurvivor(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "javascript", "storefront", ".", false, nil)
	b.codebase("dockerfile", "javascript", "app", ".", true, nil)
	b.node(schema.KindDatastore, "compose", "shop", "orders-db", nil)
	b.hint(envHint(".", "postgres://orders-db:5432/app", []string{"orders-db"}, schema.EdgePersistsTo))

	r := b.run()
	if !r.hasEdge("storefront", "orders-db") {
		t.Errorf("the .env reference did not reach the surviving component: %v", r.edgeList())
	}
}

// The surviving node was chosen because it is the better description, so a
// conflict is resolved in its favour rather than by whichever was indexed
// last.
func TestTheSurvivorsOwnFactsAreNotOverwritten(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/api", "api", false,
		schema.Attrs{"baseImage": "golang:1.22", "ports": []int{8080}})
	b.codebase("dockerfile", "go", "api", "api", true,
		schema.Attrs{"baseImage": "alpine:3.20", "ports": []int{9999}})

	n := b.run().node(t, "acme/api")
	if n.Attrs["baseImage"] != "golang:1.22" {
		t.Errorf("baseImage = %v, want the survivor's own", n.Attrs["baseImage"])
	}
	if ports, _ := n.Attrs["ports"].([]int); len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("ports = %v, want the survivor's own", n.Attrs["ports"])
	}
}

// A relationship that landed on the node that was folded away has to move to
// the one that survived. Dropping it would make the fold cost an edge.
func TestEdgesIntoTheAbsorbedComponentAreRewritten(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/api", "api", false, nil)
	absorbed := b.codebase("dockerfile", "go", "api", "api", true, nil)
	caller := b.node(schema.KindService, "compose", "shop", "gateway", nil)

	b.in.Edges = append(b.in.Edges, schema.Edge{
		From: caller, To: absorbed, Kind: schema.EdgeCalls, Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{Extractor: "compose", Rule: "compose_depends_on"}},
	})

	r := b.run()
	if !r.hasEdge("gateway", "acme/api") {
		t.Errorf("the relationship did not follow the fold; found %v", r.edgeList())
	}
}

// Three files, one component: a Dockerfile and a manifest fold into each
// other, and the result folds into the container that runs it. The merge
// chain has to resolve all the way through or a node survives with no edges.
func TestDockerfileManifestAndDeploymentCollapseToOne(t *testing.T) {
	b := &builder{}
	b.codebase("manifest", "go", "acme/checkout", "services/checkout", false, nil)
	b.codebase("dockerfile", "go", "checkout", "services/checkout", true,
		schema.Attrs{"baseImage": "gcr.io/distroless/static", "ports": []int{8080}})
	deployment := b.node(schema.KindService, "compose", "shop", "checkout", schema.Attrs{
		"image": "checkout:1", "buildContext": "services/checkout",
	})
	b.in.Nodes[len(b.in.Nodes)-1].Sources = []schema.Source{
		{Extractor: "compose", Path: "docker-compose.yml", Line: 2},
	}
	db := b.node(schema.KindDatastore, "compose", "shop", "orders-db", nil)
	b.in.Edges = append(b.in.Edges, schema.Edge{
		From: deployment, To: db, Kind: schema.EdgePersistsTo, Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{Extractor: "compose", Rule: "compose_depends_on"}},
	})

	r := b.run()
	if len(r.Nodes) != 2 {
		t.Fatalf("nodes = %d, want the service and its database: %+v", len(r.Nodes), r.Nodes)
	}
	n := r.node(t, "checkout")
	if n.Tech == nil || n.Tech.Language != "go" {
		t.Errorf("Tech = %+v, want language go", n.Tech)
	}
	if n.Attrs["baseImage"] != "gcr.io/distroless/static" {
		t.Errorf("the base image did not survive two merges: %+v", n.Attrs)
	}
	if n.Attrs["module"] != nil && n.Attrs["directory"] != "services/checkout" {
		t.Errorf("the codebase's directory did not survive: %+v", n.Attrs)
	}
	if !r.hasEdge("checkout", "orders-db") {
		t.Errorf("the relationship was lost; found %v", r.edgeList())
	}
	if len(n.Sources) != 3 {
		t.Errorf("Sources = %d, want all three files recorded", len(n.Sources))
	}
}

// A codebase with no directory is not a candidate for a fold keyed on one.
func TestNodesWithoutADirectoryAreNotFolded(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "k8s", "prod", "api", schema.Attrs{"nameFrom": "directory"})
	b.node(schema.KindService, "k8s", "prod", "other", nil)

	if got := len(b.run().Nodes); got != 2 {
		t.Errorf("nodes = %d, want 2", got)
	}
}
