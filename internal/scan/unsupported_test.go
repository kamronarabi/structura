package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

func writeFiles(t *testing.T, files map[string]string) string {
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

func scanRoot(t *testing.T, root string) schema.Graph {
	t.Helper()
	res, err := scan.Run(context.Background(), extractors.Default(), scan.Options{Root: root})
	if err != nil {
		t.Fatalf("scan.Run() = %v", err)
	}
	return res.Graph
}

func diagnosticsWithCode(g schema.Graph, code string) []schema.Diagnostic {
	var out []schema.Diagnostic
	for _, d := range g.Diagnostics {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

// The case this exists for: an application whose only infrastructure file is
// one Structura cannot read. Before, the scan reported no gaps at all, which
// reads as "nothing is missing".
func TestUnreadArchitecturalFileIsReported(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"package.json": `{"name":"app","dependencies":{"react":"^18.0.0"}}`,
		"vercel.json":  `{"rewrites":[{"source":"/api/(.*)","destination":"/api"}]}`,
	}))

	got := diagnosticsWithCode(g, "unsupported_format")
	if len(got) != 1 {
		t.Fatalf("unsupported_format diagnostics = %d, want 1: %+v", len(got), g.Diagnostics)
	}
	if got[0].Path != "vercel.json" {
		t.Errorf("Path = %q, want vercel.json", got[0].Path)
	}
	if got[0].Severity != schema.SeverityInfo {
		t.Errorf("Severity = %q, want info", got[0].Severity)
	}
	// Singular subject, singular pronouns.
	for _, wrong := range []string{"they ", "their ", "them "} {
		if strings.Contains(got[0].Message, wrong) {
			t.Errorf("singular message uses a plural pronoun: %s", got[0].Message)
		}
	}
}

// One entry per format, not one per file, or a monorepo of forty services
// buries every other diagnostic under forty copies of one gap.
func TestManyFilesOfOneFormatAreOneDiagnostic(t *testing.T) {
	files := map[string]string{"package.json": `{"name":"app"}`}
	for _, svc := range []string{"a", "b", "c"} {
		files["services/"+svc+"/Procfile"] = "web: ./" + svc + "\n"
	}
	g := scanRoot(t, writeFiles(t, files))

	got := diagnosticsWithCode(g, "unsupported_format")
	if len(got) != 1 {
		t.Fatalf("diagnostics = %d, want 1: %+v", len(got), got)
	}
	msg := got[0].Message
	if !strings.Contains(msg, "3 Procfiles") {
		t.Errorf("message does not count the files: %s", msg)
	}
	for _, svc := range []string{"a", "b", "c"} {
		if !strings.Contains(msg, "services/"+svc+"/Procfile") {
			t.Errorf("message does not name services/%s/Procfile: %s", svc, msg)
		}
	}
	// Naming one of several in Path would send a reader to an arbitrary
	// example.
	if got[0].Path != "" {
		t.Errorf("Path = %q, want empty when the entry covers several files", got[0].Path)
	}
}

// Several formats are several diagnostics, each saying what it costs.
func TestDistinctFormatsAreReportedSeparately(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"vercel.json":  `{"functions":{"api/*.ts":{"memory":1024}}}`,
		"fly.toml":     "app = \"demo\"\n",
		"Procfile":     "web: node server.js\n",
	}))

	got := diagnosticsWithCode(g, "unsupported_format")
	if len(got) != 3 {
		t.Fatalf("diagnostics = %d, want 3: %+v", len(got), got)
	}
	joined := ""
	for _, d := range got {
		joined += d.Message + "\n"
	}
	for _, want := range []string{"Vercel", "Fly.io", "Procfile"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no diagnostic mentions %s:\n%s", want, joined)
		}
	}
}

// Ordinary source code is not a gap. Reporting it on every scan would train a
// reader to skip the section that matters.
func TestSourceCodeIsNotReportedAsAGap(t *testing.T) {
	files := map[string]string{"package.json": `{"name":"app"}`}
	for _, name := range []string{
		"src/index.ts", "src/app.tsx", "README.md", "LICENSE",
		"tsconfig.json", ".eslintrc.json", "public/logo.svg", "Makefile",
	} {
		files[name] = "whatever\n"
	}
	g := scanRoot(t, writeFiles(t, files))

	if got := diagnosticsWithCode(g, "unsupported_format"); len(got) != 0 {
		t.Errorf("source and tooling files were reported as gaps: %+v", got)
	}
}

// A file an extractor does read must never be reported as unread, so each
// entry retires itself when support arrives.
func TestFilesAnExtractorReadsAreNotReported(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"docker-compose.yml": "services:\n  web:\n    image: nginx:1.27\n",
		"package.json":       `{"name":"app"}`,
		// The entry that prompted this file, retired when the extractor
		// arrived. A repository whose Dockerfile is read must not also be
		// told its Dockerfile went unread.
		"Dockerfile": "FROM node:22-alpine\nEXPOSE 3000\n",
	}))

	if got := diagnosticsWithCode(g, "unsupported_format"); len(got) != 0 {
		t.Errorf("a file an extractor claimed was reported as unread: %+v", got)
	}
}

// The stats have to carry both numbers, because the tools present them and
// "from 2 files" on a repository of five reads as a complete reading.
func TestStatsRecordScannedAndParsed(t *testing.T) {
	g := scanRoot(t, writeFiles(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"Dockerfile":   "FROM scratch\n",
		"src/index.ts": "export const x = 1\n",
	}))

	if g.Stats.FilesParsed >= g.Stats.FilesScanned {
		t.Errorf("FilesParsed=%d FilesScanned=%d; the difference is what the reader needs",
			g.Stats.FilesParsed, g.Stats.FilesScanned)
	}
	if g.Stats.FilesScanned != 3 {
		t.Errorf("FilesScanned = %d, want 3", g.Stats.FilesScanned)
	}
}
