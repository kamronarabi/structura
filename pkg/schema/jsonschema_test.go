package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kamronarabi/structura/internal/golden"
	structura "github.com/kamronarabi/structura/pkg/schema"
)

const schemaFile = "schema.json"

// schema.json is a published artifact: the cloud tier and any third-party
// consumer read it to validate graphs produced by CLI versions they do not
// have. Generating it from the Go types rather than hand-writing it means the
// two cannot drift, and the golden comparison turns any change into a visible
// diff that forces a deliberate SchemaVersion decision.
func generateJSONSchema(t *testing.T) []byte {
	t.Helper()

	r := &jsonschema.Reflector{
		// Inline everything reachable from Graph rather than splitting it
		// across $defs by package path, so the published file is readable
		// and self-contained.
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: false,
		DoNotReference:             false,
	}
	s := r.Reflect(&structura.Graph{})
	s.ID = "https://structura.dev/schema/sag-v" + structura.Version + ".json"
	s.Title = "Structura Architecture Graph"
	s.Description = "Graph schema version " + structura.Version +
		". Versioned independently of the structura CLI."

	// Reflection sees the enum types as plain strings. The constraints are
	// injected here, from the same exported lists the runtime validates
	// against, so the published schema is as strict as the Go types are.
	constrain(t, s, "Node", "kind", toAny(structura.NodeKinds()))
	constrain(t, s, "Node", "layer", toAny(structura.Layers()))
	constrain(t, s, "Edge", "kind", toAny(structura.EdgeKinds()))
	constrain(t, s, "Diagnostic", "severity", toAny(structura.Severities()))
	bound(t, s, "Node", "confidence")
	bound(t, s, "Edge", "confidence")

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("generating JSON Schema: %v", err)
	}
	return append(out, '\n')
}

func TestJSONSchemaIsPublishedAndCurrent(t *testing.T) {
	golden.Assert(t, schemaFile, generateJSONSchema(t))
}

// The generated schema is only useful if real output actually satisfies it.
func TestGoldenGraphValidatesAgainstSchema(t *testing.T) {
	schemaBytes, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("reading %s: %v (run `make golden` first)", schemaFile, err)
	}
	doc, err := validator.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatalf("parsing %s: %v", schemaFile, err)
	}

	c := validator.NewCompiler()
	if err := c.AddResource(schemaFile, doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile(schemaFile)
	if err != nil {
		t.Fatalf("compiling %s: %v", schemaFile, err)
	}

	graphBytes, err := os.ReadFile(filepath.Join("testdata", "golden", "graph.json"))
	if err != nil {
		t.Fatalf("reading golden graph: %v", err)
	}
	instance, err := validator.UnmarshalJSON(bytes.NewReader(graphBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Errorf("the golden graph does not satisfy the published schema:\n%v", err)
	}
}

// An empty repository still has to produce a schema-valid graph. Fixture 6
// in the plan asserts the CLI exits 0 on a garbage repo; this asserts the
// output is well-formed rather than merely present.
func TestEmptyGraphValidatesAgainstSchema(t *testing.T) {
	b := structura.NewBuilder()
	g, err := b.Build(structura.Root{Name: "empty"}, structura.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := structura.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}

	schemaBytes, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Skipf("%s not generated yet", schemaFile)
	}
	doc, err := validator.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	c := validator.NewCompiler()
	if err := c.AddResource(schemaFile, doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := validator.UnmarshalJSON(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Errorf("an empty graph does not satisfy the published schema:\n%v", err)
	}
}

func toAny[T ~string](vals []T) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// constrain adds an enum constraint to one property of one definition.
func constrain(t *testing.T, s *jsonschema.Schema, def, prop string, values []any) {
	t.Helper()
	property(t, s, def, prop).Enum = values
}

// bound restricts a confidence property to [0,1], matching Validate.
func bound(t *testing.T, s *jsonschema.Schema, def, prop string) {
	t.Helper()
	p := property(t, s, def, prop)
	p.Minimum = json.Number("0")
	p.Maximum = json.Number("1")
}

func property(t *testing.T, s *jsonschema.Schema, def, prop string) *jsonschema.Schema {
	t.Helper()
	d, ok := s.Definitions[def]
	if !ok {
		t.Fatalf("definition %q is missing from the reflected schema", def)
	}
	p, ok := d.Properties.Get(prop)
	if !ok {
		t.Fatalf("property %q is missing from definition %q", prop, def)
	}
	return p
}

// A schema that accepts everything is worse than no schema, because it
// creates false confidence downstream. This proves the published constraints
// actually reject bad input.
func TestSchemaRejectsInvalidGraphs(t *testing.T) {
	compiled := compileSchema(t)

	tests := []struct {
		name  string
		graph string
	}{
		{
			name: "unknown node kind",
			graph: `{"schemaVersion":"0.1.0","generatedAt":"2026-01-01T00:00:00Z","contentHash":"",
			  "root":{"name":"x"},"nodes":[{"id":"widget:a/b/c","kind":"widget","layer":"container",
			  "name":"c","confidence":1}],"edges":[],"diagnostics":[],
			  "stats":{"filesScanned":0,"filesParsed":0,"nodeCount":1,"edgeCount":0,"durationMs":0}}`,
		},
		{
			name: "confidence above one",
			graph: `{"schemaVersion":"0.1.0","generatedAt":"2026-01-01T00:00:00Z","contentHash":"",
			  "root":{"name":"x"},"nodes":[{"id":"container:@b/c","kind":"service","layer":"container",
			  "name":"c","confidence":4}],"edges":[],"diagnostics":[],
			  "stats":{"filesScanned":0,"filesParsed":0,"nodeCount":1,"edgeCount":0,"durationMs":0}}`,
		},
		{
			name: "unknown edge kind",
			graph: `{"schemaVersion":"0.1.0","generatedAt":"2026-01-01T00:00:00Z","contentHash":"",
			  "root":{"name":"x"},"nodes":[],"edges":[{"id":"e:1","from":"a","to":"b","kind":"teleports_to",
			  "confidence":1}],"diagnostics":[],
			  "stats":{"filesScanned":0,"filesParsed":0,"nodeCount":0,"edgeCount":1,"durationMs":0}}`,
		},
		{
			name: "unknown severity",
			graph: `{"schemaVersion":"0.1.0","generatedAt":"2026-01-01T00:00:00Z","contentHash":"",
			  "root":{"name":"x"},"nodes":[],"edges":[],
			  "diagnostics":[{"severity":"catastrophe","code":"x","message":"y"}],
			  "stats":{"filesScanned":0,"filesParsed":0,"nodeCount":0,"edgeCount":0,"durationMs":0}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance, err := validator.UnmarshalJSON(strings.NewReader(tt.graph))
			if err != nil {
				t.Fatalf("test fixture is not valid JSON: %v", err)
			}
			if err := compiled.Validate(instance); err == nil {
				t.Error("schema accepted a graph it should have rejected")
			}
		})
	}
}

func compileSchema(t *testing.T) *validator.Schema {
	t.Helper()
	b, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaFile, err)
	}
	doc, err := validator.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := validator.NewCompiler()
	if err := c.AddResource(schemaFile, doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}
