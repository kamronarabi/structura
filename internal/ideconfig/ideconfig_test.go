package ideconfig_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/ideconfig"
)

func entry() ideconfig.Entry {
	return ideconfig.Entry{Command: "/usr/local/bin/structura", Args: []string{"mcp", "--root", "/repo"}}
}

func claudeCode(t *testing.T) ideconfig.Client {
	t.Helper()
	c, err := ideconfig.Lookup("claude-code")
	if err != nil {
		t.Fatalf("looking up claude-code: %v", err)
	}
	return c
}

// write puts content at a fresh path and returns it.
func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the fixture config: %v", err)
	}
	return path
}

func TestCreatesAConfigThatDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", ".mcp.json")

	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if !change.Created {
		t.Error("a missing file was not reported as created")
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}

	got := read(t, path)
	if got["mcpServers"] == nil {
		t.Fatalf("no mcpServers key was written:\n%s", raw(t, path))
	}
	server := got["mcpServers"].(map[string]any)["structura"].(map[string]any)
	if server["command"] != "/usr/local/bin/structura" {
		t.Errorf("command = %v", server["command"])
	}
}

func TestPreservesOtherServersExactly(t *testing.T) {
	// This is the behavior with the highest blast radius in the whole
	// program: a user who loses three other MCP servers because we rewrote
	// their config finds out hours later, in a different tool.
	const original = `{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "secret" }
    },
    "aardvark": { "command": "aardvark" }
  },
  "unrelatedSetting": { "keep": true }
}
`
	path := write(t, original)
	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}

	after := raw(t, path)
	for _, want := range []string{
		`"args": ["-y", "@modelcontextprotocol/server-github"],`,
		`"env": { "GITHUB_TOKEN": "secret" }`,
		`"aardvark": { "command": "aardvark" }`,
		`"unrelatedSetting": { "keep": true }`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("a line the user wrote was reformatted or lost:\n  want %s\n\ngot:\n%s", want, after)
		}
	}

	// Order is preserved too: github came before aardvark, alphabetical
	// order would not have.
	if strings.Index(after, `"aardvark"`) < strings.Index(after, `"github"`) {
		t.Errorf("the user's key order was not preserved:\n%s", after)
	}
	if strings.Index(after, `"unrelatedSetting"`) < strings.Index(after, `"mcpServers"`) {
		t.Errorf("top-level key order was not preserved:\n%s", after)
	}
}

func TestOnlyAddedLinesInTheDiff(t *testing.T) {
	// A diff full of reformatting is a diff the user cannot read, and the
	// point of --dry-run is that they can see exactly what we will do.
	path := write(t, `{
  "mcpServers": {
    "github": { "command": "npx", "args": ["-y", "server-github"] }
  }
}
`)
	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	// A line may gain a trailing comma when an entry is appended after it;
	// anything else disappearing means we reformatted the user's file.
	added := map[string]bool{}
	for _, line := range strings.Split(change.Diff(), "\n") {
		if after, ok := strings.CutPrefix(line, "+ "); ok {
			added[strings.TrimSuffix(after, ",")] = true
		}
	}
	for _, line := range strings.Split(change.Diff(), "\n") {
		removed, ok := strings.CutPrefix(line, "- ")
		if !ok {
			continue
		}
		if !added[strings.TrimSuffix(removed, ",")] {
			t.Errorf("the diff loses a line from the user's config: %q\n\nfull diff:\n%s",
				removed, change.Diff())
		}
	}
	if !strings.Contains(change.Diff(), `+     "structura"`) {
		t.Errorf("the diff does not show the entry being added:\n%s", change.Diff())
	}
}

func TestIsIdempotent(t *testing.T) {
	path := write(t, `{"mcpServers": {"github": {"command": "npx"}}}`)
	client := claudeCode(t)

	first, err := ideconfig.Plan(client, path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if _, err := ideconfig.Apply(first); err != nil {
		t.Fatalf("applying: %v", err)
	}
	afterFirst := raw(t, path)

	second, err := ideconfig.Plan(client, path, entry())
	if err != nil {
		t.Fatalf("re-planning: %v", err)
	}
	if !second.AlreadyInstalled {
		t.Error("a second plan did not recognize the entry it had just written")
	}
	if !second.NoOp() {
		t.Errorf("a second install would rewrite the file:\n%s", second.Diff())
	}
	if _, err := ideconfig.Apply(second); err != nil {
		t.Fatalf("applying the no-op: %v", err)
	}
	if got := raw(t, path); got != afterFirst {
		t.Errorf("a second install changed the file:\nfirst:\n%s\nsecond:\n%s", afterFirst, got)
	}
}

func TestApplyOfANoOpWritesNoBackup(t *testing.T) {
	// A backup written on every run eventually overwrites the one copy of
	// the config the user actually wanted back.
	path := write(t, `{"mcpServers": {}}`)
	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}

	second, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("re-planning: %v", err)
	}
	if err := os.Remove(path + ideconfig.BackupSuffix); err != nil {
		t.Fatalf("removing the first backup: %v", err)
	}
	backup, err := ideconfig.Apply(second)
	if err != nil {
		t.Fatalf("applying the no-op: %v", err)
	}
	if backup != "" {
		t.Errorf("a no-op wrote a backup at %s", backup)
	}
	if _, err := os.Stat(path + ideconfig.BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("a no-op re-created the backup file")
	}
}

func TestBacksUpTheOriginalBeforeWriting(t *testing.T) {
	const original = `{"mcpServers": {"github": {"command": "npx"}}}`
	path := write(t, original)

	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	backup, err := ideconfig.Apply(change)
	if err != nil {
		t.Fatalf("applying: %v", err)
	}
	if backup != path+ideconfig.BackupSuffix {
		t.Fatalf("backup = %q, want %q", backup, path+ideconfig.BackupSuffix)
	}
	data, err := os.ReadFile(backup) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	if string(data) != original {
		t.Errorf("the backup is not the original:\ngot:  %s\nwant: %s", data, original)
	}
}

func TestReplacesAStaleEntryAndSaysSo(t *testing.T) {
	path := write(t, `{
  "mcpServers": {
    "structura": { "command": "/old/path/structura", "args": ["mcp"] }
  }
}
`)
	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if !change.Replaced {
		t.Error("an out-of-date entry was not reported as replaced")
	}
	if change.AlreadyInstalled {
		t.Error("an out-of-date entry was reported as already installed")
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}
	if after := raw(t, path); strings.Contains(after, "/old/path/structura") {
		t.Errorf("the stale command survived:\n%s", after)
	}
}

func TestRefusesAConfigThatIsNotJSON(t *testing.T) {
	// Refusing is the whole point. A file we cannot parse is a file whose
	// contents we cannot preserve, and JSON with comments — which VS Code
	// accepts and encoding/json does not — is the common case.
	path := write(t, "{\n  // the MCP servers\n  \"mcpServers\": {}\n}\n")

	_, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err == nil {
		t.Fatal("a file with comments was accepted")
	}
	var malformed *ideconfig.ErrMalformed
	if !errors.As(err, &malformed) {
		t.Fatalf("got %T, want *ideconfig.ErrMalformed: %v", err, err)
	}
	for _, want := range []string{path, "comment", "will not overwrite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}

	// And the file is untouched.
	if !strings.Contains(raw(t, path), "// the MCP servers") {
		t.Error("the refused file was modified")
	}
}

func TestRefusesAServersKeyThatIsNotAnObject(t *testing.T) {
	path := write(t, `{"mcpServers": ["github"]}`)

	_, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err == nil {
		t.Fatal(`an array-valued "mcpServers" was accepted`)
	}
	if !strings.Contains(err.Error(), "mcpServers") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}

func TestEmptyFileIsTreatedAsEmptyConfig(t *testing.T) {
	// An editor that creates the file before writing to it is common enough
	// that refusing here would look like a bug in us.
	path := write(t, "\n  \n")

	change, err := ideconfig.Plan(claudeCode(t), path, entry())
	if err != nil {
		t.Fatalf("an empty file was refused: %v", err)
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}
	if read(t, path)["mcpServers"] == nil {
		t.Errorf("nothing was written:\n%s", raw(t, path))
	}
}

func TestVSCodeUsesItsOwnShape(t *testing.T) {
	// VS Code calls the object "servers" and wants an explicit type. Writing
	// the Claude Code shape produces a config it silently ignores.
	client, err := ideconfig.Lookup("vscode")
	if err != nil {
		t.Fatalf("looking up vscode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")

	change, err := ideconfig.Plan(client, path, entry())
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if _, err := ideconfig.Apply(change); err != nil {
		t.Fatalf("applying: %v", err)
	}

	got := read(t, path)
	if got["mcpServers"] != nil {
		t.Error(`the VS Code config was written with the "mcpServers" key`)
	}
	servers, ok := got["servers"].(map[string]any)
	if !ok {
		t.Fatalf(`no "servers" object:\n%s`, raw(t, path))
	}
	if servers["structura"].(map[string]any)["type"] != "stdio" {
		t.Errorf("the entry does not declare its transport:\n%s", raw(t, path))
	}
}

func TestOutputIsValidJSON(t *testing.T) {
	// Values are copied through verbatim rather than re-marshaled, so the
	// assembled file has to be checked rather than assumed.
	for name, original := range map[string]string{
		"compact":       `{"mcpServers":{"a":{"command":"a"},"b":{"command":"b"}},"x":1}`,
		"indented":      "{\n  \"mcpServers\": {\n    \"a\": {\n      \"command\": \"a\"\n    }\n  }\n}\n",
		"empty object":  `{}`,
		"no servers":    `{"other": {"a": [1, 2, 3]}}`,
		"empty servers": `{"mcpServers": {}}`,
		"nested arrays": `{"mcpServers": {"a": {"command": "a", "args": [["x"], {"y": [null, true]}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := write(t, original)
			change, err := ideconfig.Plan(claudeCode(t), path, entry())
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			if _, err := ideconfig.Apply(change); err != nil {
				t.Fatalf("applying: %v", err)
			}

			var got map[string]any
			data := raw(t, path)
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatalf("the written config is not valid JSON: %v\n%s", err, data)
			}
			servers, ok := got["mcpServers"].(map[string]any)
			if !ok || servers["structura"] == nil {
				t.Fatalf("structura was not registered:\n%s", data)
			}

			// Everything the original had is still there.
			var before map[string]any
			if err := json.Unmarshal([]byte(original), &before); err != nil {
				t.Fatalf("fixture is not valid JSON: %v", err)
			}
			for k := range before {
				if got[k] == nil {
					t.Errorf("top-level key %q was dropped:\n%s", k, data)
				}
			}
		})
	}
}

func TestScopeErrorsNameTheAlternative(t *testing.T) {
	windsurf, err := ideconfig.Lookup("windsurf")
	if err != nil {
		t.Fatalf("looking up windsurf: %v", err)
	}
	if _, err := windsurf.ConfigPath(ideconfig.ScopeProject, "/repo", "/home"); err == nil {
		t.Error("windsurf accepted a project scope it does not support")
	} else if !strings.Contains(err.Error(), "--global") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

func TestUnknownClientListsTheKnownOnes(t *testing.T) {
	_, err := ideconfig.Lookup("emacs")
	if err == nil {
		t.Fatal("an unknown client was accepted")
	}
	for _, name := range ideconfig.ClientNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not mention %q: %v", name, err)
		}
	}
}

func TestEveryClientHasAScope(t *testing.T) {
	// "/repo" and "/home" are absolute on Unix but rooted-relative on
	// Windows, where an absolute path needs a volume name. TempDir is
	// absolute on every platform, so the IsAbs assertion below tests the
	// client's path building rather than the literals used to drive it.
	repo, home := filepath.Join(t.TempDir(), "repo"), filepath.Join(t.TempDir(), "home")
	for _, c := range ideconfig.Clients() {
		if len(c.Scopes()) == 0 {
			t.Errorf("%s has no usable scope", c.Name)
		}
		for _, scope := range c.Scopes() {
			path, err := c.ConfigPath(scope, repo, home)
			if err != nil {
				t.Errorf("%s %s: %v", c.Name, scope, err)
			}
			if !filepath.IsAbs(path) {
				t.Errorf("%s %s resolved to a relative path %q", c.Name, scope, path)
			}
		}
	}
}

func read(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw(t, path)), &out); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return out
}

func raw(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
