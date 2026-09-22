package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoWithCredentials writes a small Compose stack whose environment holds
// secrets of the shapes the redactor is supposed to recognize.
func repoWithCredentials(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	compose := `services:
  web:
    image: nginx
    environment:
      DATABASE_URL: postgres://shopfront:hunter2@db:5432/shopfront
      BROKER_URL: amqp://guest:swordfish@queue:5672//
      REDIS_URL: redis://cache:6379/0
    depends_on: [db, queue]
  db:
    image: postgres:16
  queue:
    image: rabbitmq:3
`
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The secrets that must never appear anywhere. hunter2 and swordfish are the
// passwords; the usernames beside them are ordinary words that legitimately
// appear elsewhere, so only the passwords are checked.
var secrets = []string{"hunter2", "swordfish"}

// Leaking a credential through an architecture tool would be the worst thing
// this program could do, and the CI gate for it only runs unit tests inside
// internal/resolve. Every path that puts scan output in front of someone is
// checked here instead: the file, the summary, the full listing, the raw
// pre-resolution dump, and the graph written to stdout.
func TestNoOutputPathLeaksACredential(t *testing.T) {
	root := repoWithCredentials(t)

	code, stdout, stderr := run(t, "scan", "-C", root, "--print", "--debug-dump")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}

	stored, err := os.ReadFile(filepath.Join(root, ".structura", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, piped, _ := run(t, "scan", "-C", root, "-o", "-")

	for _, secret := range secrets {
		for _, where := range []struct{ name, body string }{
			{"stdout (summary, listing and debug dump)", stdout},
			{"stderr", stderr},
			{".structura/graph.json", string(stored)},
			{"the graph written to stdout", piped},
		} {
			if strings.Contains(where.body, secret) {
				t.Errorf("%s contains the password %q", where.name, secret)
			}
		}
	}

	// The check above passes trivially if the scan found nothing, so confirm
	// it read the values and kept their shape.
	if !strings.Contains(stdout, "***") {
		t.Error("no redaction marker in the output; the connection strings may not have been read at all")
	}
	if !strings.Contains(stdout, "raw=") {
		t.Error("the debug dump printed no hints, so its output was never exercised")
	}
}

func TestScanSummaryCountsReadAsEnglish(t *testing.T) {
	singular := t.TempDir()
	if err := os.WriteFile(filepath.Join(singular, "docker-compose.yml"),
		[]byte("services:\n  solo:\n    image: nginx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := run(t, "scan", "-C", singular)
	for _, bad := range []string{"1 nodes", "1 edges", "1 files"} {
		if strings.Contains(out, bad) {
			t.Errorf("summary says %q: %s", bad, strings.TrimSpace(out))
		}
	}
	if !strings.Contains(out, "1 file scanned") {
		t.Errorf("summary does not say %q: %s", "1 file scanned", strings.TrimSpace(out))
	}

	_, plural, _ := run(t, "scan", "-C", repoWithCredentials(t))
	if !strings.Contains(plural, "nodes") || !strings.Contains(plural, "edges") {
		t.Errorf("plural summary reads wrong: %s", strings.TrimSpace(plural))
	}
}

// A repository with nothing to find is the case where the diagnostics matter
// most: they are what separates "no manifests here" from "your deployment is
// a Helm chart we did not render".
func TestScanExplainsAnEmptyResult(t *testing.T) {
	_, out, _ := run(t, "scan", "-C", t.TempDir(), "--print")
	if !strings.Contains(out, "No components found.") {
		t.Errorf("output does not say the graph is empty:\n%s", out)
	}
	if !strings.Contains(out, "none it recognized") {
		t.Errorf("an empty repository with no diagnostics should say so plainly:\n%s", out)
	}
}

// -o writes the same artifact to a different path. It used to do so with a
// plain os.WriteFile, so the atomicity the default path promises did not
// apply to it.
func TestScanToAnExplicitPathIsAtomic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not modelled on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := repoWithCredentials(t)
	outDir := t.TempDir()
	dest := filepath.Join(outDir, "graph.json")

	if code, _, errOut := run(t, "scan", "-C", root, "-o", dest); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o755) })

	if code, _, _ := run(t, "scan", "-C", root, "-o", dest); code == 0 {
		t.Fatal("exit code = 0 writing into a directory that cannot be written to")
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the previous graph is gone after a failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a failed write modified the previous graph")
	}

	if err := os.Chmod(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "graph.json" {
			t.Errorf("a failed write left %q behind", e.Name())
		}
	}
}

// -o creates the directories it needs, so a CI job can write straight into a
// build directory that does not exist yet.
func TestScanToANestedPathCreatesItsDirectories(t *testing.T) {
	root := repoWithCredentials(t)
	dest := filepath.Join(t.TempDir(), "build", "artifacts", "graph.json")
	if code, _, errOut := run(t, "scan", "-C", root, "-o", dest); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("graph was not written to %s: %v", dest, err)
	}
}

func TestQuietSuppressesOnlyTheSummary(t *testing.T) {
	root := repoWithCredentials(t)
	_, loud, _ := run(t, "scan", "-C", root)
	_, quiet, _ := run(t, "scan", "-C", root, "-q")
	if strings.TrimSpace(quiet) != "" {
		t.Errorf("-q still printed %q", quiet)
	}
	if strings.TrimSpace(loud) == "" {
		t.Error("the default scan printed no summary at all")
	}
	if _, err := os.Stat(filepath.Join(root, ".structura", "graph.json")); err != nil {
		t.Errorf("-q suppressed the graph as well as the summary: %v", err)
	}
}

// The debug dump exists to answer "why didn't Structura find my service?",
// which is nearly always a hint that was emitted and never matched, or an
// alias whose selector does not line up with any workload. So the alias
// lines, and the selector inside them, are the part that has to be readable.
func TestDebugDumpShowsAliasesAndSelectors(t *testing.T) {
	root := t.TempDir()
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
  labels: {app: api, tier: backend}
spec:
  selector:
    matchLabels: {app: api}
  template:
    metadata:
      labels: {app: api, tier: backend}
    spec:
      containers:
        - name: api
          image: nginx
---
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: prod
spec:
  selector: {app: api, tier: backend}
  ports:
    - port: 8080
`
	if err := os.WriteFile(filepath.Join(root, "deploy.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "scan", "-C", root, "--debug-dump")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "raw extractor output") {
		t.Fatalf("no debug dump in the output:\n%s", out)
	}
	if !strings.Contains(out, "alias api.prod") {
		t.Errorf("the Service produced no alias line:\n%s", out)
	}
	// Sorted, so the same selector always reads the same way rather than
	// depending on Go's map iteration order.
	if !strings.Contains(out, "selector app=api,tier=backend") {
		t.Errorf("the selector is missing or unsorted:\n%s", out)
	}
	if !strings.Contains(out, "node  ") {
		t.Errorf("the dump listed no nodes:\n%s", out)
	}
}

// A dump of a repository with nothing in it must not print a header promising
// output that never comes.
func TestDebugDumpOnAnEmptyRepository(t *testing.T) {
	code, out, errOut := run(t, "scan", "-C", t.TempDir(), "--debug-dump")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, errOut)
	}
	if strings.Contains(out, "node  ") || strings.Contains(out, "hint  ") {
		t.Errorf("an empty repository produced dump entries:\n%s", out)
	}
}
