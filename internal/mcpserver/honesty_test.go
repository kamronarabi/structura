package mcpserver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the difference between what a tool says and what is true.
//
// Every case here shipped as a sentence that was defensible word by word and
// wrong as a whole: "no diagnostics" for a repository three quarters of which
// was never opened, "from 2 files" for a repository of five. A person skims
// past that. A model, which is the reader these tools are written for, cannot
// check it against anything and carries it forward as fact.

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A scan that read two of five files must not present two as the whole.
func TestOverviewReportsFilesItDidNotRead(t *testing.T) {
	root := repoWith(t, map[string]string{
		"package.json":   `{"name":"app","dependencies":{"react":"^18.0.0"}}`,
		"Dockerfile":     "FROM node:22-alpine\n",
		"src/index.ts":   "export const x = 1\n",
		"src/app.tsx":    "export default () => null\n",
		"README.md":      "# app\n",
		"docker-compose": "not a compose file\n",
	})
	s, ctx := connect(t, root)

	out, _ := callText(t, s, ctx, "structura_overview", map[string]any{})
	if !strings.Contains(out, " of ") {
		t.Errorf("overview does not say how many files went unread:\n%s", out)
	}
}

// An empty gap list is not a clean bill of health, and the wording has to
// carry that or silence is read as completeness.
func TestEmptyDiagnosticsDoNotClaimCompleteness(t *testing.T) {
	root := repoWith(t, map[string]string{
		"docker-compose.yml": "services:\n  web:\n    image: nginx:1.27\n",
	})
	s, ctx := connect(t, root)

	out, _ := callText(t, s, ctx, "structura_diagnostics", map[string]any{})
	if !strings.Contains(out, "not the same as nothing being missing") {
		t.Errorf("an empty diagnostic list reads as completeness:\n%s", out)
	}
	if !strings.Contains(out, "application code") {
		t.Errorf("the empty message does not state what the scan never reads:\n%s", out)
	}
}

// A file whose name says it describes architecture has to be reported when
// nothing read it.
func TestUnreadInfrastructureFileSurfacesInDiagnostics(t *testing.T) {
	root := repoWith(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"Dockerfile":   "FROM node:22-alpine\nEXPOSE 3000\n",
		"fly.toml":     "app = \"demo\"\n",
	})
	s, ctx := connect(t, root)

	out, _ := callText(t, s, ctx, "structura_diagnostics", map[string]any{})
	for _, want := range []string{"Dockerfile", "Fly.io"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics do not mention the unread %s:\n%s", want, out)
		}
	}

	// And the overview has to point at them rather than leaving the reader to
	// guess that the tool exists.
	overview, _ := callText(t, s, ctx, "structura_overview", map[string]any{})
	if !strings.Contains(overview, "Gaps:") {
		t.Errorf("overview does not mention that there are gaps:\n%s", overview)
	}
}

// Filtering to nothing is a different fact from finding nothing, and saying
// "no gaps" to a filtered call would be the same mistake in a new place.
func TestFilteredDiagnosticsSayTheFilterMatchedNothing(t *testing.T) {
	root := repoWith(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"Dockerfile":   "FROM scratch\n",
	})
	s, ctx := connect(t, root)

	out, _ := callText(t, s, ctx, "structura_diagnostics", map[string]any{"code": "no_such_code"})
	if !strings.Contains(out, "filter") {
		t.Errorf("a filter that matched nothing is reported as no gaps at all:\n%s", out)
	}
	if strings.Contains(out, "not the same as nothing being missing") {
		t.Errorf("a filtered call printed the unfiltered explanation:\n%s", out)
	}
}

// trace_path answers blast-radius questions, where a missed route is the
// whole failure. A longer second route must be reported, not dropped for
// being longer than the first.
func TestTracePathReturnsEveryRouteNotJustTheShortest(t *testing.T) {
	root := repoWith(t, map[string]string{
		"docker-compose.yml": `services:
  gateway:
    image: gateway:1
    environment:
      CHECKOUT_URL: http://checkout:8080
  edge:
    image: edge:1
    environment:
      GATEWAY_URL: http://gateway:8080
      CHECKOUT_URL: http://checkout:8080
  checkout:
    image: checkout:1
    environment:
      DATABASE_URL: postgres://checkout@orders-db:5432/orders
  orders-db:
    image: postgres:16
`,
	})
	s, ctx := connect(t, root)

	out, _ := callText(t, s, ctx, "structura_trace_path", map[string]any{
		"from": "edge", "to": "orders-db", "max_paths": 5,
	})
	// edge -> checkout -> orders-db, and edge -> gateway -> checkout -> orders-db.
	if !strings.Contains(out, "2 paths") {
		t.Errorf("the longer route through gateway was dropped:\n%s", out)
	}
	if !strings.Contains(out, "gateway") {
		t.Errorf("no path mentions gateway, which sits on a real route:\n%s", out)
	}
}

// Every identifier a tool prints has to be one the tools accept back. A
// reader should not have to reconstruct an ID from a display name.
func TestPrintedIdentifiersAreAcceptedBack(t *testing.T) {
	root := repoWith(t, map[string]string{
		"docker-compose.yml": `services:
  checkout:
    image: checkout:1
    environment:
      DATABASE_URL: postgres://checkout@orders-db:5432/orders
  orders-db:
    image: postgres:16
`,
	})
	s, ctx := connect(t, root)

	described, _ := callText(t, s, ctx, "structura_describe_node", map[string]any{"node": "checkout"})

	// Pull the names this tool printed for the things checkout depends on,
	// and feed each one back in.
	for _, line := range strings.Split(described, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "persists_to") {
			continue
		}
		name := fields[1]
		out, isErr := callText(t, s, ctx, "structura_describe_node", map[string]any{"node": name})
		if isErr {
			t.Errorf("describe_node printed %q and then refused it: %s", name, out)
		}
	}
}
