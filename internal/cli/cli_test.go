package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/buildinfo"
	"github.com/kamronarabi/structura/internal/cli"
)

// run executes the CLI in-process and returns (exitCode, stdout, stderr).
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.Execute(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, out, errOut := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	for _, want := range []string{"structura", buildinfo.Version(), buildinfo.SchemaVersion} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := run(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, out)
	}
	if got["schemaVersion"] != buildinfo.SchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", got["schemaVersion"], buildinfo.SchemaVersion)
	}
	if got["version"] != buildinfo.Version() {
		t.Errorf("version = %q, want %q", got["version"], buildinfo.Version())
	}
}

// Errors must never land on stdout: stdout is reserved for command output and,
// for the mcp command, for protocol frames only.
func TestErrorsGoToStderr(t *testing.T) {
	code, out, errOut := run(t, "definitely-not-a-command")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on error", out)
	}
	if !strings.Contains(errOut, "structura:") {
		t.Errorf("stderr = %q, want a structura-prefixed error", errOut)
	}
}

func TestRootFlagMustExist(t *testing.T) {
	code, _, errOut := run(t, "--root", filepath.Join(t.TempDir(), "nope"), "version")
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for a missing --root")
	}
	if !strings.Contains(errOut, "--root") {
		t.Errorf("stderr = %q, want it to name the offending flag", errOut)
	}
}

func TestRootFlagRejectsFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a-file")
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "--root", f, "version")
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero when --root is a file")
	}
	if !strings.Contains(errOut, "not a directory") {
		t.Errorf("stderr = %q, want a 'not a directory' error", errOut)
	}
}
