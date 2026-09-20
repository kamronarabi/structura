package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kamronarabi/structura/internal/buildinfo"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// GraphResourceURI is the full graph, for a model that genuinely wants
// everything.
//
// It exists so that "give me the whole thing" is possible but never the
// default. A tool the model reaches for automatically has a budget; a
// resource it must ask for by name does not need one.
const GraphResourceURI = "structura://graph"

// Server serves one repository's architecture graph.
type Server struct {
	source *Source
	log    *slog.Logger
	mcp    *mcp.Server
}

// Options configures a Server.
type Options struct {
	// Root is the repository to serve.
	Root string
	// Registry supplies the extractors used when a rescan is needed.
	Registry *scan.Registry
	// Logger receives diagnostics. It must not write to stdout; see New.
	Logger *slog.Logger
}

// New builds a Server.
//
// The logger is forced onto stderr if none is supplied, and callers are
// expected to do the same. The stdio transport gives the protocol exclusive
// use of stdout: anything else written there is parsed as a frame, fails, and
// surfaces in the user's editor as an opaque error with nothing pointing back
// to us.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Server{
		source: NewSource(opts.Root, opts.Registry, log),
		log:    log,
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "structura",
		Version: buildinfo.Version(),
		Title:   "Structura architecture graph",
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	s.registerTools()
	s.registerResources()
	return s
}

// instructions tell the model what this server is for and, as importantly,
// what its answers do and do not mean.
const instructions = `Structura exposes this repository's architecture: the services, datastores,
queues, and external systems it declares, and the relationships between them,
extracted from infrastructure and dependency manifests.

Start with structura_overview to see what exists, then structura_describe_node
for any component, and structura_trace_path to find how one reaches another.

Two things to keep in mind when using the answers:

Every relationship carries a confidence and the evidence behind it. 1.00 means
a file declared it outright. Anything lower was inferred by matching names
across files, and the evidence names the rule, the file, and the line, so you
can check it rather than take it on trust.

The graph is built from configuration, not source code. A relationship
expressed only in application code is not here. structura_diagnostics lists
what the scan could not read, and it is worth checking before concluding that
a component has no dependencies.`

func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "structura_overview",
		Description: "Summarize this repository's architecture: how many components of each " +
			"kind, which are most connected, and whether the scan left gaps. Start here.",
	}, s.overview)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "structura_list_nodes",
		Description: "List components, optionally filtered by kind, layer, namespace, or a " +
			"substring of the name. Returns ids usable with structura_describe_node.",
	}, s.listNodes)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "structura_describe_node",
		Description: "Describe one component and everything it connects to in either " +
			"direction, with the evidence for each relationship.",
	}, s.describeNode)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "structura_trace_path",
		Description: "Find the shortest dependency paths from one component to another. " +
			"Use this for blast-radius and reachability questions.",
	}, s.tracePath)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "structura_diagnostics",
		Description: "List what the scan could not read — unrendered charts, unresolvable " +
			"references, ambiguous names — and therefore what may be missing from the graph.",
	}, s.diagnostics)
}

func (s *Server) registerResources() {
	s.mcp.AddResource(&mcp.Resource{
		URI:      GraphResourceURI,
		Name:     "architecture-graph",
		Title:    "Full architecture graph",
		MIMEType: "application/json",
		Description: "The complete Structura Architecture Graph as JSON. Large on any real " +
			"repository; prefer the tools unless you need every node at once.",
	}, s.readGraph)
}

func (s *Server) readGraph(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	g, err := s.source.Graph(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the architecture graph: %w", err)
	}
	data, err := schema.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("serializing the graph: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}

// Run serves the protocol over a transport until the context is cancelled or
// the peer disconnects.
func (s *Server) Run(ctx context.Context, transport mcp.Transport) error {
	return s.mcp.Run(ctx, transport)
}

// ServeStdio serves over stdin and stdout.
func (s *Server) ServeStdio(ctx context.Context) error {
	s.log.Info("structura mcp listening on stdio", "version", buildinfo.Version())
	return s.Run(ctx, &mcp.StdioTransport{})
}

// MCP exposes the underlying server, for tests that drive it in process.
func (s *Server) MCP() *mcp.Server { return s.mcp }
