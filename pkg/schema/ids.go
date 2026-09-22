package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
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
//
// # Folding is lossy, so it carries a discriminator
//
// Folding maps many inputs onto one output, and two components that fold
// together become one node with no sign that anything was merged. Every
// non-Latin identifier collapsed to the same ID -- a service named in
// Japanese, Chinese, or Russian all became "unknown" -- and "vttablet-{{uid}}"
// was indistinguishable from a literal "vttablet-uid".
//
// So when folding the name actually loses information, the ID carries eight
// hex characters derived from the original. Case is not counted as a loss
// here: an infrastructure name is discovered by several extractors that may
// spell it differently, and folding "Postgres" and "postgres" onto one node
// is the merge this package exists to perform. Across seven real
// repositories, one name in 374 needs a discriminator.
//
// The source and namespace segments cannot hold a slash, since those are the
// ID's own separators, so a namespace of "examples/complete" folds to
// "examples-complete" and would collide with a literal one. That is left
// alone deliberately: it affects 18 of 191 real namespaces, prevents no
// collision anyone has, and the noise would land on every Terraform node.
//
// # Code symbols
//
// A Phase 2 child -- a function, an HTTP endpoint, a type, a React component
// -- is identified by where its source is, not by what deploys it:
//
//	<kind>:<language>/<namespace>/<repo-relative path>/<symbol>
//
// Source location rather than deployment, because code has one home and a
// deployment does not. The same directory may be deployed as dev, staging,
// and prod, and identifying a function by its deployment would give it three
// identities and put it inside three containers at once. Containment has to
// stay a tree to be navigable, so a symbol is contained by the node that owns
// its file -- which is either the codebase node or, where the resolver joined
// them, the deployment it merged into.
//
// Case is significant for a symbol and is not for anything else. Go's
// HandleOrder and handleOrder are two functions and routinely both exist,
// one wrapping the other; folding them together would silently halve a
// component view. NewSymbolID therefore treats any difference from the folded
// form, case included, as a loss worth a discriminator. The readable spelling
// stays in Node.Name, which is what every tool prints.

const idSeparator = ":"

// idDiscriminatorLen is how much of the hash is kept. Thirty-two bits is far
// more than the handful of folded names in a repository needs, and short
// enough that an ID a model quotes back stays readable.
const idDiscriminatorLen = 8

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
		qualify(slugPath(name), name, false)
}

// NewSymbolID builds the ID of a code symbol: a function, an HTTP endpoint, a
// type, a React component. file is the repo-relative path it is declared in;
// symbol is the identifier as the language spells it.
//
// Case is preserved through the discriminator rather than in the ID itself,
// because allowing uppercase would change the ID grammar, and the grammar is
// a major-version promise.
func NewSymbolID(kind NodeKind, source, namespace, file, symbol string) string {
	name := strings.TrimSpace(file)
	if sym := strings.TrimSpace(symbol); sym != "" {
		if name != "" {
			name += "/"
		}
		name += sym
	}
	return NewNodeID(kind, source, namespace, qualify(slugPath(name), name, true))
}

// qualify appends a discriminator when folding destroyed what told two inputs
// apart. Applied to an already-qualified name it is a no-op, which is what
// lets ValidateNodeID check an ID by rebuilding it.
func qualify(slug, original string, strict bool) string {
	trimmed := strings.TrimSpace(original)
	if trimmed == "" || !lossy(trimmed, slug, strict) {
		return slug
	}
	return slug + "." + discriminator(trimmed)
}

// lossy reports whether folding destroyed identity rather than tidying
// punctuation.
//
// The two callers need different lines, because they fold different things.
//
// An infrastructure name is discovered by several extractors that spell it
// differently, and reconciling those spellings is the merge this package
// exists to perform: "API-Gateway", "api-gateway", and "  -api-gateway- "
// are one component, and giving them three IDs would be the bug, not the
// fix. Punctuation folded to a hyphen still stands for the separator that was
// there, and a reader sees the same name. What is not recoverable is a rune
// the fold cannot represent at all: a service named in Japanese, Chinese, or
// Russian had every character replaced, and all three landed on "unknown".
// That is the case worth a discriminator, and across seven real repositories
// it does not arise once -- which is the point. It costs nothing until it is
// the difference between one node and three.
//
// A code symbol has no such spelling variance. One extractor reads it from
// one file with the exact characters the language requires, so every
// difference is a real difference: Repository[T] is not Repository-T, and
// GET /orders/{id} is not the literal route /orders/id.
func lossy(trimmed, slug string, strict bool) bool {
	if strict {
		return trimmed != slug
	}
	for _, r := range trimmed {
		if r > unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

func discriminator(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:idDiscriminatorLen]
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

// QualifyNamespace prefixes a node ID's namespace segment, returning the new
// ID with every other segment preserved exactly.
//
// This is how a repository that holds more than one project keeps their
// components apart. Two independent stacks each declaring a "prod" namespace
// produce the same ID for different things, and the graph merges them into
// one node that claims to contain both: a boundary that really does exist
// twice, drawn once, with members from two systems inside it. Prefixing the
// namespace with the project is enough to separate them, and it leaves the ID
// grammar alone -- the shape is unchanged, only the value differs, so a
// reader built for any schema version still parses it.
//
// The name segment is copied verbatim rather than rebuilt, because rebuilding
// would fold an already-folded name a second time and could append a second
// discriminator to a name that already carries one.
//
// # A collision this does not resolve
//
// The prefix and the namespace are joined with a hyphen and folded together,
// and a hyphen is an ordinary character in both. So ("web-api", "prod") and
// ("web", "api-prod") produce the same segment, and two projects merge into
// the one boundary this exists to keep apart. TestQualifyNamespaceCollision
// holds the case.
//
// It is not fixable here. A separator that folding cannot produce would have
// to survive ValidateNodeID, which requires an ID to be unchanged by
// re-normalization -- and folding collapses a run of hyphens, so "--" does
// not survive. Underscore does survive, and therefore can appear in a project
// name, so it collides in the same way. What is left is a discriminator on
// every qualified namespace, which would churn the IDs of every multi-project
// repository to fix a case most of them do not have.
//
// The caller can fix it and this function cannot: qualifyByProject sees every
// project at once, so it can detect two pairs folding to one segment and
// disambiguate only those. Recorded here because the decision belongs with
// whoever owns that pass.
func QualifyNamespace(id, prefix string) (string, error) {
	if prefix == "" {
		return id, nil
	}
	colon := strings.Index(id, idSeparator)
	if colon <= 0 {
		return "", fmt.Errorf("%w: %q has no kind", ErrInvalidNodeID, id)
	}
	kind, rest := id[:colon], id[colon+1:]

	source, rest, ok := strings.Cut(rest, "/")
	if !ok {
		return "", fmt.Errorf("%w: %q has no namespace", ErrInvalidNodeID, id)
	}
	namespace, name, ok := strings.Cut(rest, "/")
	if !ok {
		return "", fmt.Errorf("%w: %q has no name", ErrInvalidNodeID, id)
	}
	if namespace == "" || name == "" {
		return "", fmt.Errorf("%w: %q has an empty segment", ErrInvalidNodeID, id)
	}

	return kind + idSeparator + source + "/" +
		slugSegment(prefix+"-"+namespace) + "/" + name, nil
}
