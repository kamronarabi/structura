// Package golden compares test output against committed reference files.
//
// Regeneration is driven by the STRUCTURA_GOLDEN environment variable rather
// than the conventional -update flag. A flag has to be registered by every
// test binary it is passed to, so `go test ./... -update` fails in any package
// that does not import this one. An environment variable works uniformly
// across the whole tree, which matters once fixtures are spread over the
// extractor packages.
//
//	STRUCTURA_GOLDEN=update go test ./...   # or: make golden
package golden

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EnvVar names the environment variable that enables regeneration.
const EnvVar = "STRUCTURA_GOLDEN"

// Updating reports whether golden files should be rewritten rather than
// compared.
func Updating() bool {
	return strings.EqualFold(os.Getenv(EnvVar), "update")
}

// Assert compares got against the file at goldenPath.
//
// When the file is missing it is created and the test is marked as failed, so
// that a newly added fixture cannot pass silently on its first run — the
// author has to look at what was generated and commit it deliberately.
func Assert(t *testing.T, goldenPath string, got []byte) {
	t.Helper()

	if Updating() {
		write(t, goldenPath, got)
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if os.IsNotExist(err) {
		write(t, goldenPath, got)
		t.Fatalf("golden file %s did not exist; it has been written.\n"+
			"Review it and commit it if the content is correct.", goldenPath)
	}
	if err != nil {
		t.Fatalf("reading golden file %s: %v", goldenPath, err)
	}

	if bytes.Equal(want, got) {
		return
	}
	t.Errorf("output does not match %s\n%s\n\nRe-run with %s=update to accept the new output.",
		goldenPath, Diff(string(want), string(got)), EnvVar)
}

func write(t *testing.T, goldenPath string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
		t.Fatalf("creating golden directory: %v", err)
	}
	if err := os.WriteFile(goldenPath, content, 0o644); err != nil {
		t.Fatalf("writing golden file %s: %v", goldenPath, err)
	}
}

// Diff renders a line-oriented diff of want against got. It is deliberately
// simple: golden graph files are sorted, so differences are usually a handful
// of adjacent lines rather than a reordering.
func Diff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	var b strings.Builder
	b.WriteString("--- want\n+++ got\n")

	// Trim the common prefix and suffix so the report shows the change
	// rather than the whole file.
	start := 0
	for start < len(wantLines) && start < len(gotLines) && wantLines[start] == gotLines[start] {
		start++
	}
	endW, endG := len(wantLines), len(gotLines)
	for endW > start && endG > start && wantLines[endW-1] == gotLines[endG-1] {
		endW--
		endG--
	}

	const context = 3
	ctxStart := max(start-context, 0)
	for i := ctxStart; i < start; i++ {
		b.WriteString("  " + wantLines[i] + "\n")
	}
	for i := start; i < endW; i++ {
		b.WriteString("- " + wantLines[i] + "\n")
	}
	for i := start; i < endG; i++ {
		b.WriteString("+ " + gotLines[i] + "\n")
	}
	for i := endW; i < min(endW+context, len(wantLines)); i++ {
		b.WriteString("  " + wantLines[i] + "\n")
	}
	return b.String()
}
