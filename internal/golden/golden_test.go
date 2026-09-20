package golden_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/golden"
)

func TestDiffShowsOnlyTheChange(t *testing.T) {
	want := strings.Join([]string{"a", "b", "c", "d", "e", "f", "g", "h"}, "\n")
	got := strings.Join([]string{"a", "b", "c", "d", "E", "f", "g", "h"}, "\n")

	d := golden.Diff(want, got)
	if !strings.Contains(d, "- e") || !strings.Contains(d, "+ E") {
		t.Errorf("diff does not show the changed line:\n%s", d)
	}
	// The unchanged head and tail beyond the context window stay out of it.
	if strings.Contains(d, "  a\n") {
		t.Errorf("diff includes lines far outside the change:\n%s", d)
	}
}

func TestDiffHandlesLengthChange(t *testing.T) {
	d := golden.Diff("a\nb\nc", "a\nc")
	if !strings.Contains(d, "- b") {
		t.Errorf("diff does not show the removed line:\n%s", d)
	}
}

func TestDiffOfIdenticalInputIsEmptyBody(t *testing.T) {
	d := golden.Diff("a\nb", "a\nb")
	body := strings.TrimPrefix(d, "--- want\n+++ got\n")
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
			t.Errorf("identical inputs produced a change line: %q", line)
		}
	}
}
