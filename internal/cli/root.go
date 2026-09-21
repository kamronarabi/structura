// Package cli wires the cobra command tree. It stays deliberately thin:
// commands parse flags, call into internal packages, and render results.
// No extraction, resolution, or protocol logic lives here.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kamronarabi/structura/internal/buildinfo"
)

// Options holds settings shared by every subcommand, populated from flags,
// the config file, and STRUCTURA_* environment variables in that precedence.
type Options struct {
	Root    string // repository root to operate on
	Verbose bool

	// Projects are the directories that hold independent projects, read from
	// the config file's "projects" key. Empty -- the usual case -- means the
	// repository is one project. See internal/project for why this is
	// declared rather than detected.
	Projects []string
}

const (
	// ConfigName is the base name of the optional per-repo config file.
	ConfigName = ".structura"
	// EnvPrefix namespaces environment variable overrides.
	EnvPrefix = "STRUCTURA"
	// OutputDir is where the scan writes its artifacts, relative to Root.
	OutputDir = ".structura"
	// GraphFile is the canonical graph filename inside OutputDir.
	GraphFile = "graph.json"
)

// NewRootCommand builds the command tree. It is exported so tests can drive
// the CLI without spawning a process.
func NewRootCommand() *cobra.Command {
	opts := &Options{}
	var cfgFile string

	root := &cobra.Command{
		Use:   "structura",
		Short: "Map your architecture from the configs you already have",
		Long: `Structura scans a repository's infrastructure and dependency manifests and
builds a Structura Architecture Graph (SAG): the services, datastores, queues,
and external systems in your stack, and the inferred edges between them.

The graph is written to .structura/graph.json and served to AI coding agents
over the Model Context Protocol, so a model can reason about your architecture
without reading your source code.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version(),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initConfig(cmd, opts, cfgFile)
		},
	}

	root.SetVersionTemplate("{{.Version}}\n")

	pf := root.PersistentFlags()
	pf.StringVar(&cfgFile, "config", "", "config file (default: <root>/.structura.yaml)")
	pf.StringVarP(&opts.Root, "root", "C", ".", "repository root to operate on")
	pf.BoolVarP(&opts.Verbose, "verbose", "v", false, "verbose diagnostic output on stderr")

	root.AddCommand(
		newInstallMCPCommand(opts),
		newMCPCommand(opts),
		newScanCommand(opts),
		newVersionCommand(),
	)

	return root
}

// initConfig resolves the repository root to an absolute path and layers in
// config-file and environment values for flags the user did not set.
func initConfig(cmd *cobra.Command, opts *Options, cfgFile string) error {
	abs, err := filepath.Abs(opts.Root)
	if err != nil {
		return fmt.Errorf("resolving --root %q: %w", opts.Root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("--root %q: %w", opts.Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--root %q is not a directory", opts.Root)
	}
	opts.Root = abs

	v := viper.New()
	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName(ConfigName)
		v.AddConfigPath(abs)
	}
	v.SetEnvPrefix(EnvPrefix)
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		// An explicitly requested config file that is missing is an error;
		// an absent default one is not.
		if !errors.As(err, &notFound) && cfgFile != "" {
			return fmt.Errorf("reading config %q: %w", cfgFile, err)
		}
		if !errors.As(err, &notFound) && !os.IsNotExist(err) {
			return fmt.Errorf("reading config: %w", err)
		}
	}

	// Flags win over config; only fill in what the user did not pass.
	if !cmd.Flags().Changed("verbose") && v.IsSet("verbose") {
		opts.Verbose = v.GetBool("verbose")
	}
	// Projects have no flag: a repository's boundaries are a property of the
	// repository, not of one invocation, so they belong in the file that is
	// committed alongside it.
	opts.Projects = v.GetStringSlice("projects")
	return nil
}

// Execute runs the CLI and returns a process exit code. Errors are reported on
// stderr so that stdout stays reserved for command output.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := NewRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "structura: %v\n", err)
		return 1
	}
	return 0
}
