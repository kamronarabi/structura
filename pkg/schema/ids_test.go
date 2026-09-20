package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

func TestNewNodeID(t *testing.T) {
	tests := []struct {
		name                          string
		kind                          schema.NodeKind
		source, namespace, entityName string
		want                          string
	}{
		{
			name: "plain k8s service",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "api-gateway",
			want: "service:k8s/prod/api-gateway",
		},
		{
			name: "case is folded so two spellings are one node",
			kind: schema.KindService, source: "K8s", namespace: "Prod", entityName: "API-Gateway",
			want: "service:k8s/prod/api-gateway",
		},
		{
			name: "package names keep their slashes",
			kind: schema.KindPackage, source: "gomod", namespace: "root", entityName: "github.com/spf13/cobra",
			want: "package:gomod/root/github.com/spf13/cobra",
		},
		{
			name: "missing namespace falls back to default",
			kind: schema.KindDatastore, source: "compose", namespace: "", entityName: "postgres",
			want: "datastore:compose/default/postgres",
		},
		{
			name: "missing source falls back to unknown",
			kind: schema.KindExternal, source: "", namespace: "net", entityName: "api.stripe.com",
			want: "external:unknown/net/api.stripe.com",
		},
		{
			name: "runs of invalid characters collapse to one hyphen",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "my  weird!!name",
			want: "service:k8s/prod/my-weird-name",
		},
		{
			name: "surrounding whitespace and punctuation are trimmed",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "  -api-  ",
			want: "service:k8s/prod/api",
		},
		{
			name: "doubled and edge slashes in a name are cleaned up",
			kind: schema.KindPackage, source: "npm", namespace: "root", entityName: "/@scope//pkg/",
			want: "package:npm/root/scope/pkg",
		},
		{
			name: "a name that normalizes away becomes unknown rather than empty",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "!!!",
			want: "service:k8s/prod/unknown",
		},
		{
			name: "unicode folds to hyphens rather than being dropped",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "café-service",
			want: "service:k8s/prod/caf-service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schema.NewNodeID(tt.kind, tt.source, tt.namespace, tt.entityName)
			if got != tt.want {
				t.Errorf("NewNodeID() = %q, want %q", got, tt.want)
			}
			// Everything the constructor produces must survive validation,
			// or merge would silently split one node into two.
			if err := schema.ValidateNodeID(got); err != nil {
				t.Errorf("ValidateNodeID(%q) = %v, want nil", got, err)
			}
		})
	}
}

func TestNodeIDIsIdempotent(t *testing.T) {
	// Feeding a constructed ID's parts back through the constructor must be
	// a fixed point; otherwise re-derived IDs would drift.
	for _, raw := range []string{"API Gateway", "user_service", "github.com/spf13/cobra", "x"} {
		id := schema.NewNodeID(schema.KindService, "k8s", "prod", raw)
		p, err := schema.ParseNodeID(id)
		if err != nil {
			t.Fatalf("ParseNodeID(%q) = %v", id, err)
		}
		again := schema.NewNodeID(p.Kind, p.Source, p.Namespace, p.Name)
		if again != id {
			t.Errorf("round trip of %q: got %q, want %q", raw, again, id)
		}
	}
}

func TestParseNodeIDRejects(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"no kind prefix", "k8s/prod/api"},
		{"unknown kind", "widget:k8s/prod/api"},
		{"too few segments", "service:k8s/api"},
		{"empty source segment", "service:/prod/api"},
		{"empty namespace segment", "service:k8s//api"},
		{"not normalized", "service:k8s/prod/API"},
		{"trailing slash leaves an empty name", "service:k8s/prod/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := schema.ValidateNodeID(tt.id); err == nil {
				t.Fatalf("ValidateNodeID(%q) = nil, want an error", tt.id)
			} else if !errors.Is(err, schema.ErrInvalidNodeID) {
				t.Errorf("error %v does not wrap ErrInvalidNodeID", err)
			}
		})
	}
}

func TestNewEdgeIDIsStableAndDistinct(t *testing.T) {
	const from, to = "service:k8s/prod/api", "datastore:k8s/prod/db"

	a := schema.NewEdgeID(from, to, schema.EdgePersistsTo, "postgres")
	if b := schema.NewEdgeID(from, to, schema.EdgePersistsTo, "postgres"); a != b {
		t.Errorf("same inputs gave different ids: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "e:") {
		t.Errorf("edge id %q lacks the e: prefix", a)
	}

	// Direction, kind, and protocol must each change the identity, or
	// distinct relationships would collapse into one during merge.
	others := map[string]string{
		"reversed": schema.NewEdgeID(to, from, schema.EdgePersistsTo, "postgres"),
		"kind":     schema.NewEdgeID(from, to, schema.EdgeCalls, "postgres"),
		"protocol": schema.NewEdgeID(from, to, schema.EdgePersistsTo, "mysql"),
	}
	for what, id := range others {
		if id == a {
			t.Errorf("changing %s did not change the edge id", what)
		}
	}
}
