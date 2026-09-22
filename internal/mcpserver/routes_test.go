package mcpserver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoRoutes writes a stack where "web" reaches "store" two ways of equal
// length: through "cache", declared with depends_on and therefore certain,
// and through "proxy", which finds it by reading a hostname out of an
// environment variable and is therefore not.
//
// Every route-ordering rule only does any work when two routes are in hand,
// and no fixture had ever produced two.
func twoRoutes(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	compose := `services:
  web:
    image: web:1
    depends_on: [cache, proxy]
  cache:
    image: redis:7
    depends_on: [store]
  proxy:
    image: nginx:1
    environment:
      STORE_URL: http://store:8080
  store:
    image: store:1
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The most direct and best-evidenced route has to come first, because a
// reader stops at the first one and a model quotes it. Two routes of equal
// length are ordered by their weakest edge, which is as much as a chain can
// be trusted.
func TestEqualLengthRoutesAreOrderedByEvidence(t *testing.T) {
	session, ctx := connect(t, twoRoutes(t))

	out, isErr := callText(t, session, ctx, "structura_trace_path",
		map[string]any{"from": "web", "to": "store"})
	if isErr {
		t.Fatalf("trace_path failed:\n%s", out)
	}

	viaCache := strings.Index(out, "cache")
	viaProxy := strings.Index(out, "proxy")
	if viaCache < 0 || viaProxy < 0 {
		t.Fatalf("expected both routes in the output, got:\n%s", out)
	}
	if viaCache > viaProxy {
		t.Errorf("the declared route through cache is reported after the inferred one through proxy:\n%s", out)
	}
}

// The same ordering has to survive being asked twice. Routes are discovered
// by walking maps, so a comparator that leaves two routes equal would let
// iteration order decide what a reader sees.
func TestRouteOrderIsStableAcrossCalls(t *testing.T) {
	root := twoRoutes(t)
	var first string
	for i := range 5 {
		session, ctx := connect(t, root)
		out, isErr := callText(t, session, ctx, "structura_trace_path",
			map[string]any{"from": "web", "to": "store"})
		if isErr {
			t.Fatalf("trace_path failed:\n%s", out)
		}
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("call %d returned a different ordering:\n--- first ---\n%s\n--- now ---\n%s", i+1, first, out)
		}
	}
}

// impact_of reaches a component by walking backwards, and a component
// reachable two ways at one depth has to be reported by its best-evidenced
// route rather than by whichever the walk happened to reach first.
func TestImpactReportsTheBestEvidencedRoute(t *testing.T) {
	root := twoRoutes(t)
	var first string
	for i := range 5 {
		session, ctx := connect(t, root)
		out, isErr := callText(t, session, ctx, "structura_impact_of",
			map[string]any{"node": "store"})
		if isErr {
			t.Fatalf("impact_of failed:\n%s", out)
		}
		if !strings.Contains(out, "web") {
			t.Fatalf("web depends on store through two routes and is not in the blast radius:\n%s", out)
		}
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("call %d returned a different answer:\n--- first ---\n%s\n--- now ---\n%s", i+1, first, out)
		}
	}
}

// twoEquallyGoodRoutes writes a stack where "web" reaches "store" two ways of
// the same length and the same confidence: both hops declared, so neither
// route is better evidenced than the other.
//
// This is the case the final tie-break exists for. With length and evidence
// equal, the only thing left deciding what a reader sees first is the route's
// own name, and without it the answer would come from whatever order the
// search happened to produce.
func twoEquallyGoodRoutes(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	compose := `services:
  web:
    image: web:1
    depends_on: [alpha, beta]
  alpha:
    image: alpha:1
    depends_on: [store]
  beta:
    image: beta:1
    depends_on: [store]
  store:
    image: store:1
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRoutesOfEqualLengthAndEvidenceAreStillOrdered(t *testing.T) {
	root := twoEquallyGoodRoutes(t)

	var first string
	for i := range 5 {
		session, ctx := connect(t, root)
		out, isErr := callText(t, session, ctx, "structura_trace_path",
			map[string]any{"from": "web", "to": "store"})
		if isErr {
			t.Fatalf("trace_path failed:\n%s", out)
		}
		alpha, beta := strings.Index(out, "alpha"), strings.Index(out, "beta")
		if alpha < 0 || beta < 0 {
			t.Fatalf("expected both routes, got:\n%s", out)
		}
		if alpha > beta {
			t.Errorf("routes are not in name order, so the tie is being broken by something unstable:\n%s", out)
		}
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("call %d ordered two equally good routes differently:\n--- first ---\n%s\n--- now ---\n%s",
				i+1, first, out)
		}
	}
}
