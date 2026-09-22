// Package mcpserver serves the architecture graph to AI agents over the
// Model Context Protocol.
//
// The premise of the project is token efficiency, so the server must never
// hand a model the whole graph by default. A two-hundred-service graph is
// well past the point where dumping it defeats the purpose of having built
// one: the model would spend its context re-reading a file it could have
// queried. Every tool here answers a specific question within a budget, and
// says so explicitly when it had more to give.
package mcpserver

import (
	"fmt"
	"strings"
)

// charsPerToken is the usual rough conversion for English text and code.
// Exactness does not matter — the budget exists to keep a response in the
// right order of magnitude, not to hit a number.
const charsPerToken = 4

// EstimateTokens approximates the token cost of a string.
func EstimateTokens(s string) int { return (len(s) + charsPerToken - 1) / charsPerToken }

// Response accumulates a tool's output and stops when it runs out of budget.
//
// A silently truncated response is worse than an explicit one. A model that
// receives forty-seven of three hundred and twelve nodes, with no indication
// that there were more, will reason as though it has seen the system — and
// its answer will be confidently wrong in a way nobody can trace. So
// truncation is always announced, always counted against the real total --
// see Expect -- and, where the tool can be resumed, accompanied by the cursor
// to resume it with.
type Response struct {
	budget int

	// One builder, so lines appear in the order they were written. Keeping
	// headers in a separate buffer would silently reorder any tool that has
	// more than one section — "Depends on" and "Depended on by" would both
	// print, then every edge from both would follow in a single run.
	out   strings.Builder
	used  int
	shown int
	total int
	// expected is what the caller said it would offer, which is not the same
	// as what it got round to offering. See Expect.
	expected int
	stopped  bool
}

// NewResponse returns a Response with a token budget.
func NewResponse(tokenBudget int) *Response {
	return &Response{budget: tokenBudget}
}

// Headerf writes a line that is never truncated. Headers carry the framing a
// model needs to interpret the rest — what it is looking at and how much of
// it there is — so spending budget on them is always worth it.
func (r *Response) Headerf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.out.WriteString(line)
	r.out.WriteString("\n")
	r.used += EstimateTokens(line)
}

// Itemf writes one item of a list, reporting whether it fit.
//
// Once an item does not fit, no later item is attempted either. Skipping the
// long ones and continuing would produce a response whose contents depend on
// formatting accidents rather than on the cursor, which makes pagination
// incoherent.
func (r *Response) Itemf(format string, args ...any) bool {
	r.total++
	if r.stopped {
		return false
	}
	line := fmt.Sprintf(format, args...)
	cost := EstimateTokens(line)
	if r.used+cost > r.budget {
		r.stopped = true
		return false
	}
	r.out.WriteString(line)
	r.out.WriteString("\n")
	r.used += cost
	r.shown++
	return true
}

// Countf adds to the total without writing anything, for items the caller
// filtered out before formatting.
func (r *Response) Countf(n int) { r.total += n }

// Expect declares how many items the caller is about to offer, so that a
// truncation notice can report the real total. It accumulates, so a response
// with several sections calls it once per section.
//
// Itemf can only count what it is handed, and every caller stops handing
// items over the moment one does not fit -- correctly, since formatting the
// rest is work thrown away. The consequence was that the notice counted the
// attempts rather than the items: "61 of 62 components shown" for a graph of
// 99, and "17 of 18 gaps" for a scan that reported 38.
//
// That is the exact failure this type was built to prevent, arriving through
// the mechanism meant to prevent it. A model told it has 61 of 62 stops; told
// it has 61 of 99 it asks for the rest. Worse, the header above had already
// printed the true number, so the notice did not merely undercount -- it
// contradicted the response it was attached to, and the smaller number was
// the one carrying the call to action.
func (r *Response) Expect(n int) { r.expected += n }

// Truncated reports whether anything was left out.
func (r *Response) Truncated() bool { return r.stopped }

// Shown and Total report how much of the list was rendered.
func (r *Response) Shown() int { return r.shown }

// Total reports how many items there were, which is what the caller declared
// through Expect when that is larger than what Itemf actually saw.
func (r *Response) Total() int {
	if r.expected > r.total {
		return r.expected
	}
	return r.total
}

// String renders the response, appending a truncation notice when one is
// needed.
//
// The notice carries the counts and nothing else. Only list_nodes and
// diagnostics take a cursor, but this notice offered one to every tool that
// used it -- so a truncated describe_node told the model to call again with
// an argument describe_node does not accept. An instruction that cannot be
// followed is worse than none: the model spends a call discovering that, and
// the honest number it was given in the same sentence gets less attention
// than the remedy that does not work.
func (r *Response) String() string { return r.render("", "") }

// StringWithCursor is String plus a continuation cursor, for the tools that
// accept one.
//
// A model deciding whether to paginate needs the counts and the cursor both:
// "61 of 99" tells it how much it is missing, and the cursor tells it what to
// do about that.
func (r *Response) StringWithCursor(cursor, unit string) string {
	return r.render(cursor, unit)
}

func (r *Response) render(cursor, unit string) string {
	out := r.out.String()
	if !r.stopped {
		return out
	}
	if unit != "" {
		unit = " " + unit
	}
	if cursor == "" {
		return out + fmt.Sprintf("\n[truncated: %d of %d%s shown.]\n", r.shown, r.Total(), unit)
	}
	return out + fmt.Sprintf("\n[truncated: %d of %d%s shown. Call again with cursor=%q for the rest.]\n",
		r.shown, r.Total(), unit, cursor)
}
