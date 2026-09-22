package schema_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Node IDs are built from names that come out of user-controlled YAML, HCL,
// and JSON: container names, Terraform resource labels, npm package names.
// Two properties have to hold for every one of them, because merge compares
// IDs as strings:
//
//   - the constructor's output always validates, and
//   - feeding an ID's parts back through the constructor is a fixed point.
//
// A name that normalizes differently the second time would silently split one
// component into two nodes, which is exactly the kind of bug that shows up as
// a confusing diagram rather than as a crash.
func FuzzNodeID(f *testing.F) {
	seeds := []string{
		"api-gateway", "API Gateway", "user_service", "café-service",
		"github.com/spf13/cobra", "@scope/pkg", "///", "!!!", "",
		"a--b", "-leading", "trailing-", "a/b//c/", strings.Repeat("x", 300),
		"日本語", "a\tb", "a\nb", "aws_lambda_function.api", "ns:weird",
	}
	for _, s := range seeds {
		f.Add(s, s, s)
	}

	kinds := []schema.NodeKind{
		schema.KindService, schema.KindDatastore, schema.KindQueue,
		schema.KindExternal, schema.KindCloudResource, schema.KindPackage,
		schema.KindBoundary,
	}

	f.Fuzz(func(t *testing.T, project, scope, name string) {
		for _, kind := range kinds {
			id := schema.NewNodeID(kind, scope, name)

			if err := schema.ValidateNodeID(id); err != nil {
				t.Fatalf("NewNodeID(%q, %q, %q) = %q, which does not validate: %v",
					kind, scope, name, id, err)
			}
			parsed, err := schema.ParseNodeID(id)
			if err != nil {
				t.Fatalf("ParseNodeID(%q) = %v", id, err)
			}
			// The layer is what the identifier carries, and every kind that
			// maps to one layer has to produce one identifier -- that is what
			// lets two readings of a component merge.
			if parsed.Layer != schema.LayerOf(kind) {
				t.Fatalf("layer round-tripped as %q, want %q", parsed.Layer, schema.LayerOf(kind))
			}
			again := schema.NewNodeID(kind, parsed.Scope, parsed.Name)
			if again != id {
				t.Fatalf("not a fixed point: %q -> %q", id, again)
			}

			// A scope, however mangled, must stay distinguishable from a name
			// that merely contains slashes.
			if strings.TrimSpace(scope) != "" && !strings.Contains(id, ":@") {
				t.Fatalf("NewNodeID(%q, %q, %q) = %q dropped the scope marker", kind, scope, name, id)
			}

			// Qualifying by project must survive validation, since every
			// multi-project repository's identifiers go through it.
			qualified, err := schema.QualifyProject(id, project)
			if err != nil {
				if strings.TrimSpace(project) == "" {
					t.Fatalf("QualifyProject(%q, %q) = %v", id, project, err)
				}
				return
			}
			if err := schema.ValidateNodeID(qualified); err != nil {
				t.Fatalf("QualifyProject(%q, %q) = %q, which does not validate: %v",
					id, project, qualified, err)
			}
		}
	})
}

// Marshal runs on graphs assembled from untrusted input and must never panic
// or lose determinism, whatever ends up in an attribute value.
func FuzzGraphRoundTrip(f *testing.F) {
	f.Add("api", "postgres", "deploy/api.yaml", 0.8)
	f.Add("", "", "", 1.0)
	f.Add("a", "b", "c", -5.0)
	f.Add("x", "y", "z", 1e308)

	f.Fuzz(func(t *testing.T, fromName, toName, path string, confidence float64) {
		from := schema.NewNodeID(schema.KindService, "ns", fromName)
		to := schema.NewNodeID(schema.KindDatastore, "ns", toName)

		b := schema.NewBuilder()
		b.AddNode(schema.Node{
			ID: from, Kind: schema.KindService, Layer: schema.LayerContainer,
			Name: "from", Confidence: confidence,
		})
		b.AddNode(schema.Node{
			ID: to, Kind: schema.KindDatastore, Layer: schema.LayerContainer,
			Name: "to", Confidence: confidence,
		})
		if from != to {
			b.AddEdge(schema.Edge{
				From: from, To: to, Kind: schema.EdgePersistsTo, Confidence: confidence,
				Evidence: []schema.Evidence{{Extractor: "fuzz", Rule: "fuzz"}},
			})
		}

		g, err := b.Build(schema.Root{Name: "fuzz"}, schema.Stats{})
		if err != nil {
			// Invalid input may legitimately be rejected; it must not panic.
			return
		}
		first, err := schema.Marshal(g)
		if err != nil {
			t.Fatalf("Marshal() = %v", err)
		}
		parsed, err := schema.Unmarshal(first)
		if err != nil {
			t.Fatalf("Unmarshal() = %v on our own output:\n%s", err, first)
		}
		second, err := schema.Marshal(parsed)
		if err != nil {
			t.Fatalf("Marshal() = %v", err)
		}
		if string(first) != string(second) {
			t.Fatalf("round trip changed the bytes:\n%s\n---\n%s", first, second)
		}
	})
}
