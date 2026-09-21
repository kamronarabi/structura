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
	// The cursor belongs to the tools that accept one. String is used by
	// describe_node and trace_path, which do not.
	if strings.Contains(out, "cursor=") {
		t.Fatalf("String offers a cursor its callers cannot accept:\n%s", out)
	}
	if withCursor := r.StringWithCursor("17", "gaps"); !strings.Contains(withCursor, `cursor="17"`) {
		t.Fatalf("the resumable notice does not say how to continue:\n%s", withCursor)
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

// A tool that cannot be resumed must not tell the model to resume it.
// describe_node and trace_path take no cursor argument, so an instruction to
// call again with one costs a round trip to discover it does not work -- and
// it arrives in the same sentence as the counts, which are the part that
// actually matters.
func TestANoticeWithoutACursorOffersNone(t *testing.T) {
	r := NewResponse(4)
	r.Expect(9)
	r.Itemf("%s", strings.Repeat("y", 400))

	for name, out := range map[string]string{
		"String":           r.String(),
		"StringWithCursor": r.StringWithCursor("", "components"),
	} {
		if strings.Contains(out, "cursor") {
			t.Errorf("%s offers a cursor that does not exist:\n%s", name, out)
		}
		if !strings.Contains(out, "of 9") {
			t.Errorf("%s does not report the real total:\n%s", name, out)
		}
	}
}

// Every caller stops formatting once an item does not fit, so Itemf never
// sees the rest. Without Expect the notice counted the attempts: "17 of 18"
// for a scan that reported 38 gaps, contradicting the header directly above
// it -- and the smaller number was the one attached to the call to action.
func TestTruncationCountsItemsThatWereNeverOffered(t *testing.T) {
	r := NewResponse(20)
	r.Expect(38)
	for i := 0; i < 38; i++ {
		if !r.Itemf("  item number %d with some trailing detail", i) {
			break // what every tool does, and what made the count wrong
		}
	}

	if !r.Truncated() {
		t.Fatal("38 items fit in a 20-token budget")
	}
	if got := r.Total(); got != 38 {
		t.Fatalf("Total() = %d, want 38: the items past the cut were never offered to Itemf", got)
	}
	if out := r.StringWithCursor("1", "gaps"); !strings.Contains(out, "of 38 gaps") {
		t.Fatalf("the notice undercounts what was withheld:\n%s", out)
	}
}

// Expect accumulates, because describe_node writes several sections into one
// response and the notice covers all of them.
func TestExpectAccumulatesAcrossSections(t *testing.T) {
	r := NewResponse(10)
	r.Expect(3)
	r.Expect(4)
	r.Itemf("%s", strings.Repeat("z", 400))
	if got := r.Total(); got != 7 {
		t.Fatalf("Total() = %d, want 7", got)
	}
}

// Expect is a floor, not an override: a caller that offers more than it
// declared must still have all of it counted.
func TestTotalNeverUndercountsWhatWasOffered(t *testing.T) {
	r := NewResponse(10)
	r.Expect(2)
	for i := 0; i < 6; i++ {
		r.Itemf("%s", strings.Repeat("q", 400))
	}
	if got := r.Total(); got != 6 {
		t.Fatalf("Total() = %d, want 6", got)
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
