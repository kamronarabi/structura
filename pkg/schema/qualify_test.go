package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

func boundaryID(t *testing.T, namespace, name string) string {
	t.Helper()
	return schema.NewNodeID(schema.KindBoundary, "k8s", namespace, name)
}

func TestQualifyNamespace(t *testing.T) {
	id := boundaryID(t, "prod", "prod")

	t.Run("no prefix leaves the id alone", func(t *testing.T) {
		got, err := schema.QualifyNamespace(id, "")
		if err != nil || got != id {
			t.Errorf("QualifyNamespace(%q, \"\") = (%q, %v), want it unchanged", id, got, err)
		}
	})

	t.Run("the prefix separates two projects sharing a namespace", func(t *testing.T) {
		a, err := schema.QualifyNamespace(id, "web")
		if err != nil {
			t.Fatal(err)
		}
		b, err := schema.QualifyNamespace(id, "billing")
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Fatalf("two projects produced one id: %q", a)
		}
		// The whole point: the result has to be an id the rest of the system
		// accepts, which means surviving re-normalization unchanged.
		for _, got := range []string{a, b} {
			if err := schema.ValidateNodeID(got); err != nil {
				t.Errorf("QualifyNamespace produced an invalid id %q: %v", got, err)
			}
		}
	})

	t.Run("the name is preserved exactly", func(t *testing.T) {
		// A name may already carry a discriminator from its own folding, and
		// rebuilding it would fold it twice.
		orig := schema.NewNodeID(schema.KindService, "k8s", "prod", "Ünïcödé Service")
		name := orig[strings.LastIndex(orig, "/")+1:]
		got, err := schema.QualifyNamespace(orig, "web")
		if err != nil {
			t.Fatal(err)
		}
		if gotName := got[strings.LastIndex(got, "/")+1:]; gotName != name {
			t.Errorf("name segment = %q, want %q unchanged", gotName, name)
		}
	})

	t.Run("a prefix that folds to nothing still yields a valid id", func(t *testing.T) {
		got, err := schema.QualifyNamespace(id, "!!!")
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.ValidateNodeID(got); err != nil {
			t.Errorf("QualifyNamespace(%q, \"!!!\") = %q, which is invalid: %v", id, got, err)
		}
	})
}

func TestQualifyNamespaceRejectsMalformedIDs(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"no kind", "k8s/prod/web"},
		{"no namespace", "boundary:k8s"},
		{"no name", "boundary:k8s/prod"},
		{"empty namespace", "boundary:k8s//web"},
		{"empty name", "boundary:k8s/prod/"},
		{"empty id", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := schema.QualifyNamespace(tt.id, "web")
			if err == nil {
				t.Fatalf("QualifyNamespace(%q) = (%q, nil), want an error", tt.id, got)
			}
			if !errors.Is(err, schema.ErrInvalidNodeID) {
				t.Errorf("error = %v, want one wrapping ErrInvalidNodeID so callers can tell it apart", err)
			}
		})
	}
}

// A known limitation, pinned so that it is visible rather than remembered.
//
// The prefix and the namespace are joined with a hyphen and folded together,
// and a hyphen is an ordinary character in both, so the join is ambiguous.
// Two different projects collapse into the one boundary this function exists
// to keep apart.
//
// This asserts the collision rather than the fix, because the fix is not
// available here: see QualifyNamespace's doc comment. When the caller learns
// to disambiguate, this test should start failing, and that is the signal to
// replace it with the property it was standing in for.
func TestQualifyNamespaceCollision(t *testing.T) {
	a, err := schema.QualifyNamespace(boundaryID(t, "prod", "ns"), "web-api")
	if err != nil {
		t.Fatal(err)
	}
	b, err := schema.QualifyNamespace(boundaryID(t, "api-prod", "ns"), "web")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the hyphen ambiguity is gone: %q != %q.\n"+
			"If the caller now disambiguates, delete this test and assert the property instead.", a, b)
	}
	t.Logf("known limitation: project %q namespace %q and project %q namespace %q both yield %s",
		"web-api", "prod", "web", "api-prod", a)
}
