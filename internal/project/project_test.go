package project_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/project"
)

// The default has to be "one project". Splitting a repository that is
// genuinely one system breaks joins that were correct, and does it silently.
func TestRepositoryIsOneProjectByDefault(t *testing.T) {
	s := project.Discover(nil)

	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1; roots: %v", s.Len(), s.Roots())
	}
	for _, p := range []string{
		"services/api/go.mod",
		"deploy/prod/api.yaml",
		"charts/api/Chart.yaml",
		"deploy/docker-compose.yml",
		"infra/main.tf",
		"go.mod",
	} {
		if got := s.Of(p); got != project.Root {
			t.Errorf("Of(%q) = %q, want %q", p, got, project.Root)
		}
	}
}

// The layouts that an earlier marker-based detection split in half. A chart
// in charts/, a Compose file in deploy/, and a Terraform root in infra/ are
// parts of one repository, not three projects, and nothing may infer
// otherwise without being told.
func TestStackDeclarationsAloneDoNotSplitARepository(t *testing.T) {
	s := project.Discover(nil)
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}
	if got := project.Label(s.Of("charts/api/Chart.yaml")); got != "" {
		t.Errorf("a chart directory was labelled %q; identifiers must be unchanged", got)
	}
}

// Configuration is exact, and is the only thing that creates a boundary.
func TestConfiguredRootsAreHonoured(t *testing.T) {
	s := project.Discover([]string{"samples/a", "samples/b"})

	if s.Len() != 3 { // two configured plus the repository root
		t.Fatalf("Len() = %d, want 3; roots: %v", s.Len(), s.Roots())
	}
	if got := s.Of("samples/a/k8s/app.yaml"); got != "samples/a" {
		t.Errorf("Of() = %q, want samples/a", got)
	}
	if got := s.Of("samples/b/k8s/app.yaml"); got != "samples/b" {
		t.Errorf("Of() = %q, want samples/b", got)
	}
	if got := s.Of("tools/lint/main.go"); got != project.Root {
		t.Errorf("Of() = %q, want %q for a path outside every configured root", got, project.Root)
	}
	if !s.Configured("samples/a") {
		t.Error("a configured root was not reported as configured")
	}
}

// The deepest root wins, so a project nested inside another belongs to the
// inner one.
func TestDeepestProjectWins(t *testing.T) {
	s := project.Discover([]string{"samples/outer", "samples/outer/inner"})

	if got := s.Of("samples/outer/inner/app/package.json"); got != "samples/outer/inner" {
		t.Errorf("Of(inner) = %q, want samples/outer/inner", got)
	}
	if got := s.Of("samples/outer/app/package.json"); got != "samples/outer" {
		t.Errorf("Of(outer) = %q, want samples/outer", got)
	}
}

func TestConfiguredRootsAreCleanedAndBounded(t *testing.T) {
	s := project.Discover([]string{"./samples/a/", "", "   ", "..", "../escape", "/samples/b", "."})

	roots := strings.Join(s.Roots(), ",")
	for _, bad := range []string{"..", "escape"} {
		if strings.Contains(roots, bad) {
			t.Errorf("a root escaping the repository survived: %v", s.Roots())
		}
	}
	if got := s.Of("samples/a/x.yaml"); got != "samples/a" {
		t.Errorf("Of() = %q, want samples/a (trailing slash and ./ prefix should normalize)", got)
	}
	if got := s.Of("samples/b/x.yaml"); got != "samples/b" {
		t.Errorf("Of() = %q, want samples/b (leading slash should normalize)", got)
	}
	// "." is the repository root, which is always present and never a second
	// project.
	if s.Len() != 3 {
		t.Errorf("Len() = %d, want 3; roots: %v", s.Len(), s.Roots())
	}
}

// A single-project repository must produce exactly the identifiers it
// produced before this package existed.
func TestRootProjectHasNoLabel(t *testing.T) {
	if got := project.Label(project.Root); got != "" {
		t.Errorf("Label(root) = %q, want empty", got)
	}
	if got := project.Label(""); got != "" {
		t.Errorf("Label(empty) = %q, want empty", got)
	}
	if got := project.Label("samples/a"); got != "samples-a" {
		t.Errorf("Label() = %q, want samples-a", got)
	}
}

// The result must not depend on the order configuration happened to list
// roots in, or two scans of one tree would disagree.
func TestDiscoveryIsOrderIndependent(t *testing.T) {
	a := strings.Join(project.Discover([]string{"samples/b", "samples/a", "samples/c"}).Roots(), ",")
	b := strings.Join(project.Discover([]string{"samples/c", "samples/b", "samples/a"}).Roots(), ",")
	if a != b {
		t.Errorf("Roots() depends on input order:\n  %s\n  %s", a, b)
	}
}

func TestDuplicateConfiguredRootsCollapse(t *testing.T) {
	s := project.Discover([]string{"samples/a", "./samples/a", "samples/a/"})
	if s.Len() != 2 {
		t.Errorf("Len() = %d, want 2; roots: %v", s.Len(), s.Roots())
	}
}

func TestNilSetBehavesAsSingleProject(t *testing.T) {
	var s *project.Set
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
	if got := s.Of("anything/at/all"); got != project.Root {
		t.Errorf("Of() = %q, want %q", got, project.Root)
	}
	if got := s.Roots(); len(got) != 1 || got[0] != project.Root {
		t.Errorf("Roots() = %v, want [%q]", got, project.Root)
	}
}
