package schema_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// graph.json is committed, cached against, and diffed by drift detection, so
// two scans that find the same thing have to produce the same bytes. The
// nodes and edges are sorted by an id that the builder guarantees is unique,
// which settles them. The lists *inside* a node and an edge are not: they are
// sorted by their contents, and the sort is only a total order if it compares
// every field the deduplication treats as significant.
//
// It did not. normalizeEvidence compared four of Evidence's five fields, so
// two entries differing only in Extractor were equal to the sort and distinct
// to the dedup. Both survived, in whatever order the concurrent pipeline had
// added them, and the same repository produced two different content hashes.
//
// These tests are built by reflection over the struct rather than from a
// hand-written list of cases, so that a field added later is covered without
// anyone remembering to come back here.

// variants returns 2 values of T per exported field, each one base with that
// single field overwritten. Every pair differs in exactly one field, and all
// of them are valid, because base is.
func variants[T any](t *testing.T, base T) []T {
	t.Helper()
	typ := reflect.TypeOf(base)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("variants: %s is not a struct", typ)
	}
	var out []T
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		for _, n := range []int{1, 2} {
			v := reflect.New(typ).Elem()
			v.Set(reflect.ValueOf(base))
			field := v.Field(i)
			switch field.Kind() {
			case reflect.String:
				field.SetString(fmt.Sprintf("%s-%d", f.Name, n))
			case reflect.Int:
				// Offset past anything base is likely to hold, so a variant
				// never collides with base or with another field's variant.
				field.SetInt(int64(100 + n))
			default:
				t.Fatalf("variants: %s.%s has unhandled kind %s; extend this helper",
					typ, f.Name, field.Kind())
			}
			out = append(out, v.Interface().(T))
		}
	}
	return out
}

// rotations returns the input rotated by every offset, which is enough to put
// each element in a different position relative to the others without needing
// a random source a test would then have to seed.
func rotations[T any](in []T) [][]T {
	var out [][]T
	for i := range in {
		r := make([]T, 0, len(in))
		r = append(r, in[i:]...)
		r = append(r, in[:i]...)
		out = append(out, r)
	}
	return out
}

func nodeID(name string) string {
	return schema.NewNodeID(schema.KindService, "compose", "app", name)
}

func TestEvidenceOrderDoesNotChangeTheGraph(t *testing.T) {
	all := variants(t, schema.Evidence{
		Extractor: "base", Path: "base.yml", Line: 1,
		Rule: "base_rule", Detail: "base detail",
	})
	var first string
	for i, ordering := range rotations(all) {
		b := schema.NewBuilder()
		for _, n := range []string{"a", "b"} {
			b.AddNode(schema.Node{
				ID: nodeID(n), Kind: schema.KindService,
				Layer: schema.LayerContainer, Name: n, Confidence: 1,
			})
		}
		// Added one at a time, which is how the resolver accumulates
		// evidence onto an edge several rules agree on.
		for _, e := range ordering {
			b.AddEdge(schema.Edge{
				From: nodeID("a"), To: nodeID("b"), Kind: schema.EdgeCalls,
				Confidence: 0.8, Evidence: []schema.Evidence{e},
			})
		}
		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Edges) != 1 {
			t.Fatalf("got %d edges, want 1", len(g.Edges))
		}
		if len(g.Edges[0].Evidence) != len(all) {
			t.Fatalf("got %d evidence entries, want %d: the dedup dropped a distinct one",
				len(g.Edges[0].Evidence), len(all))
		}
		if i == 0 {
			first = g.ContentHash
			continue
		}
		if g.ContentHash != first {
			t.Fatalf("rotation %d produced content hash %s, want %s: evidence arrival order changed graph.json",
				i, g.ContentHash, first)
		}
	}
}

func TestSourceOrderDoesNotChangeTheGraph(t *testing.T) {
	all := variants(t, schema.Source{Extractor: "base", Path: "base.yml", Line: 1})
	var first string
	for i, ordering := range rotations(all) {
		b := schema.NewBuilder()
		// One node added repeatedly, which is how two extractors describing
		// the same workload merge their sources onto it.
		for _, s := range ordering {
			b.AddNode(schema.Node{
				ID: nodeID("a"), Kind: schema.KindService,
				Layer: schema.LayerContainer, Name: "a", Confidence: 1,
				Sources: []schema.Source{s},
			})
		}
		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Nodes[0].Sources) != len(all) {
			t.Fatalf("got %d sources, want %d: the dedup dropped a distinct one",
				len(g.Nodes[0].Sources), len(all))
		}
		if i == 0 {
			first = g.ContentHash
			continue
		}
		if g.ContentHash != first {
			t.Fatalf("rotation %d produced content hash %s, want %s: source arrival order changed graph.json",
				i, g.ContentHash, first)
		}
	}
}

// Diagnostics are never deduplicated -- two files can legitimately produce
// the identical message -- so their sort carries the whole burden of a stable
// order. Until this test, nothing had ever sorted two diagnostics that tied
// on path, so diagLess had no coverage at all.
func TestDiagnosticOrderDoesNotChangeTheGraph(t *testing.T) {
	all := variants(t, schema.Diagnostic{
		Severity: schema.SeverityWarn, Code: "base_code",
		Path: "base.yml", Line: 1, Message: "base message",
	})
	// Severity is typed, so the reflection helper fills it with a string
	// that is not a valid severity. Put a real one back; the field still
	// varies across the set via the other valid values.
	severities := schema.Severities()
	for i := range all {
		if !all[i].Severity.Valid() {
			all[i].Severity = severities[i%len(severities)]
		}
	}

	var first string
	for i, ordering := range rotations(all) {
		b := schema.NewBuilder()
		b.AddNode(schema.Node{
			ID: nodeID("a"), Kind: schema.KindService,
			Layer: schema.LayerContainer, Name: "a", Confidence: 1,
		})
		for _, d := range ordering {
			b.Diag(d)
		}
		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Diagnostics) != len(all) {
			t.Fatalf("got %d diagnostics, want %d", len(g.Diagnostics), len(all))
		}
		if i == 0 {
			first = g.ContentHash
			continue
		}
		if g.ContentHash != first {
			t.Fatalf("rotation %d produced content hash %s, want %s: diagnostic order changed graph.json",
				i, g.ContentHash, first)
		}
	}
}

// Edges are keyed by an id derived from the endpoints, the kind and the
// protocol, so edges differing in any of those are distinct rows that have to
// sort into one order regardless of which extractor reported them first.
func TestEdgeOrderDoesNotChangeTheGraph(t *testing.T) {
	names := []string{"a", "b", "c"}
	b0 := func() *schema.Builder {
		b := schema.NewBuilder()
		for _, n := range names {
			b.AddNode(schema.Node{
				ID: nodeID(n), Kind: schema.KindService,
				Layer: schema.LayerContainer, Name: n, Confidence: 1,
			})
		}
		return b
	}
	ev := []schema.Evidence{{Extractor: "k8s", Rule: "dns_exact", Path: "a.yaml"}}
	var edges []schema.Edge
	// Vary each component of the identity in turn, so every branch of the
	// comparator has a pair that reaches it.
	for _, from := range names {
		for _, to := range names {
			if from == to {
				continue
			}
			for _, kind := range []schema.EdgeKind{schema.EdgeCalls, schema.EdgePersistsTo} {
				for _, proto := range []string{"", "http", "grpc"} {
					edges = append(edges, schema.Edge{
						From: nodeID(from), To: nodeID(to), Kind: kind,
						Protocol: proto, Confidence: 0.8, Evidence: ev,
					})
				}
			}
		}
	}

	var first string
	for i, ordering := range rotations(edges) {
		b := b0()
		for _, e := range ordering {
			b.AddEdge(e)
		}
		g, err := b.Build(schema.Root{Name: "app"}, schema.Stats{})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Edges) != len(edges) {
			t.Fatalf("got %d edges, want %d: two distinct edges collapsed into one id",
				len(g.Edges), len(edges))
		}
		if i == 0 {
			first = g.ContentHash
			continue
		}
		if g.ContentHash != first {
			t.Fatalf("rotation %d produced content hash %s, want %s: edge order changed graph.json",
				i, g.ContentHash, first)
		}
	}
}
