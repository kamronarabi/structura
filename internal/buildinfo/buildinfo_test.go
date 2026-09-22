package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

// stub replaces the link-time state for one test and restores it after.
// These are package-level variables, so a test that forgot to restore them
// would corrupt every test that ran later.
func stub(t *testing.T, v, c, d string, isDirty bool) {
	t.Helper()
	oldV, oldC, oldD, oldDirty := version, commit, date, dirty
	t.Cleanup(func() { version, commit, date, dirty = oldV, oldC, oldD, oldDirty })
	version, commit, date, dirty = v, c, d, isDirty
}

// BuildID is stamped into every graph and compared on the way back out: the
// MCP server rescans when the id in the file differs from the id of the
// binary reading it. That gives it two obligations that pull in opposite
// directions, and nothing tested either.

// If it is unstable, every session rescans a graph that was already correct.
func TestBuildIDIsStableForOneBuild(t *testing.T) {
	stub(t, "v1.2.3", "abcdef1234567890", "2026-01-01", false)
	first := BuildID()
	for i := 0; i < 3; i++ {
		if got := BuildID(); got != first {
			t.Fatalf("BuildID() = %q on call %d, want %q every time", got, i+2, first)
		}
	}
}

// If it fails to change, a graph produced by different code is served as
// current, and nothing reports it. That is the silent direction.
func TestBuildIDChangesWithTheCode(t *testing.T) {
	seen := map[string]string{}
	for _, c := range []struct {
		name          string
		v, commitHash string
		dirty         bool
	}{
		{"a release", "v1.2.3", "abcdef1234567890", false},
		{"the next commit on that release", "v1.2.3", "0123456789abcdef", false},
		{"the next release from that commit", "v1.2.4", "abcdef1234567890", false},
		{"an uncommitted change on top", "v1.2.3", "abcdef1234567890", true},
		{"a plain go build", "dev", "abcdef1234567890", false},
	} {
		stub(t, c.v, c.commitHash, "", c.dirty)
		id := BuildID()
		if prev, ok := seen[id]; ok {
			t.Errorf("%s and %s both produce BuildID %q; a graph from one would be served as current by the other",
				prev, c.name, id)
		}
		seen[id] = c.name
	}
}

func TestBuildIDContents(t *testing.T) {
	tests := []struct {
		name          string
		v, commitHash string
		dirty         bool
		want          string
	}{
		{
			name: "a release carries its commit, because a version string is chosen by a human",
			v:    "v1.2.3", commitHash: "abcdef1234567890", want: "v1.2.3+abcdef123456",
		},
		{
			name: "a plain go build is identified by its commit alone",
			v:    "dev", commitHash: "abcdef1234567890", want: "dev+abcdef123456",
		},
		{
			// The Makefile already embeds the commit in the version it
			// stamps. Appending it again would make the id longer on every
			// build without making it identify anything more.
			name: "a commit already present in the version is not repeated",
			v:    "4c529e1-dirty", commitHash: "4c529e1", want: "4c529e1-dirty",
		},
		{
			name: "a commit shorter than the truncation length is used whole",
			v:    "dev", commitHash: "abc", want: "dev+abc",
		},
		{
			// Outside a VCS there is nothing to identify the code with. The
			// id is honest about that rather than inventing one, and a reader
			// treating an unrecognized id as "rescan" is then correct.
			name: "no commit leaves only the version",
			v:    "dev", commitHash: "", want: "dev",
		},
		{
			name: "an uncommitted tree says so",
			v:    "v1.2.3", commitHash: "abcdef1234567890", dirty: true, want: "v1.2.3+abcdef123456-dirty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub(t, tt.v, tt.commitHash, "", tt.dirty)
			if got := BuildID(); got != tt.want {
				t.Errorf("BuildID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Set is called by main with whatever the linker supplied, and GoReleaser
// supplies all three while a plain build supplies none. An empty value must
// not overwrite a fallback that is better than nothing.
func TestSetIgnoresEmptyValues(t *testing.T) {
	stub(t, "fallback-version", "fallback-commit", "fallback-date", false)
	Set("", "", "")
	if Version() != "fallback-version" || Commit() != "fallback-commit" || Date() != "fallback-date" {
		t.Errorf("Set(\"\",\"\",\"\") overwrote the fallbacks: %q %q %q", Version(), Commit(), Date())
	}

	Set("v9.9.9", "deadbeef", "2026-02-02")
	if Version() != "v9.9.9" || Commit() != "deadbeef" || Date() != "2026-02-02" {
		t.Errorf("Set did not record its arguments: %q %q %q", Version(), Commit(), Date())
	}
}

func TestStringReportsWhatIsKnown(t *testing.T) {
	stub(t, "v1.2.3", "", "", false)
	out := String()
	// Unknown fields say so rather than rendering as an empty gap the reader
	// has to interpret.
	//
	// Read as fields rather than as a substring search. Counting occurrences
	// of "n/a" in the whole banner passes on linux/amd64 and fails on
	// darwin/arm64, because "darwin/arm64" contains one.
	for _, field := range []string{"commit", "built"} {
		if got := fieldValue(out, field); got != "n/a" {
			t.Errorf("String() %s = %q, want n/a:\n%s", field, got, out)
		}
	}
	if !strings.Contains(out, SchemaVersion) {
		t.Errorf("String() omits the schema version %q:\n%s", SchemaVersion, out)
	}
	if strings.Contains(out, "(dirty)") {
		t.Errorf("String() marked a clean build dirty:\n%s", out)
	}

	stub(t, "v1.2.3", "abcdef", "2026-01-01", true)
	if out := String(); !strings.Contains(out, "v1.2.3 (dirty)") {
		t.Errorf("String() does not mark a dirty build:\n%s", out)
	}
}

// fieldValue returns the value of a "  name:   value" line in the banner.
func fieldValue(banner, name string) string {
	for _, line := range strings.Split(banner, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && key == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// The VCS stamp is what identifies a plain `go build`, which is every build a
// developer makes. Reading it wrong means the MCP server either rescans
// constantly or never notices the code changed.
func TestApplyVCS(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "-compiler", Value: "gc"},
		{Key: "vcs.revision", Value: "abcdef1234567890"},
		{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		{Key: "vcs.modified", Value: "true"},
	}

	t.Run("fills what is missing", func(t *testing.T) {
		stub(t, "dev", "", "", false)
		applyVCS(settings)
		if Commit() != "abcdef1234567890" || Date() != "2026-01-02T03:04:05Z" || !Dirty() {
			t.Errorf("applyVCS left %q %q dirty=%v", Commit(), Date(), Dirty())
		}
	})

	t.Run("does not overwrite what the linker supplied", func(t *testing.T) {
		// The linker's values are the more specific statement: a release
		// stamps the tag it was cut from, and the VCS stamp must not
		// overwrite it with whatever the checkout happened to be.
		stub(t, "v1.0.0", "linker-commit", "linker-date", false)
		applyVCS(settings)
		if Commit() != "linker-commit" || Date() != "linker-date" {
			t.Errorf("applyVCS overwrote linker values: %q %q", Commit(), Date())
		}
	})

	t.Run("a clean checkout is not marked dirty", func(t *testing.T) {
		stub(t, "dev", "", "", false)
		applyVCS([]debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc"},
			{Key: "vcs.modified", Value: "false"},
		})
		if Dirty() {
			t.Error("Dirty() = true for vcs.modified=false")
		}
	})
}

// A build carrying both a commit and a date never consults the VCS stamp, so
// it never learns it was dirty. Pinned because the Makefile relies on it: it
// stamps "-dirty" into the version itself, and a second marker from here
// would render as "-dirty-dirty".
func TestVCSIsNotConsultedOnceCommitAndDateAreKnown(t *testing.T) {
	stub(t, "abc1234-dirty", "abc1234", "2026-01-01", false)
	fillFromVCS()
	if Dirty() {
		t.Error("Dirty() = true; fillFromVCS read the VCS stamp despite having commit and date")
	}
	if got, want := BuildID(), "abc1234-dirty"; got != want {
		t.Errorf("BuildID() = %q, want %q", got, want)
	}
}
