package resolve_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

// envHint is a reference carried by a file that is not a component, the way a
// .env file carries one.
func envHint(dir, raw string, tokens []string, edge schema.EdgeKind) resolve.Hint {
	return resolve.Hint{
		OwnerDir:      dir,
		Kind:          resolve.HintConnString,
		Raw:           raw,
		Tokens:        tokens,
		SuggestedEdge: edge,
		Source: schema.Evidence{
			Extractor: "dotenv", Path: dir + "/.env", Line: 1, Rule: "dotenv_reference",
		},
	}
}

func TestDirectoryHintBindsToTheServiceInThatDirectory(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "manifest", "typescript", "storefront", schema.Attrs{
		"directory": "services/web",
	})
	b.node(schema.KindDatastore, "compose", "default", "orders-db", nil)
	b.hint(envHint("services/web", "postgres://orders-db:5432/app",
		[]string{"orders-db"}, schema.EdgePersistsTo))

	r := b.run()
	if !r.hasEdge("storefront", "orders-db") {
		t.Fatalf("no edge storefront -> orders-db; found %v", r.edgeList())
	}
	if r.hasDiag("env_file_unowned") {
		t.Errorf("reported env_file_unowned for a directory that has an owner")
	}
}

// A manifest at the repository root records its directory as ".", which is
// what the extractor has to emit for a root .env file.
func TestDirectoryHintBindsAtRepositoryRoot(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "manifest", "typescript", "storefront", schema.Attrs{
		"directory": ".",
	})
	b.hint(envHint(".", "https://abcxyz.supabase.co",
		[]string{"abcxyz.supabase.co"}, schema.EdgeCalls))

	r := b.run()
	// A public hostname that matches nothing in the repository is a
	// third-party system, and gets synthesized.
	if !r.hasEdge("storefront", "abcxyz.supabase.co") {
		t.Fatalf("no edge to the external host; found %v", r.edgeList())
	}
}

// A .env file beside a Dockerfile belongs to the container Compose builds from
// that directory, which is a different attribute than a manifest's.
func TestDirectoryHintBindsThroughBuildContext(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "compose", "shop", "api", schema.Attrs{
		"buildContext": "./services/api",
	})
	b.node(schema.KindDatastore, "compose", "shop", "cache", nil)
	b.hint(envHint("services/api", "redis://cache:6379", []string{"cache"}, schema.EdgePersistsTo))

	r := b.run()
	if !r.hasEdge("api", "cache") {
		t.Fatalf("no edge api -> cache; found %v", r.edgeList())
	}
}

// Attaching a service's dependencies to the wrong service is worse than not
// attaching them, so an unowned directory is reported and drawn as nothing.
func TestUnownedDirectoryIsReportedNotGuessed(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "manifest", "typescript", "storefront", schema.Attrs{
		"directory": "services/web",
	})
	b.node(schema.KindDatastore, "compose", "default", "orders-db", nil)
	b.hint(envHint("infra", "postgres://orders-db:5432/app",
		[]string{"orders-db"}, schema.EdgePersistsTo))

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("edges = %v, want none", r.edgeList())
	}
	if !r.hasDiag("env_file_unowned") {
		t.Errorf("no env_file_unowned diagnostic; got %v", diagCodes(r))
	}
}

func TestAmbiguousOwnerIsReportedNotGuessed(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "manifest", "typescript", "web", schema.Attrs{"directory": "app"})
	b.node(schema.KindService, "manifest", "python", "worker", schema.Attrs{"directory": "app"})
	b.node(schema.KindDatastore, "compose", "default", "orders-db", nil)
	b.hint(envHint("app", "postgres://orders-db:5432/app",
		[]string{"orders-db"}, schema.EdgePersistsTo))

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("edges = %v, want none", r.edgeList())
	}
	if !r.hasDiag("env_file_ambiguous_owner") {
		t.Errorf("no env_file_ambiguous_owner diagnostic; got %v", diagCodes(r))
	}
}

// A datastore declared in a directory does not own the configuration of the
// service that happens to live beside it.
func TestDatastoreIsNotAnOwner(t *testing.T) {
	b := &builder{}
	b.node(schema.KindDatastore, "compose", "default", "db", schema.Attrs{"directory": "data"})
	b.hint(envHint("data", "redis://cache:6379", []string{"cache"}, schema.EdgePersistsTo))

	r := b.run()
	if len(r.Edges) != 0 {
		t.Errorf("edges = %v, want none", r.edgeList())
	}
	if !r.hasDiag("env_file_unowned") {
		t.Errorf("no env_file_unowned diagnostic; got %v", diagCodes(r))
	}
}

// Several variables from one unowned file are one problem, not one each.
func TestUnownedFileReportedOncePerFile(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "manifest", "go", "api", schema.Attrs{"directory": "api"})
	for _, token := range []string{"a", "b", "c"} {
		b.hint(envHint("nowhere", "redis://"+token+":6379", []string{token}, schema.EdgePersistsTo))
	}

	r := b.run()
	count := 0
	for _, d := range r.Diagnostics {
		if d.Code == "env_file_unowned" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("env_file_unowned diagnostics = %d, want 1", count)
	}
}

// The binding runs after code is merged into the container that runs it, so a
// hint from the codebase's directory has to land on the surviving node.
func TestDirectoryHintFollowsCodeMergedIntoDeployment(t *testing.T) {
	b := &builder{}
	b.node(schema.KindService, "compose", "shop", "api", schema.Attrs{
		"buildContext": "services/api",
		"image":        "acme/api:1",
	})
	b.node(schema.KindService, "manifest", "go", "acme/api", schema.Attrs{
		"directory": "services/api",
	})
	b.node(schema.KindDatastore, "compose", "shop", "cache", nil)
	b.hint(envHint("services/api", "redis://cache:6379", []string{"cache"}, schema.EdgePersistsTo))

	r := b.run()
	if !r.hasEdge("api", "cache") {
		t.Fatalf("no edge api -> cache; found %v", r.edgeList())
	}
	// The codebase was folded into the deployment, so it is not a box of its
	// own and cannot be the source of the edge.
	for _, n := range r.Nodes {
		if n.Name == "acme/api" {
			t.Errorf("merged-away code node %q survived", n.Name)
		}
	}
}

// A hint that names its own source node is untouched by any of this.
func TestHintWithExplicitSourceIsUnaffected(t *testing.T) {
	b := &builder{}
	from := b.node(schema.KindService, "k8s", "prod", "api", nil)
	b.node(schema.KindDatastore, "k8s", "prod", "cache", nil)
	b.hint(resolve.Hint{
		FromNode: from, Kind: resolve.HintConnString,
		Raw: "redis://cache:6379", Tokens: []string{"cache"},
		SuggestedEdge: schema.EdgePersistsTo,
	})

	r := b.run()
	if !r.hasEdge("api", "cache") {
		t.Fatalf("no edge api -> cache; found %v", r.edgeList())
	}
}

func diagCodes(r result) []string {
	out := make([]string, len(r.Diagnostics))
	for i, d := range r.Diagnostics {
		out[i] = d.Code
	}
	return out
}
