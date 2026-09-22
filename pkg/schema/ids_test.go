package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

func TestNewNodeID(t *testing.T) {
	tests := []struct {
		name              string
		kind              schema.NodeKind
		scope, entityName string
		want              string
	}{
		{
			name: "a scope is marked so it cannot be read as part of the name",
			kind: schema.KindService, scope: "prod", entityName: "api-gateway",
			want: "container:@prod/api-gateway",
		},
		{
			name: "most components have no scope, and say so by omission",
			kind: schema.KindService, scope: "", entityName: "api-gateway",
			want: "container:api-gateway",
		},
		{
			name: "the layer is what the identifier carries, not the kind",
			kind: schema.KindDatastore, scope: "prod", entityName: "orders-db",
			want: "container:@prod/orders-db",
		},
		{
			// The reading that named an image and the reading that did not
			// have to land on one identifier, or one database is two boxes.
			name: "a datastore and a service with one name are one identifier",
			kind: schema.KindService, scope: "", entityName: "carts-db",
			want: "container:carts-db",
		},
		{
			name: "a boundary is distinguishable from what it contains",
			kind: schema.KindBoundary, scope: "", entityName: "podinfo",
			want: "context:podinfo",
		},
		{
			name: "case is folded so two spellings are one node",
			kind: schema.KindService, scope: "Prod", entityName: "API-Gateway",
			want: "container:@prod/api-gateway",
		},
		{
			name: "package names keep their slashes",
			kind: schema.KindPackage, scope: "", entityName: "github.com/spf13/cobra",
			want: "container:github.com/spf13/cobra",
		},
		{
			name: "runs of invalid characters collapse to one hyphen",
			kind: schema.KindService, scope: "prod", entityName: "my  weird!!name",
			want: "container:@prod/my-weird-name",
		},
		{
			name: "surrounding whitespace and punctuation are trimmed",
			kind: schema.KindService, scope: "prod", entityName: "  -api-  ",
			want: "container:@prod/api",
		},
		{
			name: "doubled and edge slashes in a name are cleaned up",
			kind: schema.KindPackage, scope: "", entityName: "/@scope//pkg/",
			want: "container:scope/pkg",
		},
		{
			name: "a name that normalizes away becomes unknown rather than empty",
			kind: schema.KindService, scope: "prod", entityName: "!!!",
			want: "container:@prod/unknown",
		},
		{
			// Folding cannot represent é at all, so the fold alone would put
			// this component and any other non-Latin name on the same id.
			name: "a name folding cannot represent carries a discriminator",
			kind: schema.KindService, scope: "prod", entityName: "café-service",
			want: "container:@prod/caf-service.21d4e6ea",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schema.NewNodeID(tt.kind, tt.scope, tt.entityName)
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

// The ambiguity the scope marker exists to prevent: a name may contain
// slashes, so position alone cannot say which segment is which.
func TestAScopeIsNeverConfusedWithASlashedName(t *testing.T) {
	scoped := schema.NewNodeID(schema.KindService, "acme", "api")
	slashed := schema.NewNodeID(schema.KindService, "", "acme/api")
	if scoped == slashed {
		t.Fatalf("the service api in scope acme and the module acme/api share an identifier: %q", scoped)
	}

	p, err := schema.ParseNodeID(scoped)
	if err != nil {
		t.Fatalf("ParseNodeID(%q) = %v", scoped, err)
	}
	if p.Scope != "acme" || p.Name != "api" {
		t.Errorf("parsed %q as scope=%q name=%q", scoped, p.Scope, p.Name)
	}
	if p, err = schema.ParseNodeID(slashed); err != nil {
		t.Fatalf("ParseNodeID(%q) = %v", slashed, err)
	}
	if p.Scope != "" || p.Name != "acme/api" {
		t.Errorf("parsed %q as scope=%q name=%q", slashed, p.Scope, p.Name)
	}
}

// Every kind that maps to one layer must produce one identifier. This is what
// lets a Compose file that named an image and a sibling that did not describe
// the same component.
func TestKindsSharingALayerShareAnIdentifier(t *testing.T) {
	container := []schema.NodeKind{
		schema.KindService, schema.KindDatastore, schema.KindQueue,
		schema.KindCloudResource, schema.KindPackage,
	}
	want := schema.NewNodeID(container[0], "prod", "thing")
	for _, kind := range container[1:] {
		if got := schema.NewNodeID(kind, "prod", "thing"); got != want {
			t.Errorf("NewNodeID(%q, ...) = %q, want %q", kind, got, want)
		}
	}

	// And a kind in another layer must not.
	if got := schema.NewNodeID(schema.KindBoundary, "prod", "thing"); got == want {
		t.Errorf("a boundary shares an identifier with a container: %q", got)
	}
}

func TestNodeIDIsIdempotent(t *testing.T) {
	// Feeding a constructed ID's parts back through the constructor must be
	// a fixed point; otherwise re-derived IDs would drift.
	for _, raw := range []string{"API Gateway", "user_service", "github.com/spf13/cobra", "x"} {
		id := schema.NewNodeID(schema.KindService, "prod", raw)
		p, err := schema.ParseNodeID(id)
		if err != nil {
			t.Fatalf("ParseNodeID(%q) = %v", id, err)
		}
		again := schema.NewNodeID(schema.KindService, p.Scope, p.Name)
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
		{"no layer prefix", "prod/api"},
		{"unknown layer", "widget:api"},
		{"a kind where a layer belongs", "service:api"},
		{"scope marker with no name", "container:@prod"},
		{"empty scope segment", "container:@/api"},
		{"not normalized", "container:@prod/API"},
		{"trailing slash leaves an empty name", "container:@prod/"},
		{"an unmarked scope is a name, and this one is not normalized", "container:@Prod/api"},
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
	const from, to = "container:@prod/api", "container:@prod/db"

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
		id := schema.NewNodeID(schema.KindService, "prod", n)
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
	want := schema.NewNodeID(schema.KindService, "prod", "api-gateway")
	for _, spelling := range []string{"API-Gateway", "  -api-gateway-  ", "api  gateway", "API gateway"} {
		if got := schema.NewNodeID(schema.KindService, "prod", spelling); got != want {
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
		id := schema.NewSymbolID(schema.KindService, "checkout", c.path, c.symbol)
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
	id := schema.NewSymbolID(schema.KindService, "checkout", "src/api/routes.go", "main")
	if want := "container:@checkout/src/api/routes.go/main"; id != want {
		t.Errorf("NewSymbolID() = %q, want %q", id, want)
	}
}

// Rebuilding an id from its parts has to be a fixed point, or a graph that is
// saved and reloaded would come back with different identities.
func TestQualifiedIDsAreIdempotent(t *testing.T) {
	for _, id := range []string{
		schema.NewNodeID(schema.KindService, "prod", "café-service"),
		schema.NewSymbolID(schema.KindService, "checkout", "src/api/handlers.go", "HandleOrder"),
	} {
		p, err := schema.ParseNodeID(id)
		if err != nil {
			t.Fatalf("ParseNodeID(%q) = %v", id, err)
		}
		if got := schema.NewNodeID(schema.KindService, p.Scope, p.Name); got != id {
			t.Errorf("rebuilding %q gave %q", id, got)
		}
	}
}
