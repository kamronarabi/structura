package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kamronarabi/structura/internal/mcpserver"
	"github.com/kamronarabi/structura/internal/scan/extractors"
)

// fixture copies a golden fixture repository into a temporary directory.
//
// The copy matters: serving a repository writes .structura/graph.json into
// it, and a test that dirtied the committed fixtures would make every later
// golden comparison depend on test ordering.
func fixture(t *testing.T, name string) string {
	t.Helper()

	src := filepath.Join("..", "..", "testdata", "golden", name)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	dst := t.TempDir()

	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// The committed graph is the expectation, not part of the repository
		// under test. Copying it would have the server serve a stored answer
		// and leave the scan path untested.
		if d.IsDir() && d.Name() == ".structura" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := os.ReadFile(path) //nolint:gosec // test fixture path
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copying fixture %s: %v", name, err)
	}
	return dst
}

// connect starts a server over an in-memory transport and returns a connected
// client session.
func connect(t *testing.T, root string) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx := context.Background()

	server := mcpserver.New(mcpserver.Options{
		Root:     root,
		Registry: extractors.Default(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, ctx
}

// callText calls a tool and returns its text content.
func callText(t *testing.T, s *mcp.ClientSession, ctx context.Context, name string, args any) (string, bool) {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling arguments: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("unmarshaling arguments: %v", err)
	}

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: params})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s returned %T; the tool surface is text-only", name, c)
		}
		b.WriteString(tc.Text)
	}
	return b.String(), res.IsError
}

func TestListToolsAdvertisesTheWholeSurface(t *testing.T) {
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}

	want := map[string]bool{
		"structura_overview":      false,
		"structura_list_nodes":    false,
		"structura_describe_node": false,
		"structura_trace_path":    false,
		"structura_diagnostics":   false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description; the model chooses tools by reading these", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s was not advertised", name)
		}
	}
}

func TestOverviewOrientsTheModel(t *testing.T) {
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	out, isErr := callText(t, session, ctx, "structura_overview", mcpserver.OverviewArgs{})
	if isErr {
		t.Fatalf("overview reported an error:\n%s", out)
	}
	for _, want := range []string{"components", "relationships", "Components by kind:"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview is missing %q:\n%s", want, out)
		}
	}
	if tokens := mcpserver.EstimateTokens(out); tokens > 700 {
		t.Errorf("overview cost %d tokens; it is the call a model makes on every "+
			"question and must stay small:\n%s", tokens, out)
	}
}

func TestListNodesAndDescribeRoundTrip(t *testing.T) {
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	list, isErr := callText(t, session, ctx, "structura_list_nodes",
		mcpserver.ListNodesArgs{Kind: "service"})
	if isErr {
		t.Fatalf("list_nodes reported an error:\n%s", list)
	}

	// The ids a list returns must be usable verbatim in the next call; that
	// round trip is the whole contract between the two tools.
	var id string
	for _, line := range strings.Split(list, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "service:") {
			id = strings.Fields(trimmed)[0]
			break
		}
	}
	if id == "" {
		t.Fatalf("no service id in the listing:\n%s", list)
	}

	desc, isErr := callText(t, session, ctx, "structura_describe_node",
		mcpserver.DescribeNodeArgs{Node: id})
	if isErr {
		t.Fatalf("describe_node rejected an id that list_nodes produced (%q):\n%s", id, desc)
	}
	if !strings.Contains(desc, id) {
		t.Errorf("description does not name the node it describes:\n%s", desc)
	}
}

func TestDescribeNodeAcceptsAName(t *testing.T) {
	// A model will reach for the name, because that is what it saw and what a
	// person would say. Requiring the full id would cost a round trip on
	// every lookup.
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	list, _ := callText(t, session, ctx, "structura_list_nodes", mcpserver.ListNodesArgs{Kind: "service"})
	var name string
	for _, line := range strings.Split(list, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "service:") {
			id := strings.Fields(trimmed)[0]
			name = id[strings.LastIndex(id, "/")+1:]
			break
		}
	}
	if name == "" {
		t.Fatalf("no service in the listing:\n%s", list)
	}

	desc, isErr := callText(t, session, ctx, "structura_describe_node",
		mcpserver.DescribeNodeArgs{Node: name})
	if isErr {
		t.Fatalf("describe_node rejected the bare name %q:\n%s", name, desc)
	}
}

func TestUnknownNodeSuggestsARecovery(t *testing.T) {
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	out, isErr := callText(t, session, ctx, "structura_describe_node",
		mcpserver.DescribeNodeArgs{Node: "definitely-not-a-service"})
	if !isErr {
		t.Fatalf("an unknown node was reported as success:\n%s", out)
	}
	if !strings.Contains(out, "structura_list_nodes") {
		t.Errorf("the error does not tell the model how to recover:\n%s", out)
	}
}

func TestUnknownEnumNamesTheValidValues(t *testing.T) {
	// A model that asks for kind="database" and is told "0 components match"
	// concludes the system has no databases. Naming the right word costs one
	// call instead of a wrong answer.
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	out, isErr := callText(t, session, ctx, "structura_list_nodes",
		mcpserver.ListNodesArgs{Kind: "database"})
	if !isErr {
		t.Fatalf("an invalid kind was accepted:\n%s", out)
	}
	if !strings.Contains(out, "datastore") {
		t.Errorf("the error does not name the valid values:\n%s", out)
	}
}

func TestTracePathReportsEvidenceAndWeakestLink(t *testing.T) {
	session, ctx := connect(t, fixture(t, "compose-monolith"))

	list, _ := callText(t, session, ctx, "structura_list_nodes", mcpserver.ListNodesArgs{})
	var ids []string
	for _, line := range strings.Split(list, "\n") {
		trimmed := strings.TrimSpace(line)
		if i := strings.Index(trimmed, ":"); i > 0 && strings.HasPrefix(line, "  ") {
			if f := strings.Fields(trimmed); len(f) > 0 && strings.Contains(f[0], ":") {
				ids = append(ids, f[0])
			}
		}
	}
	if len(ids) < 2 {
		t.Skipf("fixture has fewer than two components:\n%s", list)
	}

	// Try every ordered pair until one is connected; which pair that is
	// depends on the fixture, but that some pair is connected is the point.
	var found string
	for _, from := range ids {
		for _, to := range ids {
			if from == to {
				continue
			}
			out, isErr := callText(t, session, ctx, "structura_trace_path",
				mcpserver.TracePathArgs{From: from, To: to})
			if isErr {
				continue
			}
			if strings.Contains(out, "weakest link:") {
				found = out
			}
		}
		if found != "" {
			break
		}
	}
	if found == "" {
		t.Fatal("no path was found between any pair of components in compose-monolith")
	}
	if !strings.Contains(found, "->") {
		t.Errorf("the path does not render its hops:\n%s", found)
	}
}

func TestTracePathWithNoPathDoesNotClaimIndependence(t *testing.T) {
	// "No path" in a configuration-only graph is not "no dependency", and
	// saying so is the difference between an honest answer and a misleading
	// one.
	session, ctx := connect(t, fixture(t, "polyglot-monorepo"))

	list, _ := callText(t, session, ctx, "structura_list_nodes", mcpserver.ListNodesArgs{})
	var ids []string
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(strings.TrimSpace(line)); len(f) > 0 && strings.Contains(f[0], ":") {
			ids = append(ids, f[0])
		}
	}
	if len(ids) < 2 {
		t.Skipf("fixture has fewer than two components:\n%s", list)
	}

	for _, from := range ids {
		for _, to := range ids {
			if from == to {
				continue
			}
			out, isErr := callText(t, session, ctx, "structura_trace_path",
				mcpserver.TracePathArgs{From: from, To: to})
			if isErr || strings.Contains(out, "weakest link:") {
				continue
			}
			if !strings.Contains(out, "No path") {
				continue
			}
			if !strings.Contains(out, "structura_diagnostics") {
				t.Fatalf("a missing path does not point at what was skipped:\n%s", out)
			}
			return
		}
	}
	t.Skip("every pair in this fixture is connected")
}

func TestDiagnosticsRejectsAnUnknownSeverity(t *testing.T) {
	session, ctx := connect(t, fixture(t, "helm-chart"))

	out, isErr := callText(t, session, ctx, "structura_diagnostics",
		mcpserver.DiagnosticsArgs{Severity: "critical"})
	if !isErr {
		t.Fatalf("an invalid severity was accepted:\n%s", out)
	}
	if !strings.Contains(out, "warn") {
		t.Errorf("the error does not name the valid severities:\n%s", out)
	}
}

func TestGraphResourceServesValidJSON(t *testing.T) {
	session, ctx := connect(t, fixture(t, "k8s-microservices"))

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: mcpserver.GraphResourceURI})
	if err != nil {
		t.Fatalf("reading %s: %v", mcpserver.GraphResourceURI, err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(res.Contents))
	}
	var graph map[string]any
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &graph); err != nil {
		t.Fatalf("the graph resource is not valid JSON: %v", err)
	}
	if graph["schemaVersion"] == nil {
		t.Error("the graph resource has no schemaVersion")
	}
}

func TestServerInstructionsWarnAboutTheGraphsLimits(t *testing.T) {
	// The instructions are the only place a model is told that this graph
	// comes from configuration and not from code. Without that, an absent
	// edge reads as an absent dependency.
	session, ctx := connect(t, fixture(t, "k8s-microservices"))
	_ = ctx

	got := session.InitializeResult().Instructions
	for _, want := range []string{"confidence", "evidence", "configuration", "structura_diagnostics"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions do not mention %q:\n%s", want, got)
		}
	}
}
