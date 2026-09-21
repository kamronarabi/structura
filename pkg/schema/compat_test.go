package schema_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/pkg/schema"
)

// The compatibility rule is a promise made to builds that are not this one:
// graph.json is committed, so a graph written today will be read by a binary
// nobody has compiled yet. These cases are that promise, written down.
func TestCompareVersionFollowsTheDocumentedPolicy(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		want   schema.Relation
	}{
		{"the version this build writes", schema.Version, schema.RelationSame},
		{"a patch bump changes no structure", "0.1.99", schema.RelationSame},
		{"an earlier minor lacks what we added", "0.0.7", schema.RelationOlder},
		{"a later minor holds more than we know", "0.2.0", schema.RelationNewer},
		{"a later major may mean anything", "1.0.0", schema.RelationIncompatible},
		{"a pre-release suffix is ignored", "0.1.0-rc.1", schema.RelationSame},
		{"a build suffix is ignored", "0.1.0+20260921", schema.RelationSame},
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
	cases := []struct {
		stored          string
		readable, lossy bool
	}{
		{schema.Version, true, false},
		{"0.0.1", true, false},
		{"0.9.0", true, true},
		{"2.0.0", false, false},
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
	c := schema.CompareVersion("0.2.0")
	if !strings.Contains(c.Reason(), "0.2.0") || !strings.Contains(c.Reason(), schema.Version) {
		t.Fatalf("reason %q does not name both versions", c.Reason())
	}
}
