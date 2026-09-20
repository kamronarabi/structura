package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/kamronarabi/structura/internal/mcpserver"
	"github.com/kamronarabi/structura/internal/scan/extractors"
)

func newMCPCommand(opts *Options) *cobra.Command {
	var logLevel string

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this repository's architecture graph over MCP",
		Long: `Run a Model Context Protocol server on stdin and stdout, exposing this
repository's architecture to an AI coding agent.

The server scans on startup, and rescans whenever a manifest changes, so the
model never reasons about a stale graph. It is normally launched by the editor
rather than by hand; see 'structura install-mcp'.`,
		Args: cobra.NoArgs,
		// The stdio transport owns stdout. Cobra would otherwise print usage
		// text there on a flag error, which the client reads as a malformed
		// protocol frame.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return err
			}
			if opts.Verbose {
				level = slog.LevelDebug
			}

			// Logs go to stderr, always. Anything on stdout that is not a
			// protocol frame corrupts the transport, and the failure the
			// user sees is an opaque parse error in their editor with
			// nothing to connect it back to this process.
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

			server := mcpserver.New(mcpserver.Options{
				Root:     opts.Root,
				Registry: extractors.Default(),
				Logger:   log,
			})
			return server.ServeStdio(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "stderr log level: debug, info, warn, error")
	return cmd
}
