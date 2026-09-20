// Command structura maps a repository's architecture from its configuration
// files and serves the result to AI coding agents over MCP.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/kamronarabi/structura/internal/buildinfo"
	"github.com/kamronarabi/structura/internal/cli"
)

// Stamped by GoReleaser via -ldflags -X.
var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	os.Exit(run())
}

// run exists so that deferred cleanup happens before os.Exit.
func run() int {
	buildinfo.Set(version, commit, date)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
