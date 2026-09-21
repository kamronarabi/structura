package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the schema version this package implements. It follows SemVer
// independently of the CLI version: a CLI bugfix must not imply a schema
// change, and the cloud tier has to ingest graphs produced by many CLI
// versions at once.
//
// # What a version change means
//
// A minor bump is additive, and only additive:
//
//   - a new optional field on an existing type
//   - a new value for NodeKind, Layer, EdgeKind, or Severity
//   - a new key in Attrs, a new diagnostic code, a new confidence constant
//
// A reader built for an earlier minor can still parse a later one: fields it
// does not know are ignored, and enum values it does not know arrive as
// plain strings rather than as a parse error. It understands less than the
// writer did, but nothing it does understand is wrong.
//
// A major bump is everything else:
//
//   - removing or renaming a JSON field
//   - changing a field's type or cardinality
//   - changing the node ID grammar
//   - changing what an existing kind, layer, or edge kind means
//   - making an optional field required
//
// A patch bump is documentation and wording only. Two graphs differing just
// in patch are structurally identical, which is why CompareVersion treats
// them as the same: a clarification must not invalidate every cached graph in
// the world.
//
// # The pre-1.0 promise
//
// SemVer permits 0.x minors to break. Structura does not take that
// permission. The rules above hold at 0.x exactly as they would at 1.x,
// because graph.json is committed to repositories and read by builds that are
// not the one that wrote it -- the situation SemVer exists to govern. If a
// genuine break becomes necessary before the format stabilizes, it goes to
// 1.0.0 rather than hiding in a 0.x minor.
//
// # Phase 2
//
// Phase 2 adds component-layer nodes, call edges between them, and finer
// source locations. That is a minor bump, not a break: LayerComponent and
// EdgeCalls are already defined and already validate, Attrs is an open map,
// and Source already carries a path and a line.
//
// Fields are not reserved speculatively. An unused field in the published
// JSON Schema is a promise about a design that does not exist yet, and
// withdrawing it later is precisely the major break this policy is meant to
// avoid. Phase 2 adds what Phase 2 turns out to need, and bumps the minor
// when it does -- to whatever the next minor is by then, which is why no
// number is written down here.
const Version = "0.2.0"

// Relation describes how one schema version stands to another.
type Relation string

const (
	// RelationSame means the two versions are structurally identical. Equal
	// versions, or versions differing only in patch.
	RelationSame Relation = "same"
	// RelationOlder means the stored graph predates the reader within the
	// same major: readable in full, but lacking whatever the reader's
	// version added since.
	RelationOlder Relation = "older"
	// RelationNewer means the stored graph comes from a later minor of the
	// same major. It parses, but it may carry fields and enum values the
	// reader has no meaning for, so the reader is seeing less than is there.
	RelationNewer Relation = "newer"
	// RelationIncompatible means a different major, or a version string that
	// is not a version at all. Nothing may be assumed about the contents.
	RelationIncompatible Relation = "incompatible"
)

// Compat is the result of comparing a stored graph's schema version against
// the one a build implements.
type Compat struct {
	Stored   string
	Current  string
	Relation Relation
}

// CompareVersion reports how a stored graph's schema version relates to the
// version this build implements.
func CompareVersion(stored string) Compat {
	c := Compat{Stored: stored, Current: Version, Relation: RelationIncompatible}

	storedMajor, storedMinor, ok := parseVersion(stored)
	if !ok {
		return c
	}
	currentMajor, currentMinor, ok := parseVersion(Version)
	if !ok {
		// Unreachable: Version is a constant, and a test parses it.
		return c
	}

	switch {
	case storedMajor != currentMajor:
		c.Relation = RelationIncompatible
	case storedMinor == currentMinor:
		c.Relation = RelationSame
	case storedMinor < currentMinor:
		c.Relation = RelationOlder
	default:
		c.Relation = RelationNewer
	}
	return c
}

// Readable reports whether a build implementing Current can parse a graph
// declaring Stored without misreading what it does understand.
//
// A newer graph is readable: the additive rule guarantees that every field
// this build knows still means what it has always meant. What this build
// cannot do is claim to have seen all of it -- see Lossy.
func (c Compat) Readable() bool { return c.Relation != RelationIncompatible }

// Lossy reports whether parsing a graph declaring Stored may drop data.
//
// This is the case that matters for writing. A build that re-serializes a
// graph it parsed lossily silently deletes whatever it did not understand,
// and since graph.json is committed, that deletion lands in someone's history
// looking like an architecture change.
func (c Compat) Lossy() bool { return c.Relation == RelationNewer }

// Reason states the relation as a sentence, for a log line or a staleness
// explanation.
func (c Compat) Reason() string {
	switch c.Relation {
	case RelationSame:
		return fmt.Sprintf("the stored graph is schema %s, which is what this build writes", c.Current)
	case RelationOlder:
		return fmt.Sprintf("the stored graph is schema %s and this build writes %s", c.Stored, c.Current)
	case RelationNewer:
		return fmt.Sprintf("the stored graph is schema %s, written by a build newer than this one, which speaks %s",
			c.Stored, c.Current)
	default:
		return fmt.Sprintf("the stored graph declares schema %q, which this build (%s) cannot read",
			c.Stored, c.Current)
	}
}

// parseVersion reads the major and minor of a SemVer string. The patch is
// parsed for validation but not returned: no comparison this package makes
// depends on it, because a patch bump changes no structure.
//
// A pre-release or build suffix is accepted and ignored. Hand-rolled rather
// than taken from golang.org/x/mod so that this package, which is the
// project's public API, keeps its dependency set empty.
func parseVersion(v string) (major, minor int, ok bool) {
	v, _, _ = strings.Cut(v, "+")
	v, _, _ = strings.Cut(v, "-")

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		// Rejected explicitly, because ParseInt accepts a leading sign and
		// "0.1.+0" is not a version.
		if p == "" || strings.ContainsAny(p, "+-") {
			return 0, 0, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], true
}
