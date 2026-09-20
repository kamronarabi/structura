package yamlpos_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/yamlpos"
)

const doc = `version: "3"
services:
  web:
    image: acme/web
    ports:
      - "8080:8080"
  db:
    image: postgres:16
`

func TestLineAndKeyLine(t *testing.T) {
	l := yamlpos.New([]byte(doc))

	tests := []struct {
		name string
		path []string
		want int
	}{
		// Line reports where the value begins; for a block mapping
		// that is the line after the key. KeyLine gives the key.
		{"top-level key's value block", []string{"services"}, 3},
		{"nested service", []string{"services", "web"}, 4},
		{"leaf value", []string{"services", "web", "image"}, 4},
		{"second service", []string{"services", "db"}, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l.Line(tt.path...); got != tt.want {
				t.Errorf("Line(%v) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}

	// KeyLine points at the key, which is what a reader scans for when the
	// value is a block starting on the following line.
	if got := l.KeyLine("services", "web"); got != 3 {
		t.Errorf("KeyLine(services, web) = %d, want 3", got)
	}
	if got := l.KeyLine("services", "db"); got != 7 {
		t.Errorf("KeyLine(services, db) = %d, want 7", got)
	}
}

func TestMissingPathReturnsZero(t *testing.T) {
	l := yamlpos.New([]byte(doc))
	for _, path := range [][]string{
		{"nope"}, {"services", "nope"}, {"services", "web", "nope"},
		{"services", "web", "image", "deeper"}, {},
	} {
		if got := l.Line(path...); got != 0 {
			t.Errorf("Line(%v) = %d, want 0", path, got)
		}
	}
}

func TestMultiDocument(t *testing.T) {
	multi := "kind: Service\nmetadata:\n  name: a\n---\nkind: Deployment\nmetadata:\n  name: b\n"
	l := yamlpos.New([]byte(multi))

	if l.Docs() != 2 {
		t.Fatalf("Docs() = %d, want 2", l.Docs())
	}
	if got := l.LineIn(1, "metadata", "name"); got != 7 {
		t.Errorf("LineIn(1, metadata, name) = %d, want 7", got)
	}
	// Line searches every document and takes the first match.
	if got := l.Line("metadata", "name"); got != 3 {
		t.Errorf("Line(metadata, name) = %d, want 3", got)
	}
	if got := l.LineIn(9, "kind"); got != 0 {
		t.Errorf("LineIn() on a document that does not exist = %d, want 0", got)
	}
}

func TestAnchorsResolve(t *testing.T) {
	anchored := "base: &base\n  image: acme/api\nservices:\n  web: *base\n"
	l := yamlpos.New([]byte(anchored))

	// An aliased mapping resolves to the anchor it points at, so a service
	// defined by reference still yields a position.
	if got := l.Line("services", "web", "image"); got == 0 {
		t.Error("an aliased mapping produced no position")
	}
}

// A locator that cannot parse its input returns zeroes rather than failing:
// a missing line number is a far smaller loss than a failed extraction.
func TestMalformedInputDegrades(t *testing.T) {
	for _, bad := range []string{"", "\x00\x01", "a: [unclosed", strings.Repeat("{", 100)} {
		l := yamlpos.New([]byte(bad))
		if got := l.Line("anything"); got != 0 {
			t.Errorf("Line() = %d on malformed input, want 0", got)
		}
	}
}

func FuzzLocator(f *testing.F) {
	f.Add(doc, "services")
	f.Add("", "")
	f.Add("a: &x\nb: *x\n", "a")
	f.Add(strings.Repeat("a:\n ", 200), "a")

	f.Fuzz(func(t *testing.T, content, key string) {
		l := yamlpos.New([]byte(content))
		if got := l.Line(key); got < 0 {
			t.Fatalf("Line() = %d", got)
		}
		if got := l.KeyLine(key); got < 0 {
			t.Fatalf("KeyLine() = %d", got)
		}
	})
}
