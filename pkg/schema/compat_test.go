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
		{"a later minor holds more than we know", ver(major, minor+1, 0), schema.RelationNewer},
		{"a later major may mean anything", ver(major+1, 0, 0), schema.RelationIncompatible},
		{"an earlier major may mean anything too", ver(major-1, 9, 9), schema.RelationIncompatible},
		{"a pre-release suffix is ignored", schema.Version + "-rc.1", schema.RelationSame},
		{"a build suffix is ignored", schema.Version + "+20260921", schema.RelationSame},
		{"an unversioned graph is not trusted", "", schema.RelationIncompatible},
		{"nor is a partial version", "0.1", schema.RelationIncompatible},
		{"nor a word", "dev", schema.RelationIncompatible},
		{"nor a signed component", "0.+1.0", schema.RelationIncompatible},
	}

	// At minor 0 there is no earlier minor of this major to compare against,
	// so the relation is asserted one major back instead, where a minor
	// certainly existed.
	if minor > 0 {
		cases = append(cases, struct {
			name   string
			stored string
			want   schema.Relation
		}{"an earlier minor lacks what we added", ver(major, minor-1, 7), schema.RelationOlder})
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
		{ver(major, minor+9, 0), true, true},
		{ver(major+2, 0, 0), false, false},
		{ver(major-1, 9, 9), false, false},
		{"", false, false},
	}
	if minor > 0 {
		cases = append(cases, struct {
			stored          string
			readable, lossy bool
		}{ver(major, minor-1, 1), true, false})
	} else {
		t.Logf("schema %s is a fresh major, so there is no earlier minor to read", schema.Version)
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
	// A minor of zero has no earlier minor of the same major to compare
	// against. That is not an error -- it is where every major lands the day
	// it is cut -- but it does mean the "older minor" relation cannot be
	// built from Version alone, so each caller that wants it guards for this
	// and says so rather than quietly asserting nothing.
	return major, minor
}

func ver(major, minor, patch int) string {
	return fmt.Sprintf("%d.%d.%d", major, minor, patch)
}

// The compatibility policy promises that a graph written by an earlier minor
// still reads, and that a graph from an earlier major is refused rather than
// misread. Nothing tested either against real bytes: the golden graph is
// regenerated on every bump, so it only ever proves the current version
// round-trips with itself.
//
// These fixtures are frozen and must never be regenerated.

// 0.1.0 is now an earlier major, because 1.0.0 changed the node ID grammar.
// What the policy promises for that case is not readability -- it is that the
// break is detected. A graph whose identifiers mean something else must not be
// quietly treated as current, because every node in it would resolve to
// nothing and the result would look like an architecture that had lost its
// edges.
func TestAGraphFromAnEarlierMajorIsRefusedNotMisread(t *testing.T) {
	const fixture = "testdata/compat/graph-0.1.0.json"

	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading %s: %v", fixture, err)
	}

	// Parsing still has to work. A reader that panics or errors on old bytes
	// cannot tell the user what is wrong with them.
	g, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("a 0.1.0 graph no longer parses, so nothing can report why: %v", err)
	}
	if g.SchemaVersion != "0.1.0" {
		t.Fatalf("the fixture declares %q; it must stay frozen at 0.1.0", g.SchemaVersion)
	}
	if len(g.Nodes) == 0 || len(g.Edges) == 0 {
		t.Fatalf("the fixture lost its contents: %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}

	c := schema.CompareVersion(g.SchemaVersion)
	if c.Relation != schema.RelationIncompatible {
		t.Errorf("a 0.1.0 graph compares as %s to %s, want incompatible: the ID grammar changed",
			c.Relation, schema.Version)
	}
	if c.Readable() {
		t.Error("a graph from an earlier major reports itself readable")
	}
	if !strings.Contains(c.Reason(), "0.1.0") || !strings.Contains(c.Reason(), schema.Version) {
		t.Errorf("reason %q does not name both versions", c.Reason())
	}

	// The identifiers really are the break: they no longer validate. Asserted
	// so that this test says why the versions are incompatible rather than
	// only that they are.
	var rejected int
	for _, n := range g.Nodes {
		if err := schema.ValidateNodeID(n.ID); err != nil {
			rejected++
		}
	}
	if rejected != len(g.Nodes) {
		t.Errorf("%d of %d identifiers from a 0.1.0 graph still validate; if the grammar is "+
			"compatible after all, the major bump was not needed",
			len(g.Nodes)-rejected, len(g.Nodes))
	}
}

// The fixture the next minor will need. Frozen at 1.0.0, it is what proves the
// additive promise the first time something is added: today it only asserts
// that a current graph reads, which is worth having anyway because it is real
// committed bytes rather than a round trip of freshly generated ones.
func TestAGraphFromThisMajorReads(t *testing.T) {
	const fixture = "testdata/compat/graph-1.0.0.json"

	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading %s: %v", fixture, err)
	}
	g, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("a 1.0.0 graph does not parse: %v", err)
	}
	if g.SchemaVersion != "1.0.0" {
		t.Fatalf("the fixture declares %q; it must stay frozen at 1.0.0", g.SchemaVersion)
	}
	if c := schema.CompareVersion(g.SchemaVersion); !c.Readable() {
		t.Fatalf("a 1.0.0 graph compares as %s to %s", c.Relation, schema.Version)
	}
	if len(g.Nodes) == 0 || len(g.Edges) == 0 {
		t.Fatalf("the fixture lost its contents: %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}
	for _, n := range g.Nodes {
		if err := n.Validate(); err != nil {
			t.Errorf("node from a 1.0.0 graph does not validate: %v", err)
		}
	}
	for _, e := range g.Edges {
		if err := e.Validate(); err != nil {
			t.Errorf("edge from a 1.0.0 graph does not validate: %v", err)
		}
	}
}
