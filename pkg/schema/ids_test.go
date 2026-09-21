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
			// Folding cannot represent é at all, so the fold alone would put
			// this component and any other non-Latin name on the same id.
			name: "a name folding cannot represent carries a discriminator",
			kind: schema.KindService, source: "k8s", namespace: "prod", entityName: "café-service",
			want: "service:k8s/prod/caf-service.21d4e6ea",
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

// Every non-Latin identifier folded onto the same id: a service named in
// Japanese, Chinese, or Russian all became "unknown", so a repository with
// three of them had one node where it should have had three, with nothing to
// show that anything had been merged.
func TestNamesFoldingCannotRepresentStayDistinct(t *testing.T) {
	names := []string{"ハンドラ", "处理器", "Обработчик", "café", "cafè", "naïve"}

	seen := map[string]string{}
	for _, n := range names {
		id := schema.NewNodeID(schema.KindService, "k8s", "prod", n)
		if prev, ok := seen[id]; ok {
			t.Errorf("%q and %q share the id %s", prev, n, id)
		}
		seen[id] = n
		if err := schema.ValidateNodeID(id); err != nil {
			t.Errorf("ValidateNodeID(%q) = %v", id, err)
		}
	}
}

// Spelling variance is what the merge exists to reconcile, so punctuation and
// case must keep folding together. A discriminator here would split one
// component into several, which is the opposite failure.
func TestSpellingVariantsStillFoldTogether(t *testing.T) {
	want := schema.NewNodeID(schema.KindService, "k8s", "prod", "api-gateway")
	for _, spelling := range []string{"API-Gateway", "  -api-gateway-  ", "api  gateway", "API gateway"} {
		if got := schema.NewNodeID(schema.KindService, "k8s", "prod", spelling); got != want {
			t.Errorf("%q gave %s, want %s; one component must not become two", spelling, got, want)
		}
	}
}

// A code symbol is read once, from one file, with the characters the language
// requires -- so every difference is a real difference. Go's HandleOrder and
// handleOrder are two functions that routinely both exist, one wrapping the
// other, and folding them together would silently halve a component view.
func TestSymbolIDsKeepEveryDifference(t *testing.T) {
	cases := []struct{ path, symbol string }{
		{"src/api/handlers.go", "HandleOrder"},
		{"src/api/handlers.go", "handleOrder"},
		{"src/api/handlers.go", "handleorder"},
		{"src/api/handlers.go", "HANDLEORDER"},
		{"src/api/routes.go", "GET /orders/{id}"},
		{"src/api/routes.go", "GET /orders/:id"},
		{"src/api/routes.go", "GET /orders/id"},
		{"src/api/routes.go", "POST /orders/{id}"},
		{"src/db/store.go", "Repository[T]"},
		{"src/db/store.go", "Repository[U]"},
		{"src/db/store.go", "Repository-T"},
		{"src/db/store.go", "(*Store).Get"},
		{"src/db/store.go", "Store.Get"},
		// The same symbol in another file is another symbol.
		{"src/admin/handlers.go", "HandleOrder"},
	}

	seen := map[string]string{}
	for _, c := range cases {
		id := schema.NewSymbolID(schema.KindService, "go", "checkout", c.path, c.symbol)
		key := c.path + "::" + c.symbol
		if prev, ok := seen[id]; ok {
			t.Errorf("%s and %s share the id %s", prev, key, id)
		}
		seen[id] = key
		// An id the constructor produces must survive validation, or merge
		// would split one node in two.
		if err := schema.ValidateNodeID(id); err != nil {
			t.Errorf("ValidateNodeID(%q) = %v", id, err)
		}
	}
}

// A symbol that needs no discrimination should not carry any: the id is what
// a model quotes back, and a hash on every entry would be noise.
func TestASimpleSymbolStaysReadable(t *testing.T) {
	id := schema.NewSymbolID(schema.KindService, "go", "checkout", "src/api/routes.go", "main")
	if want := "service:go/checkout/src/api/routes.go/main"; id != want {
		t.Errorf("NewSymbolID() = %q, want %q", id, want)
	}
}

// Rebuilding an id from its parts has to be a fixed point, or a graph that is
// saved and reloaded would come back with different identities.
func TestQualifiedIDsAreIdempotent(t *testing.T) {
	for _, id := range []string{
		schema.NewNodeID(schema.KindService, "k8s", "prod", "café-service"),
		schema.NewSymbolID(schema.KindService, "go", "checkout", "src/api/handlers.go", "HandleOrder"),
	} {
		p, err := schema.ParseNodeID(id)
		if err != nil {
			t.Fatalf("ParseNodeID(%q) = %v", id, err)
		}
		if got := schema.NewNodeID(p.Kind, p.Source, p.Namespace, p.Name); got != id {
			t.Errorf("rebuilding %q gave %q", id, got)
		}
	}
}
