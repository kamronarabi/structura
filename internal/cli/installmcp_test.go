package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/ideconfig"
)

// repoWithBinary returns a temporary repository and a file standing in for
// the structura binary, since install-mcp refuses to record a path that does
// not exist.
func repoWithBinary(t *testing.T) (root, binary string) {
	t.Helper()
	root = t.TempDir()
	binary = filepath.Join(t.TempDir(), "structura")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test stand-in
		t.Fatalf("writing the stand-in binary: %v", err)
	}
	return root, binary
}

func TestInstallMCPWritesAProjectConfig(t *testing.T) {
	root, binary := repoWithBinary(t)

	code, out, errOut := run(t, "install-mcp", "--client", "claude-code", "-C", root, "--binary", binary)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}

	path := filepath.Join(root, ".mcp.json")
	if !strings.Contains(out, path) {
		t.Errorf("the output does not say what was written:\n%s", out)
	}
	if !strings.Contains(out, "Restart Claude Code") {
		t.Errorf("the output does not say what to do next:\n%s", out)
	}

	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("the config is not valid JSON: %v\n%s", err, data)
	}
	server, ok := config.MCPServers["structura"]
	if !ok {
		t.Fatalf("structura was not registered:\n%s", data)
	}

	// Both paths must be absolute: the editor launches this process from a
	// working directory we do not control and cannot predict.
	if !filepath.IsAbs(server.Command) {
		t.Errorf("command %q is not absolute", server.Command)
	}
	if len(server.Args) != 3 || server.Args[0] != "mcp" || server.Args[1] != "--root" {
		t.Fatalf("args = %v, want [mcp --root <abs>]", server.Args)
	}
	if !filepath.IsAbs(server.Args[2]) {
		t.Errorf("--root %q is not absolute", server.Args[2])
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		wantRoot = root
	}
	gotRoot, err := filepath.EvalSymlinks(server.Args[2])
	if err != nil {
		gotRoot = server.Args[2]
	}
	if gotRoot != wantRoot {
		t.Errorf("--root = %q, want %q", gotRoot, wantRoot)
	}
}

func TestInstallMCPDryRunWritesNothing(t *testing.T) {
	root, binary := repoWithBinary(t)

	code, out, errOut := run(t, "install-mcp", "--client", "claude-code", "-C", root,
		"--binary", binary, "--dry-run")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "Nothing was written") {
		t.Errorf("the dry run does not say it changed nothing:\n%s", out)
	}
	if !strings.Contains(out, `"structura"`) {
		t.Errorf("the dry run does not show what it would add:\n%s", out)
	}

	if _, err := os.Stat(filepath.Join(root, ".mcp.json")); !os.IsNotExist(err) {
		t.Error("--dry-run created the config file")
	}
}

func TestInstallMCPDryRunOnAnExistingConfigShowsTheDiff(t *testing.T) {
	root, binary := repoWithBinary(t)
	path := filepath.Join(root, ".mcp.json")
	const original = `{
  "mcpServers": {
    "github": { "command": "npx" }
  }
}
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	code, out, errOut := run(t, "install-mcp", "--client", "claude-code", "-C", root,
		"--binary", binary, "--dry-run")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, ideconfig.BackupSuffix) {
		t.Errorf("the dry run does not mention the backup it would make:\n%s", out)
	}

	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	if string(data) != original {
		t.Errorf("--dry-run modified the config:\n%s", data)
	}
}

func TestInstallMCPIsIdempotent(t *testing.T) {
	root, binary := repoWithBinary(t)
	path := filepath.Join(root, ".mcp.json")
	args := []string{"install-mcp", "--client", "claude-code", "-C", root, "--binary", binary}

	if code, _, errOut := run(t, args...); code != 0 {
		t.Fatalf("first install: exit %d (stderr: %s)", code, errOut)
	}
	first, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	code, out, errOut := run(t, args...)
	if code != 0 {
		t.Fatalf("second install: exit %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "already configured") {
		t.Errorf("a repeat install does not say it was a no-op:\n%s", out)
	}
	second, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("re-reading the config: %v", err)
	}
	if string(second) != string(first) {
		t.Errorf("a repeat install changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestInstallMCPRefusesMalformedJSON(t *testing.T) {
	root, binary := repoWithBinary(t)
	path := filepath.Join(root, ".mcp.json")
	const original = "{\n  // a comment VS Code would accept\n  \"mcpServers\": {}\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	code, _, errOut := run(t, "install-mcp", "--client", "claude-code", "-C", root, "--binary", binary)
	if code == 0 {
		t.Fatal("a malformed config was accepted")
	}
	if !strings.Contains(errOut, "not valid JSON") {
		t.Errorf("the error does not say what is wrong:\n%s", errOut)
	}

	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	if string(data) != original {
		t.Errorf("the refused config was modified:\n%s", data)
	}
	if _, err := os.Stat(path + ideconfig.BackupSuffix); !os.IsNotExist(err) {
		t.Error("a refused install left a backup behind")
	}
}

func TestInstallMCPRequiresAClient(t *testing.T) {
	root, _ := repoWithBinary(t)

	code, _, errOut := run(t, "install-mcp", "-C", root)
	if code == 0 {
		t.Fatal("install-mcp ran without --client")
	}
	for _, name := range ideconfig.ClientNames() {
		if !strings.Contains(errOut, name) {
			t.Errorf("the error does not offer %q:\n%s", name, errOut)
		}
	}
}

func TestInstallMCPRejectsAnUnknownClient(t *testing.T) {
	root, binary := repoWithBinary(t)

	code, _, errOut := run(t, "install-mcp", "--client", "emacs", "-C", root, "--binary", binary)
	if code == 0 {
		t.Fatal("an unknown client was accepted")
	}
	if !strings.Contains(errOut, "claude-code") {
		t.Errorf("the error does not list the supported clients:\n%s", errOut)
	}
}

func TestInstallMCPRejectsAnUnsupportedScope(t *testing.T) {
	root, binary := repoWithBinary(t)

	code, _, errOut := run(t, "install-mcp", "--client", "windsurf", "--scope", "project",
		"-C", root, "--binary", binary)
	if code == 0 {
		t.Fatal("windsurf accepted a project scope it does not have")
	}
	if !strings.Contains(errOut, "global") {
		t.Errorf("the error does not name the scope that would work:\n%s", errOut)
	}
}

func TestInstallMCPRejectsAMissingBinary(t *testing.T) {
	// Recording a path that does not exist produces a config the editor
	// fails on later, with an error that points at the editor rather than
	// at us.
	root := t.TempDir()

	code, _, errOut := run(t, "install-mcp", "--client", "claude-code", "-C", root,
		"--binary", filepath.Join(root, "nope"))
	if code == 0 {
		t.Fatal("a nonexistent binary was accepted")
	}
	if !strings.Contains(errOut, "--binary") {
		t.Errorf("the error does not say how to fix it:\n%s", errOut)
	}
}

func TestInstallMCPListShowsEveryClient(t *testing.T) {
	code, out, errOut := run(t, "install-mcp", "--list")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	for _, name := range ideconfig.ClientNames() {
		if !strings.Contains(out, name) {
			t.Errorf("--list omits %q:\n%s", name, out)
		}
	}
}

func TestInstallMCPVSCodeUsesItsOwnShape(t *testing.T) {
	root, binary := repoWithBinary(t)

	code, _, errOut := run(t, "install-mcp", "--client", "vscode", "-C", root, "--binary", binary)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}

	data, err := os.ReadFile(filepath.Join(root, ".vscode", "mcp.json")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("reading the VS Code config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("the config is not valid JSON: %v\n%s", err, data)
	}
	if config["servers"] == nil {
		t.Errorf(`the VS Code config has no "servers" object:\n%s`, data)
	}
}
