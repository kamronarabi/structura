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
// truncation is always announced, always counted, and always accompanied by
// the cursor needed to continue.
type Response struct {
	budget int

	// One builder, so lines appear in the order they were written. Keeping
	// headers in a separate buffer would silently reorder any tool that has
	// more than one section — "Depends on" and "Depended on by" would both
	// print, then every edge from both would follow in a single run.
	out     strings.Builder
	used    int
	shown   int
	total   int
	stopped bool
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

// Truncated reports whether anything was left out.
func (r *Response) Truncated() bool { return r.stopped }

// Shown and Total report how much of the list was rendered.
func (r *Response) Shown() int { return r.shown }

// Total reports how many items were offered.
func (r *Response) Total() int { return r.total }

// Remaining reports the unspent budget.
func (r *Response) Remaining() int { return r.budget - r.used }

// String renders the response, appending a truncation notice when one is
// needed.
//
// The notice names the unit, the counts, and how to continue, because a model
// deciding whether to paginate needs all three: "47 of 312" tells it how much
// it is missing, and the cursor tells it what to do about that.
func (r *Response) String() string {
	out := r.out.String()
	if !r.stopped {
		return out
	}
	return out + fmt.Sprintf("\n[truncated: %d of %d shown. Call again with cursor=%d for the rest.]\n",
		r.shown, r.total, r.shown)
}

// StringWithCursor is String with an explicit continuation cursor, for tools
// whose pagination is not a simple offset.
func (r *Response) StringWithCursor(cursor, unit string) string {
	out := r.out.String()
	if !r.stopped {
		return out
	}
	if cursor == "" {
		return out + fmt.Sprintf("\n[truncated: %d of %d %s shown. Narrow the query to see the rest.]\n",
			r.shown, r.total, unit)
	}
	return out + fmt.Sprintf("\n[truncated: %d of %d %s shown. Call again with cursor=%q for the rest.]\n",
		r.shown, r.total, unit, cursor)
}
