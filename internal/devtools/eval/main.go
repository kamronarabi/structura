//go:build eval

// Command eval measures whether a model can answer architecture questions
// through Structura's MCP tools.
//
// Everything else in this repository tests the parts. This tests the product:
// a graph can be structurally perfect and still useless if the tool surface
// makes the model take six calls to learn what one should have told it. The
// golden tests cannot see that, and neither can a human reading the JSON.
//
// It is deliberately not part of CI. It costs money, it is nondeterministic,
// and a flaky gate gets disabled — at which point it stops being run at all.
// Run it per milestone and before each release:
//
//	ANTHROPIC_API_KEY=... make eval
//
// Its real value is diagnostic rather than pass/fail. A question that takes
// eight tool calls is a tool-surface design bug, and no amount of graph
// correctness fixes it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.yaml.in/yaml/v3"

	"github.com/kamronarabi/structura/internal/mcpserver"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Suite is the question file.
type Suite struct {
	Questions []Question `yaml:"questions"`
}

// Question is one thing a developer might ask, with the facts a correct
// answer has to contain.
type Question struct {
	ID       string `yaml:"id"`
	Fixture  string `yaml:"fixture"`
	Question string `yaml:"question"`
	// MustMention lists required facts. Alternatives within one fact are
	// separated by "|", because more than one wording is often right.
	MustMention []string `yaml:"must_mention"`
	// MustNotMention catches the failure that matters most: a model that
	// invents a component and states it with confidence.
	MustNotMention []string `yaml:"must_not_mention"`
	MaxToolCalls   int      `yaml:"max_tool_calls"`
}

// Result is one graded answer.
type Result struct {
	Question  Question
	Answer    string
	ToolCalls []string
	Missing   []string
	Invented  []string
	Err       error
	Elapsed   time.Duration
}

// Passed reports whether every required fact appeared and nothing forbidden
// did. Tool-call budget is reported but does not fail a question; it is a
// design signal, not a correctness one.
func (r Result) Passed() bool {
	return r.Err == nil && len(r.Missing) == 0 && len(r.Invented) == 0
}

// defaultModel is the model the tool surface is designed for. The eval is a
// measurement of that surface, so it is pinned rather than following whatever
// the SDK currently calls newest.
const defaultModel = "claude-opus-5"

const systemPrompt = `You are answering questions about a software system's architecture using the
Structura tools, which expose a graph extracted from the repository's
configuration files.

Answer from the tools. Do not speculate about components you have not seen,
and do not fill gaps from general knowledge about how systems like this are
usually built — an invented dependency is worse than an admitted gap.

When a relationship was inferred rather than declared, say so and give its
confidence. When the tools show that something could not be read, say that
too, rather than treating the graph as complete.

Be concise: a few sentences, naming the specific components involved.`

func main() {
	var (
		questionsPath = flag.String("questions", "testdata/eval/questions.yaml", "question file")
		fixtureDir    = flag.String("fixtures", "testdata/golden", "directory holding the fixture repositories")
		model         = flag.String("model", defaultModel, "model to evaluate against")
		only          = flag.String("only", "", "run only questions whose id contains this substring")
		maxTurns      = flag.Int("max-turns", 12, "hard stop on the agent loop, per question")
		verbose       = flag.Bool("v", false, "print each answer in full")
	)
	flag.Parse()

	if err := run(*questionsPath, *fixtureDir, *model, *only, *maxTurns, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "eval: %v\n", err)
		os.Exit(1)
	}
}

func run(questionsPath, fixtureDir, model, only string, maxTurns int, verbose bool) error {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("ANTHROPIC_API_KEY is not set; this harness calls the API and costs money")
	}

	data, err := os.ReadFile(questionsPath) //nolint:gosec // operator-supplied path
	if err != nil {
		return fmt.Errorf("reading %s: %w", questionsPath, err)
	}
	var suite Suite
	if err := yaml.Unmarshal(data, &suite); err != nil {
		return fmt.Errorf("parsing %s: %w", questionsPath, err)
	}

	var selected []Question
	for _, q := range suite.Questions {
		if only == "" || strings.Contains(q.ID, only) {
			selected = append(selected, q)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no questions match %q", only)
	}

	ctx := context.Background()
	client := anthropic.NewClient()

	// One server per fixture, reused across that fixture's questions, so the
	// scan cost is paid once and the model sees exactly what a user would.
	sessions := map[string]*mcp.ClientSession{}
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}()

	fmt.Fprintf(os.Stderr, "evaluating %d questions against %s\n\n", len(selected), model)

	results := make([]Result, 0, len(selected))
	for _, q := range selected {
		session, ok := sessions[q.Fixture]
		if !ok {
			session, err = openFixture(ctx, filepath.Join(fixtureDir, q.Fixture))
			if err != nil {
				return fmt.Errorf("opening fixture %s: %w", q.Fixture, err)
			}
			sessions[q.Fixture] = session
		}

		// Validated before the call, not after: a bad question costs money
		// and produces a failure that looks like a product defect.
		if err := validateQuestion(ctx, session, q); err != nil {
			return fmt.Errorf("question %s: %w", q.ID, err)
		}

		started := time.Now()
		result := ask(ctx, &client, session, model, q, maxTurns)
		result.Elapsed = time.Since(started)
		results = append(results, result)

		printProgress(result, verbose)
	}

	return report(results)
}

// validateQuestion rejects a must_not_mention term that names a real
// component in the graph being asked about.
//
// Such a term cannot be checked by substring matching: the model will
// legitimately name that component while excluding it, and the check will
// read the exclusion as an assertion. Catching this when the suite loads,
// rather than as a mysterious failure after paying for the call, is the
// difference between a harness that measures the tool surface and one that
// measures its own phrasing.
func validateQuestion(ctx context.Context, session *mcp.ClientSession, q Question) error {
	names, err := componentNames(ctx, session)
	if err != nil {
		return err
	}
	for _, forbidden := range q.MustNotMention {
		for _, alt := range strings.Split(forbidden, "|") {
			alt = strings.ToLower(strings.TrimSpace(alt))
			if commonWords[alt] {
				return fmt.Errorf("must_not_mention lists %q, which is too ordinary a word to forbid.\n"+
					"It will match inside other words and inside correct negations. "+
					"Forbid only names a model could invent", alt)
			}
			if names[alt] {
				return fmt.Errorf("must_not_mention lists %q, which is a real component in %s.\n"+
					"A substring check cannot tell \"%s is the answer\" from \"%s is not the answer\", "+
					"so a correct answer that rules it out would be scored as a failure.\n"+
					"Use must_not_mention only for things absent from the graph; put the "+
					"attribution in must_mention instead (for example \"only\" or \"no\")",
					alt, q.Fixture, alt, alt)
			}
		}
	}
	return nil
}

// componentNames returns every node name and id in the served graph, lowered.
func componentNames(ctx context.Context, session *mcp.ClientSession) (map[string]bool, error) {
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: mcpserver.GraphResourceURI})
	if err != nil {
		return nil, fmt.Errorf("reading the graph: %w", err)
	}
	if len(res.Contents) == 0 {
		return nil, errors.New("the graph resource returned nothing")
	}
	// Decoded through the real schema type rather than a hand-rolled struct,
	// so a change to the graph format shows up as a compile error here
	// instead of as a silently empty set that disables the check.
	graph, err := schema.Unmarshal([]byte(res.Contents[0].Text))
	if err != nil {
		return nil, fmt.Errorf("parsing the graph: %w", err)
	}
	names := make(map[string]bool, len(graph.Nodes)*4)
	add := func(s string) {
		if s != "" {
			names[strings.ToLower(s)] = true
		}
	}
	for _, n := range graph.Nodes {
		add(n.Name)
		add(n.ID)
		if n.Tech != nil {
			add(n.Tech.Language)
			add(n.Tech.Runtime)
			add(n.Tech.Framework)
		}
		// A node named "search" running elasticsearch:8.13.0 gets described
		// as elasticsearch, so forbidding that word has exactly the same
		// problem as forbidding the node's own name.
		if image, ok := n.Attrs["image"].(string); ok {
			base := image
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[i+1:]
			}
			base, _, _ = strings.Cut(base, ":")
			add(base)
		}
	}
	return names, nil
}

// commonWords are too ordinary to forbid.
//
// "no" appears in "not" and "cannot"; a model that correctly answers "no"
// would fail a check meant to stop it inventing a component. A term this
// generic is always an attribution check wearing the wrong hat.
var commonWords = map[string]bool{
	"no": true, "not": true, "none": true, "yes": true, "all": true,
	"any": true, "directly": true, "direct": true, "nothing": true,
}

// openFixture starts a server over an in-memory transport, which exercises
// the same tool implementations the stdio binary serves.
func openFixture(ctx context.Context, root string) (*mcp.ClientSession, error) {
	server := mcpserver.New(mcpserver.Options{
		Root:     root,
		Registry: extractors.Default(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.MCP().Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "structura-eval", Version: "0"}, nil)
	return client.Connect(ctx, clientTransport, nil)
}

// ask runs one question to completion, bridging MCP tools into the API's
// tool-use loop.
func ask(ctx context.Context, client *anthropic.Client, session *mcp.ClientSession,
	model string, q Question, maxTurns int,
) Result {
	result := Result{Question: q}

	tools, err := bridgeTools(ctx, session)
	if err != nil {
		result.Err = err
		return result
	}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(q.Question)),
	}

	for turn := 0; turn < maxTurns; turn++ {
		msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: 2048,
			System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			result.Err = fmt.Errorf("turn %d: %w", turn, err)
			return result
		}
		messages = append(messages, msg.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				result.Answer += variant.Text
			case anthropic.ToolUseBlock:
				result.ToolCalls = append(result.ToolCalls, variant.Name)
				output, isErr := callTool(ctx, session, variant.Name, variant.Input)
				results = append(results,
					anthropic.NewToolResultBlock(variant.ID, output, isErr))
			}
		}
		if len(results) == 0 {
			break
		}
		// Each turn's answer text is provisional until the model stops
		// calling tools; only the final turn's prose is the answer.
		result.Answer = ""
		messages = append(messages, anthropic.NewUserMessage(results...))
	}

	result.Missing, result.Invented = grade(result.Answer, q)
	return result
}

// bridgeTools converts the server's advertised tools into API tool
// definitions, so the model sees exactly the surface an editor would give it.
func bridgeTools(ctx context.Context, session *mcp.ClientSession) ([]anthropic.ToolUnionParam, error) {
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing tools: %w", err)
	}
	out := make([]anthropic.ToolUnionParam, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("%s: marshaling its schema: %w", tool.Name, err)
		}
		var decoded struct {
			Properties any      `json:"properties"`
			Required   []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("%s: reading its schema: %w", tool.Name, err)
		}
		properties := decoded.Properties
		if properties == nil {
			properties = map[string]any{}
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: properties,
				Required:   decoded.Required,
			},
		}})
	}
	return out, nil
}

func callTool(ctx context.Context, session *mcp.ClientSession, name string, input json.RawMessage) (string, bool) {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return fmt.Sprintf("could not read the arguments: %v", err), true
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Sprintf("the tool failed: %v", err), true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String(), res.IsError
}

// grade scores an answer by fact coverage.
//
// A substring match is crude, and deliberately so: the alternative is a model
// grading a model, which introduces a second source of nondeterminism into a
// harness whose whole job is to be a stable signal.
//
// The crudeness has one sharp edge, and validateQuestion guards it.
// must_not_mention can only ask "does this string appear at all", which is
// the right question for a component that does not exist and the wrong one
// for a component that does. A good answer to "which service uses Redis"
// names the others while ruling them out — and a substring check reads that
// as the model naming them. Scoring it as a failure penalizes exactly the
// caveats the system prompt asks for.
func grade(answer string, q Question) (missing, invented []string) {
	lower := strings.ToLower(answer)
	for _, fact := range q.MustMention {
		found := false
		for _, alt := range strings.Split(fact, "|") {
			if strings.Contains(lower, strings.ToLower(strings.TrimSpace(alt))) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, fact)
		}
	}
	// Forbidden terms match on word boundaries. A plain substring check would
	// find "sqs" inside a longer identifier and "no" inside "cannot", which
	// makes short but legitimate terms unusable and turns correct answers
	// into failures.
	for _, forbidden := range q.MustNotMention {
		for _, alt := range strings.Split(forbidden, "|") {
			alt = strings.TrimSpace(alt)
			if alt == "" {
				continue
			}
			if wordBoundary(alt).MatchString(answer) {
				invented = append(invented, alt)
				break
			}
		}
	}
	return missing, invented
}

// wordBoundary compiles a case-insensitive whole-word matcher, cached because
// the same terms recur across a run.
var boundaryCache sync.Map

func wordBoundary(term string) *regexp.Regexp {
	if cached, ok := boundaryCache.Load(term); ok {
		return cached.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)(^|[^\w-])` + regexp.QuoteMeta(term) + `($|[^\w-])`)
	boundaryCache.Store(term, re)
	return re
}

func printProgress(r Result, verbose bool) {
	mark := "PASS"
	if !r.Passed() {
		mark = "FAIL"
	}
	fmt.Fprintf(os.Stderr, "%s  %-28s %d calls  %s\n",
		mark, r.Question.ID, len(r.ToolCalls), r.Elapsed.Round(time.Millisecond))
	if r.Err != nil {
		fmt.Fprintf(os.Stderr, "      error: %v\n", r.Err)
	}
	if verbose || !r.Passed() {
		if len(r.Missing) > 0 {
			fmt.Fprintf(os.Stderr, "      missing: %s\n", strings.Join(r.Missing, ", "))
		}
		if len(r.Invented) > 0 {
			fmt.Fprintf(os.Stderr, "      should not have said: %s\n", strings.Join(r.Invented, ", "))
		}
		if r.Answer != "" {
			fmt.Fprintf(os.Stderr, "      %s\n", strings.ReplaceAll(strings.TrimSpace(r.Answer), "\n", "\n      "))
		}
	}
}

// report prints the summary and decides the exit status.
func report(results []Result) error {
	passed := 0
	overBudget := make([]Result, 0)
	totalCalls := 0
	for _, r := range results {
		if r.Passed() {
			passed++
		}
		totalCalls += len(r.ToolCalls)
		if r.Question.MaxToolCalls > 0 && len(r.ToolCalls) > r.Question.MaxToolCalls {
			overBudget = append(overBudget, r)
		}
	}

	fmt.Fprintln(os.Stderr)
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "score\t%d/%d\n", passed, len(results))
	fmt.Fprintf(w, "tool calls\t%d total, %.1f per question\n",
		totalCalls, float64(totalCalls)/float64(len(results)))
	if err := w.Flush(); err != nil {
		return err
	}

	// Over-budget questions are the diagnostic output this harness exists
	// for, and they are reported whether or not the answers were right.
	if len(overBudget) > 0 {
		fmt.Fprintf(os.Stderr, "\nOver the tool-call budget — these are tool-surface findings, not graph bugs:\n")
		sort.Slice(overBudget, func(i, j int) bool {
			return len(overBudget[i].ToolCalls) > len(overBudget[j].ToolCalls)
		})
		for _, r := range overBudget {
			fmt.Fprintf(os.Stderr, "  %-28s %d calls (budget %d): %s\n",
				r.Question.ID, len(r.ToolCalls), r.Question.MaxToolCalls,
				strings.Join(r.ToolCalls, " → "))
		}
	}

	if passed < len(results) {
		return fmt.Errorf("%d of %d questions failed", len(results)-passed, len(results))
	}
	return nil
}
