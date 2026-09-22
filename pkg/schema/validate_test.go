package schema_test

import (
	"math"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

func svc(id string) schema.Node {
	return schema.Node{
		ID: id, Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "n", Confidence: 1,
	}
}

// Validate is what stops a malformed graph reaching a file that gets
// committed and read by other tools. Each rule below is the only thing
// standing between a specific kind of nonsense and graph.json.
func TestNodeValidate(t *testing.T) {
	good := svc(schema.NewNodeID(schema.KindService, "k8s", "prod", "api"))
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed node did not validate: %v", err)
	}

	for _, tt := range []struct {
		name string
		mut  func(n *schema.Node)
		want string
	}{
		{"unknown kind", func(n *schema.Node) { n.Kind = "widget" }, "unknown kind"},
		{"unknown layer", func(n *schema.Node) { n.Layer = "orbit" }, "unknown layer"},
		{"empty name", func(n *schema.Node) { n.Name = "" }, "empty name"},
		{"confidence above one", func(n *schema.Node) { n.Confidence = 1.5 }, "out of range"},
		{"negative confidence", func(n *schema.Node) { n.Confidence = -0.1 }, "out of range"},
		{"malformed id", func(n *schema.Node) { n.ID = "nonsense" }, "node id"},
		{
			// An absolute path in a source would leak the scanning machine's
			// directory layout into a file people commit.
			"absolute source path",
			func(n *schema.Node) { n.Sources = []schema.Source{{Path: "/etc/passwd"}} },
			"source",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := good
			tt.mut(&n)
			err := n.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestEdgeValidate(t *testing.T) {
	from := schema.NewNodeID(schema.KindService, "k8s", "prod", "a")
	to := schema.NewNodeID(schema.KindService, "k8s", "prod", "b")
	good := schema.Edge{
		ID: "e:1", From: from, To: to, Kind: schema.EdgeCalls, Confidence: 0.8,
		Evidence: []schema.Evidence{{Extractor: "k8s", Rule: "dns_exact", Path: "a.yaml"}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed edge did not validate: %v", err)
	}

	for _, tt := range []struct {
		name string
		mut  func(e *schema.Edge)
		want string
	}{
		{"empty id", func(e *schema.Edge) { e.ID = "" }, "empty id"},
		{"malformed from", func(e *schema.Edge) { e.From = "nonsense" }, "from"},
		{"malformed to", func(e *schema.Edge) { e.To = "nonsense" }, "to"},
		{"unknown kind", func(e *schema.Edge) { e.Kind = "pokes" }, "unknown kind"},
		{"confidence out of range", func(e *schema.Edge) { e.Confidence = 2 }, "out of range"},
		{
			// The confidence model is only worth anything if every claim is
			// traceable, so an edge with no evidence is an assertion with no
			// basis.
			"no evidence",
			func(e *schema.Edge) { e.Evidence = nil },
			"no evidence",
		},
		{
			"evidence with no rule",
			func(e *schema.Edge) { e.Evidence = []schema.Evidence{{Extractor: "k8s"}} },
			"empty rule",
		},
		{
			"evidence with an absolute path",
			func(e *schema.Edge) {
				e.Evidence = []schema.Evidence{{Extractor: "k8s", Rule: "r", Path: "/tmp/x"}}
			},
			"evidence",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := good
			tt.mut(&e)
			if err := e.Validate(); err == nil {
				t.Fatalf("Validate() = nil for %s", tt.name)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestDiagnosticValidate(t *testing.T) {
	good := schema.Diagnostic{
		Severity: schema.SeverityWarn, Code: "helm_unrendered",
		Path: "charts/api/templates", Message: "not rendered",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed diagnostic did not validate: %v", err)
	}
	for _, tt := range []struct {
		name string
		mut  func(d *schema.Diagnostic)
		want string
	}{
		{"unknown severity", func(d *schema.Diagnostic) { d.Severity = "nagging" }, "unknown severity"},
		{"empty code", func(d *schema.Diagnostic) { d.Code = "" }, "empty code"},
		{"empty message", func(d *schema.Diagnostic) { d.Message = "" }, "empty message"},
		{"absolute path", func(d *schema.Diagnostic) { d.Path = "/var/log" }, "path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := good
			tt.mut(&d)
			if err := d.Validate(); err == nil {
				t.Fatalf("Validate() = nil for %s", tt.name)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// Confidences are combined arithmetically during resolution, so a stray
// 0.7000000000000001 would serialize differently from 0.7 and break
// byte-equality for no reason. Everything unrepresentable collapses to a
// number rather than reaching the JSON encoder, which cannot write NaN.
func TestConfidenceNormalization(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   float64
		want float64
	}{
		{"rounds to two decimals", 0.7000000000000001, 0.7},
		{"rounds half up", 0.005, 0.01},
		{"clamps above one", 1.5, 1},
		{"clamps below zero", -3, 0},
		{"NaN becomes zero", math.NaN(), 0},
		{"positive infinity becomes zero", math.Inf(1), 0},
		{"negative infinity becomes zero", math.Inf(-1), 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := svc(schema.NewNodeID(schema.KindService, "k8s", "prod", "api"))
			n.Confidence = tt.in
			g := schema.Graph{Nodes: []schema.Node{n}}
			g.Normalize()
			if got := g.Nodes[0].Confidence; got != tt.want {
				t.Errorf("confidence %v normalized to %v, want %v", tt.in, got, tt.want)
			}
			// Whatever it became has to be serializable: encoding/json
			// refuses NaN and Inf outright.
			if _, err := schema.Marshal(g); err != nil {
				t.Errorf("a graph with confidence %v does not marshal: %v", tt.in, err)
			}
		})
	}
}

// Two extractors describing one workload each know part of the story: a
// Dockerfile names the runtime, a manifest names the framework. Merging has
// to accumulate what each contributes without the more confident reading
// losing to whatever happened to arrive first.
func TestMergingAccumulatesWhatEachExtractorKnows(t *testing.T) {
	id := schema.NewNodeID(schema.KindService, "compose", "app", "api")
	tech := func(lang, runtime, framework string) *schema.Tech {
		return &schema.Tech{Language: lang, Runtime: runtime, Framework: framework}
	}

	t.Run("fields fill in from whichever contribution has them", func(t *testing.T) {
		b := schema.NewBuilder()
		n := svc(id)
		n.Tech = tech("go", "", "")
		b.AddNode(n)
		n2 := svc(id)
		n2.Tech = tech("", "go1.27", "chi")
		b.AddNode(n2)

		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		got := g.Nodes[0].Tech
		if got == nil || got.Language != "go" || got.Runtime != "go1.27" || got.Framework != "chi" {
			t.Errorf("merged tech = %+v, want every field filled from the contribution that had it", got)
		}
	})

	t.Run("a more confident contribution overwrites a field already set", func(t *testing.T) {
		b := schema.NewBuilder()
		n := svc(id)
		n.Confidence, n.Tech = 0.4, tech("javascript", "", "")
		b.AddNode(n)
		n2 := svc(id)
		n2.Confidence, n2.Tech = 0.95, tech("typescript", "", "")
		b.AddNode(n2)

		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Nodes[0].Tech; got == nil || got.Language != "typescript" {
			t.Errorf("merged language = %+v, want typescript from the 0.95 reading", got)
		}
		if got := g.Nodes[0].Confidence; got != 0.95 {
			t.Errorf("merged confidence = %v, want 0.95", got)
		}
	})

	t.Run("an equally confident contribution does not displace the first", func(t *testing.T) {
		// The pipeline merges in the walker's sorted path order, so "first"
		// is a stable fact about the repository rather than a race. See
		// AddNode.
		b := schema.NewBuilder()
		n := svc(id)
		n.Tech = tech("go", "", "")
		b.AddNode(n)
		n2 := svc(id)
		n2.Tech = tech("rust", "", "")
		b.AddNode(n2)

		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Nodes[0].Tech; got == nil || got.Language != "go" {
			t.Errorf("merged language = %+v, want go: a tie keeps the earlier reading", got)
		}
	})

	t.Run("a contribution with no tech leaves what is there", func(t *testing.T) {
		b := schema.NewBuilder()
		n := svc(id)
		n.Tech = tech("go", "", "")
		b.AddNode(n)
		b.AddNode(svc(id))

		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Nodes[0].Tech; got == nil || got.Language != "go" {
			t.Errorf("merged tech = %+v, want the go reading kept", got)
		}
	})
}
