package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allFiles claims every file, so a walk returns exactly the set the ignore
// rules permitted.
type allFiles struct{}

func (allFiles) Name() string                                  { return "all" }
func (allFiles) Match(FileMeta) bool                           { return true }
func (allFiles) Extract(context.Context, *File, Emitter) error { return nil }

func TestPatternMatching(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// A pattern without a slash matches at any depth.
		{"*.log", "app.log", false, true},
		{"*.log", "deep/nested/app.log", false, true},
		{"*.log", "app.log.bak", false, false},

		// A leading slash anchors to the ignore file's directory.
		{"/root.txt", "root.txt", false, true},
		{"/root.txt", "sub/root.txt", false, false},

		// A slash anywhere but the end also anchors.
		{"docs/*.tmp", "docs/a.tmp", false, true},
		{"docs/*.tmp", "sub/docs/a.tmp", false, false},

		// * does not cross a separator.
		{"docs/*.tmp", "docs/sub/a.tmp", false, false},

		// A trailing slash restricts the pattern to directories.
		{"build/", "build", true, true},
		{"build/", "build", false, false},

		// ** spans any number of segments, including none.
		{"**/deep/*.secret", "deep/a.secret", false, true},
		{"**/deep/*.secret", "x/y/deep/a.secret", false, true},
		{"**/deep/*.secret", "deep/x/a.secret", false, false},
		{"a/**/b", "a/b", false, true},
		{"a/**/b", "a/x/y/b", false, true},
		{"a/**", "a/x/y", false, true},

		// Single-character and class wildcards.
		{"a?c.txt", "abc.txt", false, true},
		{"a?c.txt", "ac.txt", false, false},
		{"[xy]z.txt", "xz.txt", false, true},
		{"[xy]z.txt", "zz.txt", false, false},

		// Directory patterns apply to the directory itself.
		{"node_modules/", "sub/node_modules", true, true},
	}

	for _, tt := range tests {
		name := tt.pattern + " vs " + tt.path
		t.Run(name, func(t *testing.T) {
			p, ok := compilePattern(tt.pattern)
			if !ok {
				t.Fatalf("compilePattern(%q) returned no pattern", tt.pattern)
			}
			if got := p.match(tt.path, tt.isDir); got != tt.want {
				t.Errorf("match(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestPatternParsing(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantOK  bool
		negate  bool
		dirOnly bool
	}{
		{name: "blank line", line: "", wantOK: false},
		{name: "whitespace only", line: "   ", wantOK: false},
		{name: "comment", line: "# a comment", wantOK: false},
		{name: "negation", line: "!keep.log", wantOK: true, negate: true},
		{name: "directory only", line: "build/", wantOK: true, dirOnly: true},
		{name: "escaped hash is a literal", line: `\#notacomment`, wantOK: true},
		{name: "trailing space is insignificant", line: "foo.txt   ", wantOK: true},
		{name: "escaped trailing space is significant", line: `foo\ `, wantOK: true},
		{name: "bare slash is not a pattern", line: "/", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := compilePattern(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("compilePattern(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if p.negate != tt.negate {
				t.Errorf("negate = %v, want %v", p.negate, tt.negate)
			}
			if p.dirOnly != tt.dirOnly {
				t.Errorf("dirOnly = %v, want %v", p.dirOnly, tt.dirOnly)
			}
		})
	}
}

func TestTrailingSpaceHandling(t *testing.T) {
	if got := trimUnescapedTrailingSpace("foo.txt   "); got != "foo.txt" {
		t.Errorf("unescaped trailing space was kept: %q", got)
	}
	if got := trimUnescapedTrailingSpace(`foo\ `); got != `foo\ ` {
		t.Errorf("escaped trailing space was stripped: %q", got)
	}
	if got := trimUnescapedTrailingSpace(`foo\\  `); got != `foo\\` {
		t.Errorf("escaped backslash confused the space handling: %q", got)
	}
}

// The ignore matcher is a reimplementation of a format with a lot of case
// law, so it is checked against the only authority that matters. A tree is
// built, walked by this package, and listed by git; the two file sets must be
// identical. Without an oracle this code would be tested against its author's
// idea of the spec, which is exactly how gitignore reimplementations go wrong.
func TestWalkerAgreesWithGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	files := map[string]string{
		".gitignore": strings.Join([]string{
			"*.log",
			"!important.log",
			"build/",
			"/root-only.txt",
			"docs/*.tmp",
			"**/deep/*.secret",
			"a?c.txt",
			"[xy]z.txt",
			"# a comment",
			"",
			"trailing-space.txt   ",
		}, "\n"),
		"app.log":                  "ignored by *.log",
		"important.log":            "re-included by the negation",
		"root-only.txt":            "ignored: anchored at the root",
		"sub/root-only.txt":        "kept: the anchor does not reach here",
		"trailing-space.txt":       "ignored: trailing spaces are insignificant",
		"build/output.bin":         "ignored: the whole directory is excluded",
		"docs/notes.tmp":           "ignored by docs/*.tmp",
		"docs/notes.md":            "kept",
		"docs/sub/notes.tmp":       "kept: * does not cross a separator",
		"deep/a.secret":            "ignored by **/deep/*.secret",
		"x/y/deep/b.secret":        "ignored at depth",
		"abc.txt":                  "ignored by a?c.txt",
		"ac.txt":                   "kept: ? needs exactly one character",
		"xz.txt":                   "ignored by the character class",
		"zz.txt":                   "kept",
		"sub/.gitignore":           "local.txt\n!keep-me.txt\n",
		"sub/local.txt":            "ignored by the nested ignore file",
		"sub/keep-me.txt":          "re-included by the nested negation",
		"sub/other.txt":            "kept",
		"other/local.txt":          "kept: the nested rule is scoped to sub/",
		"src/main.go":              "kept",
		"nested/deep/inner/x.yaml": "kept",
	}

	dir := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		// A developer's global excludes or template directory would make
		// this comparison depend on the machine it runs on.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "--quiet")

	var gitFiles []string
	for _, line := range strings.Split(run("ls-files", "--others", "--exclude-standard"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			gitFiles = append(gitFiles, line)
		}
	}
	sort.Strings(gitFiles)

	res, err := Walk(context.Background(), dir, NewRegistry(allFiles{}), WalkOptions{
		// git has no built-in exclusion list, so neither can we for this
		// comparison.
		DisableDefaultIgnores: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ours []string
	for _, f := range res.Files {
		// git never reports its own metadata; we filter it rather than rely
		// on the default ignores we just disabled.
		if strings.HasPrefix(f.Path, ".git/") {
			continue
		}
		ours = append(ours, f.Path)
	}
	sort.Strings(ours)

	if diff := diffStrings(gitFiles, ours); diff != "" {
		t.Errorf("walker disagrees with git about which files are ignored:\n%s", diff)
	}
	if len(gitFiles) == 0 {
		t.Fatal("git reported no files; the fixture did not get written")
	}
}

func diffStrings(want, got []string) string {
	inWant := make(map[string]bool, len(want))
	for _, s := range want {
		inWant[s] = true
	}
	inGot := make(map[string]bool, len(got))
	for _, s := range got {
		inGot[s] = true
	}

	var b strings.Builder
	for _, s := range want {
		if !inGot[s] {
			b.WriteString("  git kept, we ignored:  " + s + "\n")
		}
	}
	for _, s := range got {
		if !inWant[s] {
			b.WriteString("  git ignored, we kept:  " + s + "\n")
		}
	}
	return b.String()
}
