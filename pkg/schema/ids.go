package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Node IDs have the form:
//
//	<kind>:<source>/<namespace>/<name>
//
// where kind, source, and namespace contain no slash and name may. The ID is
// semantic rather than a content hash so that moving a file does not rename
// every node — which would make each drift diff look like an architecture
// change — and so that IDs stay readable when a model quotes them back.
//
// Every segment is normalized: lowercased, with anything outside
// [a-z0-9._-] folded to a hyphen. Name preserves slashes so that package
// identifiers such as github.com/spf13/cobra survive intact. The original,
// unnormalized string is kept in Node.Name for display.

const idSeparator = ":"

// ErrInvalidNodeID is returned for a string that is not a well-formed node ID.
var ErrInvalidNodeID = errors.New("invalid node id")

// NewNodeID builds a node ID from its parts. Empty source or namespace are
// replaced with "unknown" and "default" respectively so that the shape stays
// fixed and parseable.
func NewNodeID(kind NodeKind, source, namespace, name string) string {
	// Joined by hand rather than with path.Join, which would Clean the
	// result and silently collapse a segment that normalized to "." or "..".
	return string(kind) + idSeparator +
		slugSegment(orDefault(source, "unknown")) + "/" +
		slugSegment(orDefault(namespace, "default")) + "/" +
		slugPath(name)
}

// ParsedNodeID is the decomposition of a node ID.
type ParsedNodeID struct {
	Kind      NodeKind
	Source    string
	Namespace string
	Name      string
}

// ParseNodeID splits an ID back into its parts.
func ParseNodeID(id string) (ParsedNodeID, error) {
	kindStr, rest, ok := strings.Cut(id, idSeparator)
	if !ok {
		return ParsedNodeID{}, fmt.Errorf("%w: %q has no kind prefix", ErrInvalidNodeID, id)
	}
	kind := NodeKind(kindStr)
	if !kind.Valid() {
		return ParsedNodeID{}, fmt.Errorf("%w: %q has unknown kind %q", ErrInvalidNodeID, id, kindStr)
	}
	// SplitN with 3 keeps any slashes in the name segment.
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return ParsedNodeID{}, fmt.Errorf(
			"%w: %q wants <kind>:<source>/<namespace>/<name>", ErrInvalidNodeID, id)
	}
	for i, p := range parts {
		if p == "" {
			return ParsedNodeID{}, fmt.Errorf("%w: %q has an empty segment %d", ErrInvalidNodeID, id, i)
		}
	}
	return ParsedNodeID{Kind: kind, Source: parts[0], Namespace: parts[1], Name: parts[2]}, nil
}

// ValidateNodeID reports whether id is well-formed and already normalized.
func ValidateNodeID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidNodeID)
	}
	p, err := ParseNodeID(id)
	if err != nil {
		return err
	}
	// An ID that is not already normalized would compare unequal to the ID
	// the constructor produces for the same node, silently splitting it in
	// two during merge.
	if got := NewNodeID(p.Kind, p.Source, p.Namespace, p.Name); got != id {
		return fmt.Errorf("%w: %q is not normalized (want %q)", ErrInvalidNodeID, id, got)
	}
	return nil
}

// NewEdgeID derives a stable, short identifier from the edge's endpoints.
// Two runs that find the same relationship produce the same ID, which is what
// lets a stored graph be diffed against a fresh one.
func NewEdgeID(from, to string, kind EdgeKind, protocol string) string {
	sum := sha256.Sum256([]byte(from + "\x00" + to + "\x00" + string(kind) + "\x00" + protocol))
	return "e:" + hex.EncodeToString(sum[:6])
}

// slugSegment normalizes one slash-free ID segment.
func slugSegment(s string) string {
	out := foldInvalid(s, false)
	if out == "" {
		return "unknown"
	}
	return out
}

// slugPath normalizes a segment that is allowed to contain slashes, by
// normalizing each element independently and dropping empty ones. Doing it
// per element rather than over the whole string is what makes the result
// idempotent: re-running it on its own output is a fixed point, which
// ValidateNodeID relies on to detect unnormalized IDs.
func slugPath(s string) string {
	parts := strings.Split(foldInvalid(s, true), "/")
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.Trim(p, "-."); p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "unknown"
	}
	return strings.Join(kept, "/")
}

// foldInvalid lowercases s and replaces every run of disallowed characters
// with a single hyphen, so that "my  service!!" and "my-service" do not
// become two distinct nodes. Runs of literal hyphens collapse too, which
// matters for inputs like "café-service" where a folded character sits next
// to one that was already a hyphen.
func foldInvalid(s string, allowSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastHyphen = false
		case r == '/' && allowSlash:
			b.WriteRune(r)
			lastHyphen = false
		case r == '-':
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	// Trailing and leading dots are trimmed as well as hyphens: a
	// segment of "." or ".." is path-traversal shaped and must never
	// survive into an identifier.
	return strings.Trim(b.String(), "-.")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// validateRelPath enforces the repo-relative, slash-separated path invariant.
// An empty path is allowed: many diagnostics are not file-scoped.
func validateRelPath(p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path %q must use forward slashes on every platform", p)
	}
	if path.IsAbs(p) || (len(p) > 1 && p[1] == ':') {
		return fmt.Errorf("path %q must be repo-relative, not absolute", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not escape the repository root", p)
		}
	}
	return nil
}
