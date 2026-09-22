package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

func boundaryID(t *testing.T, scope, name string) string {
	t.Helper()
	return schema.NewNodeID(schema.KindBoundary, scope, name)
}

func TestQualifyProject(t *testing.T) {
	id := boundaryID(t, "prod", "prod")

	t.Run("no prefix leaves the id alone", func(t *testing.T) {
		got, err := schema.QualifyProject(id, "")
		if err != nil || got != id {
			t.Errorf("QualifyProject(%q, \"\") = (%q, %v), want it unchanged", id, got, err)
		}
	})

	t.Run("the prefix separates two projects sharing a namespace", func(t *testing.T) {
		a, err := schema.QualifyProject(id, "web")
		if err != nil {
			t.Fatal(err)
		}
		b, err := schema.QualifyProject(id, "billing")
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
				t.Errorf("QualifyProject produced an invalid id %q: %v", got, err)
			}
		}
	})

	t.Run("the name is preserved exactly", func(t *testing.T) {
		// A name may already carry a discriminator from its own folding, and
		// rebuilding it would fold it twice.
		orig := schema.NewNodeID(schema.KindService, "prod", "Ünïcödé Service")
		name := orig[strings.LastIndex(orig, "/")+1:]
		got, err := schema.QualifyProject(orig, "web")
		if err != nil {
			t.Fatal(err)
		}
		if gotName := got[strings.LastIndex(got, "/")+1:]; gotName != name {
			t.Errorf("name segment = %q, want %q unchanged", gotName, name)
		}
	})

	t.Run("a node with no scope gains one, which is the project", func(t *testing.T) {
		plain := schema.NewNodeID(schema.KindService, "", "api")
		got, err := schema.QualifyProject(plain, "billing")
		if err != nil {
			t.Fatal(err)
		}
		if got != "container:@billing/api" {
			t.Errorf("QualifyProject(%q, \"billing\") = %q, want container:@billing/api", plain, got)
		}
		if err := schema.ValidateNodeID(got); err != nil {
			t.Errorf("%q is invalid: %v", got, err)
		}
	})

	t.Run("a prefix that folds to nothing still yields a valid id", func(t *testing.T) {
		got, err := schema.QualifyProject(id, "!!!")
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.ValidateNodeID(got); err != nil {
			t.Errorf("QualifyProject(%q, \"!!!\") = %q, which is invalid: %v", id, got, err)
		}
	})
}

func TestQualifyProjectRejectsMalformedIDs(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"no layer", "prod/web"},
		{"no name after a scope", "context:@prod"},
		{"empty scope", "context:@/web"},
		{"empty name", "context:@prod/"},
		{"empty id", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := schema.QualifyProject(tt.id, "web")
			if err == nil {
				t.Fatalf("QualifyProject(%q) = (%q, nil), want an error", tt.id, got)
			}
			if !errors.Is(err, schema.ErrInvalidNodeID) {
				t.Errorf("error = %v, want one wrapping ErrInvalidNodeID so callers can tell it apart", err)
			}
		})
	}
}

// The hyphen join is ambiguous, and this pins that it is, because the fix
// lives one level up.
//
// internal/scan's projectLabels detects two projects folding to one segment
// and passes prefixes that cannot collide;
// TestProjectsWhoseNamespacesWouldFoldTogetherStayApart is the end-to-end
// assertion. This one holds the reason that pass has to exist. If it ever
// starts failing, this function grew a defence of its own and the caller
// should be re-examined rather than left doing work twice.
func TestQualifyProjectCollision(t *testing.T) {
	a, err := schema.QualifyProject(boundaryID(t, "prod", "ns"), "web-api")
	if err != nil {
		t.Fatal(err)
	}
	b, err := schema.QualifyProject(boundaryID(t, "api-prod", "ns"), "web")
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
