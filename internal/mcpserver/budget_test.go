package mcpserver

import (
	"strings"
	"testing"
)

func TestResponseHeadersAreNeverTruncated(t *testing.T) {
	// Headers carry the framing the model needs to interpret the body. A
	// response that spends its whole budget on headers is degenerate, but
	// silently dropping them would be worse: the model would not know what
	// it was looking at.
	r := NewResponse(2)
	r.Headerf("a header well over the budget, spelled out at length")
	r.Headerf("and a second one")

	out := r.String()
	if !strings.Contains(out, "a header well over the budget") ||
		!strings.Contains(out, "and a second one") {
		t.Fatalf("a header was dropped:\n%s", out)
	}
}

func TestResponseStopsAtBudgetAndSaysSo(t *testing.T) {
	r := NewResponse(20)
	fit := 0
	for i := 0; i < 50; i++ {
		if r.Itemf("  item number %d with some trailing detail", i) {
			fit++
		}
	}

	if !r.Truncated() {
		t.Fatal("50 items fit in a 20-token budget; the budget is not being applied")
	}
	if r.Shown() != fit {
		t.Fatalf("Shown() = %d, but %d items reported fitting", r.Shown(), fit)
	}
	if r.Total() != 50 {
		t.Fatalf("Total() = %d, want 50 — items past the cut must still be counted", r.Total())
	}

	out := r.String()
	if !strings.Contains(out, "truncated") {
		t.Fatalf("truncation was silent, which is the failure this type exists to prevent:\n%s", out)
	}
	if !strings.Contains(out, "of 50") {
		t.Fatalf("the notice does not say how much was withheld:\n%s", out)
	}
	if !strings.Contains(out, "cursor=") {
		t.Fatalf("the notice does not say how to continue:\n%s", out)
	}
}

func TestResponseStopsAtFirstOverflow(t *testing.T) {
	// Once one item does not fit, no later item is attempted. Skipping the
	// long ones and continuing would make the contents depend on formatting
	// accidents rather than on the cursor, and pagination would lose items.
	r := NewResponse(30)
	r.Itemf("short")
	r.Itemf("%s", strings.Repeat("x", 4*40))
	if r.Itemf("short again") {
		t.Fatal("an item was accepted after an earlier one overflowed")
	}
	if got := r.Shown(); got != 1 {
		t.Fatalf("Shown() = %d, want 1", got)
	}
}

func TestResponseUntruncatedHasNoNotice(t *testing.T) {
	r := NewResponse(1000)
	r.Headerf("header")
	r.Itemf("  one")
	r.Itemf("  two")

	out := r.String()
	if strings.Contains(out, "truncated") {
		t.Fatalf("a complete response claims truncation:\n%s", out)
	}
	if out != "header\n  one\n  two\n" {
		t.Fatalf("unexpected rendering:\n%q", out)
	}
}

func TestStringWithCursorWithoutACursor(t *testing.T) {
	r := NewResponse(4)
	r.Itemf("%s", strings.Repeat("y", 400))

	out := r.StringWithCursor("", "components")
	if !strings.Contains(out, "Narrow the query") {
		t.Fatalf("a tool with no continuation cursor must say what to do instead:\n%s", out)
	}
}

func TestEstimateTokensRoundsUp(t *testing.T) {
	// Rounding down would let a response creep over its budget one item at a
	// time; the estimate is meant to be conservative.
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
	} {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResponsePreservesWriteOrder(t *testing.T) {
	// Headers and items must interleave in the order they were written. A
	// response that buffers them separately renders every section's heading
	// first and then every section's contents in one run, which turns
	// "Depends on" and "Depended on by" into a single undifferentiated list
	// — with each edge filed under the wrong direction.
	r := NewResponse(1000)
	r.Headerf("Depends on:")
	r.Itemf("  postgres")
	r.Headerf("Depended on by:")
	r.Itemf("  gateway")

	want := "Depends on:\n  postgres\nDepended on by:\n  gateway\n"
	if got := r.String(); got != want {
		t.Errorf("sections were reordered:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
