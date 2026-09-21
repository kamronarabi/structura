package schema_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// The compatibility rule is a promise made to builds that are not this one:
// graph.json is committed, so a graph written today will be read by a binary
// nobody has compiled yet. These cases are that promise, written down.
func TestCompareVersionFollowsTheDocumentedPolicy(t *testing.T) {
	// Derived from Version rather than written out, so that bumping the
	// schema does not turn every case here into a false failure -- and so
	// that what is being asserted stays the relation rather than a literal.
	major, minor := majorMinor(t, schema.Version)

	cases := []struct {
		name   string
		stored string
		want   schema.Relation
	}{
		{"the version this build writes", schema.Version, schema.RelationSame},
		{"a patch bump changes no structure", ver(major, minor, 99), schema.RelationSame},
		{"an earlier minor lacks what we added", ver(major, minor-1, 7), schema.RelationOlder},
		{"a later minor holds more than we know", ver(major, minor+1, 0), schema.RelationNewer},
		{"a later major may mean anything", ver(major+1, 0, 0), schema.RelationIncompatible},
		{"a pre-release suffix is ignored", schema.Version + "-rc.1", schema.RelationSame},
		{"a build suffix is ignored", schema.Version + "+20260921", schema.RelationSame},
		{"an unversioned graph is not trusted", "", schema.RelationIncompatible},
		{"nor is a partial version", "0.1", schema.RelationIncompatible},
		{"nor a word", "dev", schema.RelationIncompatible},
		{"nor a signed component", "0.+1.0", schema.RelationIncompatible},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := schema.CompareVersion(tc.stored)
			if got.Relation != tc.want {
				t.Fatalf("CompareVersion(%q) = %s, want %s", tc.stored, got.Relation, tc.want)
			}
			if got.Stored != tc.stored || got.Current != schema.Version {
				t.Errorf("CompareVersion(%q) reported stored=%q current=%q",
					tc.stored, got.Stored, got.Current)
			}
			if got.Reason() == "" {
				t.Errorf("CompareVersion(%q) gave no reason; callers log it", tc.stored)
			}
		})
	}
}

// Version itself has to parse, or CompareVersion would call every graph in
// existence incompatible. The "unreachable" branch in CompareVersion rests on
// this.
func TestVersionIsAParseableSemVer(t *testing.T) {
	if got := schema.CompareVersion(schema.Version); got.Relation != schema.RelationSame {
		t.Fatalf("Version %q does not compare equal to itself: %s", schema.Version, got.Relation)
	}
}

// Readable and Lossy are separate questions, and conflating them is the bug
// this is here to prevent: a newer graph can be served but must not be
// written back, because re-serializing it would delete what this build could
// not represent.
func TestReadableAndLossyAreDifferentQuestions(t *testing.T) {
	major, minor := majorMinor(t, schema.Version)
	cases := []struct {
		stored          string
		readable, lossy bool
	}{
		{schema.Version, true, false},
		{ver(major, minor-1, 1), true, false},
		{ver(major, minor+9, 0), true, true},
		{ver(major+2, 0, 0), false, false},
		{"", false, false},
	}

	for _, tc := range cases {
		c := schema.CompareVersion(tc.stored)
		if c.Readable() != tc.readable {
			t.Errorf("CompareVersion(%q).Readable() = %v, want %v", tc.stored, c.Readable(), tc.readable)
		}
		if c.Lossy() != tc.lossy {
			t.Errorf("CompareVersion(%q).Lossy() = %v, want %v", tc.stored, c.Lossy(), tc.lossy)
		}
	}
}

// A reason that does not name both versions is useless in a log line, where
// the reader has neither in front of them.
func TestReasonNamesBothVersions(t *testing.T) {
	major, minor := majorMinor(t, schema.Version)
	newer := ver(major, minor+1, 0)
	c := schema.CompareVersion(newer)
	if !strings.Contains(c.Reason(), newer) || !strings.Contains(c.Reason(), schema.Version) {
		t.Fatalf("reason %q does not name both versions", c.Reason())
	}
}

// majorMinor splits Version so the cases above can be written as relations to
// it rather than as literals that go stale on the next bump.
func majorMinor(t *testing.T, v string) (major, minor int) {
	t.Helper()
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		t.Fatalf("Version %q is not X.Y.Z", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("Version %q has a non-numeric major: %v", v, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("Version %q has a non-numeric minor: %v", v, err)
	}
	// A minor of zero has no earlier minor to compare against, which would
	// make the "older" case untestable without saying so.
	if minor == 0 {
		t.Fatalf("Version %q has minor 0; the older-minor case cannot be built from it", v)
	}
	return major, minor
}

func ver(major, minor, patch int) string {
	return fmt.Sprintf("%d.%d.%d", major, minor, patch)
}

// The compatibility policy promises that a graph written by an earlier minor
// still reads. Nothing tested that against real bytes: the golden graph is
// regenerated on every bump, so it only ever proves the current version
// round-trips with itself.
//
// This fixture is frozen at 0.1.0 and must never be regenerated. It is the
// only thing standing between the promise and a field quietly acquiring a
// meaning that an older graph does not carry.
func TestAGraphFromAnEarlierMinorStillReads(t *testing.T) {
	const fixture = "testdata/compat/graph-0.1.0.json"

	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading %s: %v", fixture, err)
	}

	g, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("a 0.1.0 graph no longer parses, which is a major break: %v", err)
	}
	if g.SchemaVersion != "0.1.0" {
		t.Fatalf("the fixture declares %q; it must stay frozen at 0.1.0", g.SchemaVersion)
	}
	if c := schema.CompareVersion(g.SchemaVersion); c.Relation != schema.RelationOlder {
		t.Fatalf("a 0.1.0 graph compares as %s to %s, want older", c.Relation, schema.Version)
	}

	// Parsing is not the promise -- the promise is that what this build
	// understands still means what it meant. Spot-check the load-bearing
	// parts rather than only that the bytes went in.
	if len(g.Nodes) == 0 || len(g.Edges) == 0 {
		t.Fatalf("the fixture lost its contents: %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}
	for _, n := range g.Nodes {
		if err := n.Validate(); err != nil {
			t.Errorf("node from a 0.1.0 graph no longer validates: %v", err)
		}
	}
	for _, e := range g.Edges {
		if err := e.Validate(); err != nil {
			t.Errorf("edge from a 0.1.0 graph no longer validates: %v", err)
		}
	}
	// A field added since is absent, not wrong, which is what "additive"
	// buys and the only reason an old graph is usable at all.
	if g.Generator != nil {
		t.Errorf("a 0.1.0 graph reports a producer it could not have recorded: %+v", g.Generator)
	}
}
