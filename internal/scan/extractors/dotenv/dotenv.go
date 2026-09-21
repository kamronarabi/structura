// Package dotenv extracts references from .env files.
//
// For a container-orchestrated repository the architecture is in the
// orchestrator: a Compose file or a Deployment says what runs and what it
// talks to. A great many repositories have no orchestrator at all. A React or
// Next.js application deployed to a managed platform, talking to a managed
// database, declares its entire outward topology in a .env file and nowhere
// else — and without this extractor such a repository produces one node and
// no edges, which is a worse answer than none because it looks complete.
//
// Two things make this narrower than it sounds.
//
// A .env file is not a component. It carries references on behalf of the
// service whose directory it sits in, so everything here is emitted as a
// resolve.Hint with OwnerDir set, and the resolver binds it to that service
// once the full node set exists. A .env file that owns nothing is reported
// rather than guessed at.
//
// The file this extractor usually gets is .env.example, because a real .env
// is gitignored and the walker honours .gitignore. Example files are written
// to be filled in, so their values are frequently placeholders, and a
// placeholder that reaches the resolver becomes a fabricated third-party
// system on the diagram. Rejecting them is most of the work below.
package dotenv

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "dotenv"

// Extractor reads .env files.
type Extractor struct{}

// New returns a dotenv extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// skipNames are dotfiles that begin with ".env" but are not KEY=VALUE files.
// .envrc is a direnv shell script; .env.vault holds a dotenv-vault ciphertext,
// whose decrypted contents this scan has no business reconstructing.
var skipNames = map[string]bool{
	".envrc":     true,
	".env.vault": true,
	".env.keys":  true,
}

// Match implements scan.Extractor.
func (*Extractor) Match(f scan.FileMeta) bool {
	name := strings.ToLower(f.Name)
	if skipNames[name] {
		return false
	}
	// ".env", ".env.local", ".env.production.local", and also "env.example",
	// which some projects commit without the leading dot so that it is not
	// hidden.
	return name == ".env" || strings.HasPrefix(name, ".env.") ||
		name == "env.example" || name == "env.sample" || name == "env.template"
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	owner := f.Dir
	if owner == "" {
		owner = "."
	}
	example := isExampleFile(f.Name)

	var rejected []string
	for _, entry := range parse(f.Content) {
		ref, ok := resolve.ParseValue(entry.key, entry.value)
		if !ok {
			continue
		}
		// A reference that parsed is the interesting case for placeholder
		// rejection: it is about to become an edge, or an invented external
		// system, on the strength of a value nobody ever filled in.
		if placeholder(entry.value, example) {
			rejected = append(rejected, entry.key)
			continue
		}

		// Redacted here as well as at serialization, because --debug-dump
		// prints hints and a .env file is mostly credentials.
		redacted, _ := resolve.RedactValue(entry.key, entry.value)
		emit.Hint(resolve.Hint{
			OwnerDir:      owner,
			Kind:          ref.Kind,
			Raw:           redacted,
			Tokens:        ref.Tokens,
			Port:          ref.Port,
			Protocol:      ref.Protocol,
			SuggestedEdge: ref.Edge,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: entry.line,
				Rule:   "dotenv_reference",
				Detail: fmt.Sprintf("env %s=%s", entry.key, redacted),
			},
		})
	}

	// Every rejected value is a dependency this component really has, which
	// the graph is about to omit. Reporting only files where nothing resolved
	// would be the more comfortable rule and the wrong one: a template that
	// happens to carry one real endpoint alongside five placeholders is
	// exactly where a reader is most likely to mistake the one for the whole
	// picture.
	if len(rejected) > 0 {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "env_placeholder_reference",
			Path:     f.Path,
			Message:  placeholderMessage(rejected),
		})
	}
	return nil
}

func placeholderMessage(keys []string) string {
	if len(keys) == 1 {
		return fmt.Sprintf(
			"%s names a location but is an unfilled placeholder, so no edge was drawn for it; "+
				"its real value is set outside the repository, and whatever it points at is "+
				"missing from the graph",
			keys[0])
	}
	return fmt.Sprintf(
		"%s name locations but are unfilled placeholders, so no edges were drawn for them; "+
			"their real values are set outside the repository, and whatever they point at is "+
			"missing from the graph",
		strings.Join(keys, ", "))
}

// isExampleFile reports whether a file is a template meant to be copied,
// which is the case where a placeholder value is the norm rather than a
// mistake.
func isExampleFile(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"example", "sample", "template", "dist", "defaults"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// entry is one assignment in a .env file.
type entry struct {
	key   string
	value string
	line  int
}

// parse reads dotenv syntax: KEY=VALUE, one per line, with optional "export",
// optional quoting, and "#" comments.
//
// This is deliberately not a full dotenv implementation. Variable expansion,
// multi-line values, and escape sequences are all things a .env file may
// contain, and none of them can produce a hostname that a simple reading
// would miss. Anything not understood is skipped, never guessed at.
func parse(content []byte) []entry {
	var out []entry

	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	for i, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !validKey(key) {
			continue
		}
		value, ok := unquote(strings.TrimSpace(line[eq+1:]))
		if !ok || value == "" {
			continue
		}
		out = append(out, entry{key: key, value: value, line: i + 1})
	}
	return out
}

// validKey rejects anything that is not a shell-style identifier, which keeps
// YAML, JSON, and prose that happens to contain "=" out of the results.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquote strips one layer of matching quotes, or removes a trailing comment
// from a bare value. It reports false for a value whose quoting does not
// close on this line, which is the multi-line case this parser does not read.
func unquote(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	switch q := value[0]; q {
	case '\'', '"':
		if len(value) < 2 || value[len(value)-1] != q {
			return "", false
		}
		return value[1 : len(value)-1], true
	}
	// An unquoted value ends at the first whitespace-preceded "#". A "#"
	// with no space before it is part of the value: a URL fragment, or a
	// character in a password.
	if i := strings.Index(value, " #"); i >= 0 {
		value = value[:i]
	}
	if i := strings.Index(value, "\t#"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value), true
}

// placeholderTokens are the substrings that mark a value as unfilled. They
// are matched against the whole value, lowercased.
//
// The list is short on purpose. Every entry costs a real reference that
// happens to contain the word, and the cost of missing a placeholder is one
// plausible-looking node, while the cost of an over-broad rule is a silently
// dropped dependency.
// Every entry has to be a string that no real hostname would contain.
// "todo" is not on the list for that reason: a service genuinely called
// "todos" is more likely than a placeholder spelled that way, and dropping a
// real dependency is the failure this file exists to avoid in the first place.
var placeholderTokens = []string{
	"your-", "your_", "yourproject", "yourdomain", "youraccount", "yourcompany",
	"my-project", "myproject", "changeme", "change-me", "change_me",
	"replaceme", "replace-me", "replace_me", "placeholder",
	"xxxxx", "project-ref", "project_ref", "projectref",
}

// reservedDomains can never name a real system: RFC 2606 and RFC 6761 set
// them aside precisely so that documentation and examples cannot collide with
// the public DNS.
var reservedDomains = []string{
	"example.com", "example.org", "example.net", "example.edu",
	".invalid", ".test", ".localdomain",
}

// placeholder reports whether a value was left to be filled in.
//
// A value that is clearly a template is rejected everywhere. A reserved
// documentation domain is rejected only in an example file: elsewhere in
// Structura an example.com hostname written into an Ingress rule is treated
// as a real external system, and this extractor is not the place to overturn
// that. In a file whose whole purpose is to be copied, it is a placeholder.
func placeholder(value string, example bool) bool {
	lower := strings.ToLower(value)

	// Angle brackets and unexpanded braces are never part of a hostname.
	if strings.ContainsAny(lower, "<>{}") {
		return true
	}
	for _, token := range placeholderTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	if example {
		for _, domain := range reservedDomains {
			if strings.Contains(lower, domain) {
				return true
			}
		}
	}
	return false
}

// OwnerDirOf normalizes a directory to the form the resolver indexes nodes
// under, which is what the manifest extractor writes into its "directory"
// attribute.
func OwnerDirOf(dir string) string {
	if dir == "" {
		return "."
	}
	return path.Clean(dir)
}
