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
//	<layer>:<name>
//	<layer>:@<scope>/<name>
//
// A node is identified by what it is called and where it lives. Nothing about
// which file described it, and nothing inferred about what it is.
//
// # What is not in here, and why
//
// The identifier used to carry the extractor that found the node and the kind
// it was taken to be. Both are properties of the reading, not of the
// component, and both split one component into several.
//
// A kind is inferred from an image. docker-compose.yml gave carts-db an image
// of mongo and it became a datastore; docker-compose.logging.yml mentioned the
// same service to attach a log driver, named no image, and got the service
// fallback. One database, two identifiers. Only the layer survives into the
// identifier, because the layer is the part that does not move: both of those
// readings are container level. See LayerOf.
//
// An extractor is worse. A Helm chart and a cluster manifest describing one
// service produced different identifiers by construction, and no later rule
// can join what the grammar has already separated. Two fifths of
// online-boutique's nodes were another node again.
//
// # Scope
//
// Scope is the deployment scope a component lives in: a Kubernetes namespace,
// optionally prefixed with the project when a repository declares more than
// one. A chart name, a Compose project, a Terraform module directory and a
// language are not scopes, and the field that once held all five now holds
// only this one. What the others knew is recorded where it belongs -- the
// language in Tech, the packaging in an attribute, the grouping in a boundary
// node's contains edge.
//
// Most nodes have no scope, so most identifiers are a layer and a name. The
// "@" marks a scope when there is one, and it is required rather than
// decorative: a name may contain slashes, so without a marker
// "container:acme/api" could be the module acme/api or the service api in
// namespace acme. "@" cannot occur in a folded name -- it is not in the
// allowed set, so it folds to a hyphen and a leading hyphen is trimmed --
// which is what makes it unambiguous.
//
// The ID is semantic rather than a content hash so that moving a file does not
// rename every node — which would make each drift diff look like an
// architecture change — and so that IDs stay readable when a model quotes them
// back.
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

const (
	idSeparator = ":"
	// scopeMarker introduces the scope segment. See the grammar note above
	// for why a marker is needed rather than position alone.
	scopeMarker = "@"
)

// idDiscriminatorLen is how much of the hash is kept. Thirty-two bits is far
// more than the handful of folded names in a repository needs, and short
// enough that an ID a model quotes back stays readable.
const idDiscriminatorLen = 8

// ErrInvalidNodeID is returned for a string that is not a well-formed node ID.
var ErrInvalidNodeID = errors.New("invalid node id")

// NewNodeID builds a node ID from a component's identity: the layer its kind
// belongs to, the deployment scope it lives in, and its name. Scope is empty
// for the many components that have none.
//
// The kind is taken rather than the layer so that a node's identifier and its
// Layer field cannot disagree: both come from LayerOf.
func NewNodeID(kind NodeKind, scope, name string) string {
	// Joined by hand rather than with path.Join, which would Clean the
	// result and silently collapse a segment that normalized to "." or "..".
	id := string(LayerOf(kind)) + idSeparator
	if trimmed := strings.TrimSpace(scope); trimmed != "" {
		id += scopeMarker + slugSegment(trimmed) + "/"
	}
	return id + qualify(slugPath(name), name, false)
}

// NewSymbolID builds the ID of a code symbol: a function, an HTTP endpoint, a
// type, a React component. file is the repo-relative path it is declared in;
// symbol is the identifier as the language spells it.
//
// Case is preserved through the discriminator rather than in the ID itself,
// because allowing uppercase would change the ID grammar, and the grammar is
// a major-version promise.
func NewSymbolID(kind NodeKind, scope, file, symbol string) string {
	name := strings.TrimSpace(file)
	if sym := strings.TrimSpace(symbol); sym != "" {
		if name != "" {
			name += "/"
		}
		name += sym
	}
	return NewNodeID(kind, scope, qualify(slugPath(name), name, true))
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
	Layer Layer
	// Scope is the deployment scope, empty when the node has none.
	Scope string
	Name  string
}

// ParseNodeID splits an ID back into its parts.
//
// Resolution does not go through here. A node's identity is read from its
// fields, which is what lets the identifier be chosen for readability; this
// exists for validation and for the few places that have an identifier and no
// node.
func ParseNodeID(id string) (ParsedNodeID, error) {
	layerStr, rest, ok := strings.Cut(id, idSeparator)
	if !ok {
		return ParsedNodeID{}, fmt.Errorf("%w: %q has no layer prefix", ErrInvalidNodeID, id)
	}
	layer := Layer(layerStr)
	if !layer.Valid() {
		return ParsedNodeID{}, fmt.Errorf("%w: %q has unknown layer %q", ErrInvalidNodeID, id, layerStr)
	}

	out := ParsedNodeID{Layer: layer}
	if scoped, found := strings.CutPrefix(rest, scopeMarker); found {
		scope, name, ok := strings.Cut(scoped, "/")
		if !ok {
			return ParsedNodeID{}, fmt.Errorf(
				"%w: %q has a scope marker and no name", ErrInvalidNodeID, id)
		}
		out.Scope, out.Name = scope, name
	} else {
		out.Name = rest
	}
	if out.Name == "" || (strings.HasPrefix(rest, scopeMarker) && out.Scope == "") {
		return ParsedNodeID{}, fmt.Errorf("%w: %q has an empty segment", ErrInvalidNodeID, id)
	}
	return out, nil
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
	// two during merge. Rebuilt through a kind so that the layer is checked
	// to be one a kind can actually produce.
	if got := rebuild(p); got != id {
		return fmt.Errorf("%w: %q is not normalized (want %q)", ErrInvalidNodeID, id, got)
	}
	return nil
}

// rebuild reconstructs an identifier from its parsed parts, which is how
// normalization is checked. The layer is carried through directly rather than
// via a kind, because several kinds map onto one layer and any of them would
// do.
func rebuild(p ParsedNodeID) string {
	id := string(p.Layer) + idSeparator
	if p.Scope != "" {
		id += scopeMarker + slugSegment(p.Scope) + "/"
	}
	return id + qualify(slugPath(p.Name), p.Name, false)
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

// QualifyProject prefixes a node ID's scope with the project it belongs to,
// returning the new ID with the name preserved exactly.
//
// This is how a repository that holds more than one project keeps their
// components apart. Two independent stacks each declaring a "prod" namespace
// produce the same ID for different things, and the graph merges them into
// one node that claims to contain both: a boundary that really does exist
// twice, drawn once, with members from two systems inside it.
//
// A node with no scope gains one, which is the project alone. That is correct
// and is the point: in a repository of several projects, "where it lives" is
// at least which project, even when nothing narrower is known.
//
// The name is copied verbatim rather than rebuilt, because rebuilding would
// fold an already-folded name a second time and could append a second
// discriminator to a name that already carries one.
//
// # A collision this does not resolve
//
// The project and the scope are joined with a hyphen and folded together, and
// a hyphen is an ordinary character in both. So ("web-api", "prod") and
// ("web", "api-prod") produce the same segment, and two projects merge into
// the one boundary this exists to keep apart. TestQualifyProjectCollision
// holds the case.
//
// It is not fixable here. A separator that folding cannot produce would have
// to survive ValidateNodeID, which requires an ID to be unchanged by
// re-normalization -- and folding collapses a run of hyphens, so "--" does
// not survive. Underscore does survive, and therefore can appear in a project
// name, so it collides in the same way. What is left is a discriminator on
// every qualified scope, which would churn the IDs of every multi-project
// repository to fix a case most of them do not have.
//
// The caller fixes it, because the caller can. internal/scan's projectLabels
// holds every project and every scope in the repository at once, folds them
// the same way this function will, and if two projects land on one segment it
// gives every label a digest of its project root before calling here.
// Repositories with no collision keep the readable label, so this stays the
// common path and the identifiers it produces are unchanged.
//
// What that leaves here is a function that is unsafe to call with an
// arbitrary prefix and safe with the one it is given. Said plainly because
// the next caller will not have the first one's global view.
func QualifyProject(id, project string) (string, error) {
	if project == "" {
		return id, nil
	}
	layer, rest, ok := strings.Cut(id, idSeparator)
	if !ok || layer == "" {
		return "", fmt.Errorf("%w: %q has no layer", ErrInvalidNodeID, id)
	}

	scope, name := "", rest
	if scoped, found := strings.CutPrefix(rest, scopeMarker); found {
		scope, name, ok = strings.Cut(scoped, "/")
		if !ok || scope == "" {
			return "", fmt.Errorf("%w: %q has no name after its scope", ErrInvalidNodeID, id)
		}
	}
	if name == "" {
		return "", fmt.Errorf("%w: %q has an empty name", ErrInvalidNodeID, id)
	}

	qualified := project
	if scope != "" {
		qualified = project + "-" + scope
	}
	return layer + idSeparator + scopeMarker + slugSegment(qualified) + "/" + name, nil
}
