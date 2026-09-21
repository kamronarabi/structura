package dotenv_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/dotenv"
	"github.com/kamronarabi/structura/pkg/schema"
)

type capture struct {
	nodes   []schema.Node
	edges   []schema.Edge
	hints   []resolve.Hint
	aliases []resolve.Alias
	diags   []schema.Diagnostic
}

func (c *capture) Node(n schema.Node)       { c.nodes = append(c.nodes, n) }
func (c *capture) Edge(e schema.Edge)       { c.edges = append(c.edges, e) }
func (c *capture) Hint(h resolve.Hint)      { c.hints = append(c.hints, h) }
func (c *capture) Alias(a resolve.Alias)    { c.aliases = append(c.aliases, a) }
func (c *capture) Diag(d schema.Diagnostic) { c.diags = append(c.diags, d) }

func (c *capture) tokens() []string {
	var out []string
	for _, h := range c.hints {
		if len(h.Tokens) > 0 {
			out = append(out, h.Tokens[0])
		}
	}
	return out
}

func (c *capture) codes() []string {
	var out []string
	for _, d := range c.diags {
		out = append(out, d.Code)
	}
	return out
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{Path: path, Dir: dir, Name: name, Size: int64(len(content))},
		Content:  []byte(content),
	}
	if err := dotenv.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatch(t *testing.T) {
	e := dotenv.New()
	yes := []string{
		".env", ".env.local", ".env.production", ".env.production.local",
		".env.example", ".env.sample", ".ENV", "env.example",
	}
	for _, name := range yes {
		if !e.Match(scan.FileMeta{Name: name}) {
			t.Errorf("Match(%q) = false, want true", name)
		}
	}
	// .envrc is a direnv shell script and .env.vault is a ciphertext; neither
	// is a KEY=VALUE file, and reading them as one produces nonsense.
	no := []string{".envrc", ".env.vault", ".env.keys", "environment.yaml", "README.md", ".environment"}
	for _, name := range no {
		if e.Match(scan.FileMeta{Name: name}) {
			t.Errorf("Match(%q) = true, want false", name)
		}
	}
}

// A .env file is not a component, so it must never emit a node, and its hints
// have to name the directory whose service they belong to.
func TestEmitsHintsOwnedByItsDirectory(t *testing.T) {
	c := extract(t, "services/api/.env", "DATABASE_URL=postgres://app:secret@db.abcxyz.supabase.co:5432/postgres\n")

	if len(c.nodes) != 0 {
		t.Fatalf("emitted %d nodes, want 0: a .env file is not a component", len(c.nodes))
	}
	if len(c.hints) != 1 {
		t.Fatalf("hints = %d, want 1", len(c.hints))
	}
	h := c.hints[0]
	if h.OwnerDir != "services/api" {
		t.Errorf("OwnerDir = %q, want %q", h.OwnerDir, "services/api")
	}
	if h.FromNode != "" {
		t.Errorf("FromNode = %q, want empty: the extractor cannot know the owner", h.FromNode)
	}
	if h.SuggestedEdge != schema.EdgePersistsTo {
		t.Errorf("SuggestedEdge = %q, want %q", h.SuggestedEdge, schema.EdgePersistsTo)
	}
	if h.Protocol != "postgres" || h.Port != 5432 {
		t.Errorf("protocol/port = %q/%d, want postgres/5432", h.Protocol, h.Port)
	}
	if h.Tokens[0] != "db.abcxyz.supabase.co" {
		t.Errorf("Tokens[0] = %q, want db.abcxyz.supabase.co", h.Tokens[0])
	}
	if h.Source.Line != 1 {
		t.Errorf("Source.Line = %d, want 1", h.Source.Line)
	}
}

// A file at the repository root has no directory, and the manifest extractor
// records that same place as ".".
func TestRootFileOwnedByDot(t *testing.T) {
	c := extract(t, ".env", "VITE_API_URL=https://api.acme.io\n")
	if len(c.hints) != 1 {
		t.Fatalf("hints = %d, want 1", len(c.hints))
	}
	if got := c.hints[0].OwnerDir; got != "." {
		t.Errorf("OwnerDir = %q, want %q", got, ".")
	}
}

// The credentials a .env file is full of must not reach a hint, either as a
// value of their own or inside a connection string.
func TestSecretsAreNotEmittedAndNotLeaked(t *testing.T) {
	c := extract(t, ".env", strings.Join([]string{
		"SUPABASE_ANON_KEY=eyJhbGciOiJIUzI1NiJ9.notarealtoken",
		"STRIPE_SECRET_KEY=sk_live_51NotARealKey",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI",
		"DATABASE_URL=postgres://admin:sup3rs3cret@db.internal:5432/app",
	}, "\n")+"\n")

	if len(c.hints) != 1 {
		t.Fatalf("hints = %d, want 1: only DATABASE_URL names a location", len(c.hints))
	}
	for _, h := range c.hints {
		for _, secret := range []string{"sup3rs3cret", "notarealtoken", "sk_live_51NotARealKey", "wJalrXUtnFEMI"} {
			if strings.Contains(h.Raw, secret) {
				t.Errorf("hint Raw leaked %q: %s", secret, h.Raw)
			}
			if strings.Contains(h.Source.Detail, secret) {
				t.Errorf("evidence detail leaked %q: %s", secret, h.Source.Detail)
			}
		}
	}
}

// Keys that do not name a location must not be matched against component
// names, or a value like "production" becomes an edge.
func TestNonReferenceKeysIgnored(t *testing.T) {
	c := extract(t, ".env", strings.Join([]string{
		"NODE_ENV=production",
		"LOG_LEVEL=debug",
		"PORT=3000",
		"APP_NAME=storefront",
		"NEXT_PUBLIC_SITE_TITLE=Acme",
	}, "\n")+"\n")

	if len(c.hints) != 0 {
		t.Fatalf("hints = %v, want none", c.tokens())
	}
}

func TestParsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"quoted double", `API_URL="https://a.acme.io"`, []string{"a.acme.io"}},
		{"quoted single", `API_URL='https://b.acme.io'`, []string{"b.acme.io"}},
		{"export prefix", `export API_URL=https://c.acme.io`, []string{"c.acme.io"}},
		{"comment line", "# API_URL=https://d.acme.io\nAPI_URL=https://e.acme.io", []string{"e.acme.io"}},
		{"trailing comment", "API_URL=https://f.acme.io # the api", []string{"f.acme.io"}},
		{"surrounding space", "  API_URL = https://g.acme.io  ", []string{"g.acme.io"}},
		{"crlf", "API_URL=https://h.acme.io\r\nDB_URL=postgres://u@i.acme.io:5432/d\r\n", []string{"h.acme.io", "i.acme.io"}},
		{"blank and empty", "\n\nAPI_URL=\nDB_URL=postgres://u@j.acme.io:5432/d\n", []string{"j.acme.io"}},
		{"unclosed quote", "API_URL=\"https://k.acme.io\nDB_URL=postgres://u@l.acme.io:5432/d", []string{"l.acme.io"}},
		{"no equals", "JUST_A_LINE\nAPI_URL=https://m.acme.io", []string{"m.acme.io"}},
		{"invalid key", "1BAD=https://n.acme.io\nsome text = https://o.acme.io\nAPI_URL=https://p.acme.io", []string{"p.acme.io"}},
		{"localhost rejected", "API_URL=http://localhost:3000\nDB_URL=postgres://u@127.0.0.1:5432/d", nil},
		{"unexpanded interpolation", "API_URL=https://${HOST}/api", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := extract(t, ".env", tt.content)
			got := c.tokens()
			if len(got) != len(tt.want) {
				t.Fatalf("tokens = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("tokens[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// An unfilled placeholder that reaches the resolver becomes an invented
// third-party system on the diagram, which is the failure this guards.
func TestPlaceholdersRejected(t *testing.T) {
	rejected := []string{
		"SUPABASE_URL=https://your-project.supabase.co",
		"SUPABASE_URL=https://<project-ref>.supabase.co",
		"SUPABASE_URL=https://YOUR_PROJECT.supabase.co",
		"API_URL=https://changeme.acme.io",
		"API_URL=https://placeholder.acme.io",
		"DATABASE_URL=postgres://user:pass@project-ref.pooler.supabase.com:5432/postgres",
		"API_URL=https://api.replace-me.io",
	}
	for _, line := range rejected {
		c := extract(t, ".env.example", line)
		if len(c.hints) != 0 {
			t.Errorf("%s produced %v, want no hints", line, c.tokens())
		}
	}

	kept := []string{
		"SUPABASE_URL=https://abcxyzdefghij.supabase.co",
		"API_URL=https://api.acme.io",
		"DATABASE_URL=postgres://u:p@db.internal:5432/app",
	}
	for _, line := range kept {
		c := extract(t, ".env.example", line)
		if len(c.hints) != 1 {
			t.Errorf("%s produced %v, want one hint", line, c.tokens())
		}
	}
}

// RFC 2606 reserved a handful of domains so that examples cannot collide with
// real systems. In a template file they are placeholders; in a real .env they
// are treated the way the rest of Structura treats them.
func TestReservedDomainsOnlyRejectedInTemplates(t *testing.T) {
	const line = "API_URL=https://api.example.com"

	if c := extract(t, ".env.example", line); len(c.hints) != 0 {
		t.Errorf(".env.example: got %v, want no hints", c.tokens())
	}
	if c := extract(t, ".env", line); len(c.hints) != 1 {
		t.Errorf(".env: got %v, want one hint", c.tokens())
	}
}

// A template whose every reference is unfilled is the common case for a
// committed repository, and silence would read as "no dependencies found".
func TestPlaceholderOnlyFileIsReported(t *testing.T) {
	c := extract(t, ".env.example",
		"NEXT_PUBLIC_SUPABASE_URL=https://your-project.supabase.co\n"+
			"DATABASE_URL=postgres://postgres:your-password@db.your-project.supabase.co:5432/postgres\n")

	if len(c.hints) != 0 {
		t.Fatalf("hints = %v, want none", c.tokens())
	}
	if got := c.codes(); len(got) != 1 || got[0] != "env_placeholder_reference" {
		t.Fatalf("diagnostics = %v, want [env_placeholder_reference]", got)
	}
	if c.diags[0].Path != ".env.example" {
		t.Errorf("diagnostic Path = %q, want .env.example", c.diags[0].Path)
	}
}

// A file where one reference resolved and another did not is the case most
// likely to mislead: the graph shows a dependency, so it looks complete, and
// the missing one is invisible. It has to be reported too.
func TestMixedFileIsAlsoReported(t *testing.T) {
	c := extract(t, ".env.example",
		"NEXT_PUBLIC_SUPABASE_URL=https://your-project.supabase.co\n"+
			"REDIS_URL=redis://cache.internal:6379\n")

	if len(c.hints) != 1 {
		t.Fatalf("hints = %v, want one", c.tokens())
	}
	if got := c.codes(); len(got) != 1 || got[0] != "env_placeholder_reference" {
		t.Fatalf("diagnostics = %v, want [env_placeholder_reference]", got)
	}
	// The key is named, so a reader can see which dependency is missing
	// rather than only that one is.
	if !strings.Contains(c.diags[0].Message, "NEXT_PUBLIC_SUPABASE_URL") {
		t.Errorf("message does not name the key: %s", c.diags[0].Message)
	}
	if strings.Contains(c.diags[0].Message, "REDIS_URL") {
		t.Errorf("message names a key that resolved: %s", c.diags[0].Message)
	}
}
