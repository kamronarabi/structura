package mcpserver_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildBinary compiles the CLI once per test binary.
//
// The in-process tests exercise the tool logic; these exercise the thing the
// user's editor actually launches. The two can diverge — a stray write to
// stdout from a dependency, a flag parsed before the transport is wired — and
// the failure mode is an opaque parse error in the editor with nothing
// pointing back here.
var buildBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "structura-protocol-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "structura")
	if runtime.GOOS == "windows" {
		// Windows will not execute a file without a recognised extension,
		// and go build writes structura.exe regardless of what -o asks for.
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./../../cmd/structura")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &buildError{err: err, out: string(out)}
	}
	return bin, nil
})

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

func binaryPath(t *testing.T) string {
	t.Helper()
	bin, err := buildBinary()
	if err != nil {
		t.Fatalf("building the CLI: %v", err)
	}
	return bin
}

// TestProtocolHandshakeOverStdio drives the compiled binary the way an editor
// does: as a subprocess speaking newline-delimited JSON-RPC over pipes.
func TestProtocolHandshakeOverStdio(t *testing.T) {
	root := fixture(t, "k8s-microservices")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binaryPath(t), "mcp", "--root", root)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "protocol-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to the subprocess: %v\nstderr:\n%s", err, stderr.String())
	}
	defer func() { _ = session.Close() }()

	init := session.InitializeResult()
	if init.ServerInfo == nil || init.ServerInfo.Name != "structura" {
		t.Fatalf("unexpected server info: %+v", init.ServerInfo)
	}
	if init.Instructions == "" {
		t.Error("the server sent no instructions")
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("listing tools over stdio: %v\nstderr:\n%s", err, stderr.String())
	}
	if len(tools.Tools) != 5 {
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("got %d tools over stdio (%s), want 5", len(tools.Tools), strings.Join(names, ", "))
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "structura_overview"})
	if err != nil {
		t.Fatalf("calling structura_overview over stdio: %v\nstderr:\n%s", err, stderr.String())
	}
	if res.IsError {
		t.Fatalf("overview failed over stdio: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("overview returned no content over stdio")
	}

	// The scan happens in the subprocess; its log belongs on stderr, and its
	// presence there confirms the process really did the work rather than
	// serving an empty graph.
	if !strings.Contains(stderr.String(), "listening on stdio") {
		t.Errorf("the startup log did not reach stderr:\n%s", stderr.String())
	}
}

// TestStdoutPurity is the invariant the whole transport rests on: the only
// bytes on stdout are protocol frames.
//
// A single stray fmt.Println anywhere in the dependency tree — ours or a
// library's — corrupts the stream, and the user sees an unexplained failure
// in their editor. A lint rule blocks the obvious cases; this catches the
// ones that come from somewhere we do not control.
func TestStdoutPurity(t *testing.T) {
	root := fixture(t, "polyglot-monorepo")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binaryPath(t), "mcp", "--root", root, "--log-level", "debug")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the server: %v", err)
	}

	// Debug logging is on, a scan will run, and several tools are exercised:
	// if anything in the process is going to write to stdout, this is when.
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"purity","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"structura_overview","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"structura_list_nodes","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"structura_diagnostics","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"structura://graph"}}`,
	}
	for _, frame := range frames {
		if _, err := io.WriteString(stdin, frame+"\n"); err != nil {
			t.Fatalf("writing a frame: %v\nstderr:\n%s", err, stderr.String())
		}
	}

	// Wait for the last reply before closing stdin: EOF tears the connection
	// down and cancels anything still in flight.
	waitForReply(t, stdout, 6, 60*time.Second)
	_ = stdin.Close()
	_ = cmd.Wait()

	out := stdout.String()
	assertPureStdout(t, out, stderr.String())

	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	ids := map[float64]bool{}
	for scanner.Scan() {
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			continue // assertPureStdout already failed the test
		}
		if id, ok := frame["id"].(float64); ok {
			ids[id] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading stdout: %v", err)
	}
	for id := 1; id <= 6; id++ {
		if !ids[float64(id)] {
			t.Errorf("no response for request id %d", id)
		}
	}
}

// TestStdoutPurityOnAMalformedRepo covers the path where the scan has bad news.
//
// Error paths are the likeliest source of an accidental stdout write: they
// are the ones a library reaches for a print statement on, and the ones least
// likely to be exercised by hand. A repository full of files that look like
// manifests and are not forces all of them.
func TestStdoutPurityOnAMalformedRepo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := t.TempDir()
	for name, content := range map[string]string{
		"docker-compose.yml": "services:\n  api:\n    image: [unclosed\n",
		"deploy.yaml":        "apiVersion: apps/v1\nkind: Deployment\nmetadata: {{ .Values.name }}\n",
		"main.tf":            `resource "aws_s3_bucket" {`,
		"package.json":       `{"name": "broken",`,
		"go.mod":             "module\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	cmd := exec.CommandContext(ctx, binaryPath(t), "mcp", "--root", root, "--log-level", "debug")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the server: %v", err)
	}

	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"purity","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"structura_overview","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"structura_diagnostics","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"structura_describe_node","arguments":{"node":"nothing-by-this-name"}}}`,
	} {
		if _, err := io.WriteString(stdin, frame+"\n"); err != nil {
			t.Fatalf("writing a frame: %v\nstderr:\n%s", err, stderr.String())
		}
	}
	waitForReply(t, stdout, 4, 60*time.Second)
	_ = stdin.Close()
	_ = cmd.Wait()

	assertPureStdout(t, stdout.String(), stderr.String())
}

// TestStdoutEmptyOnInvalidRoot checks the one path that never reaches the
// server at all.
//
// A usage error is exactly where a CLI framework wants to print to stdout,
// and the editor launching this process is already reading that stream.
func TestStdoutEmptyOnInvalidRoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	cmd := exec.CommandContext(ctx, binaryPath(t), "mcp", "--root", missing)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	if err == nil {
		t.Error("a nonexistent root was accepted")
	}
	if got := stdout.String(); got != "" {
		t.Errorf("a failed startup wrote to stdout:\n%s", truncate(got))
	}
	if !strings.Contains(stderr.String(), "does-not-exist") {
		t.Errorf("the error does not name the root that was rejected:\n%s", stderr.String())
	}
}

// assertPureStdout checks that every line is a JSON-RPC frame, and that the
// logs went somewhere else rather than not happening at all.
func assertPureStdout(t *testing.T, stdout, stderr string) {
	t.Helper()

	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		lines++
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("stdout line %d is not a JSON-RPC frame: %v\n%s", lines, err, truncate(line))
		}
		if frame["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line %d is JSON but not JSON-RPC: %s", lines, truncate(line))
		}
	}
	if lines == 0 {
		t.Fatalf("the server wrote nothing to stdout\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "structura mcp listening on stdio") {
		t.Errorf("startup log missing from stderr; purity that comes from silence is not purity:\n%s",
			stderr)
	}
}

// syncBuffer is a bytes.Buffer that tolerates the copying goroutine os/exec
// starts for a non-*os.File Stdout writing while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.buf.Bytes(), []byte(s))
}

// waitForReply blocks until a response carrying id appears on stdout.
func waitForReply(t *testing.T, buf *syncBuffer, id int, timeout time.Duration) {
	t.Helper()

	needle := `"id":` + itoa(id)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if buf.contains(needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no response for id %d within %s; stdout so far:\n%s", id, timeout, truncate(buf.String()))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func truncate(s string) string {
	const limit = 500
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "... (truncated)"
}
