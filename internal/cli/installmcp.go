package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kamronarabi/structura/internal/ideconfig"
)

func newInstallMCPCommand(opts *Options) *cobra.Command {
	var (
		client   string
		scope    string
		dryRun   bool
		binPath  string
		listOnly bool
	)

	cmd := &cobra.Command{
		Use:   "install-mcp",
		Short: "Register Structura as an MCP server in your editor",
		Long: fmt.Sprintf(`Add Structura to an AI coding tool's MCP configuration so it can query this
repository's architecture.

Supported clients: %s.

The existing configuration is parsed and merged, never overwritten: servers
you configured elsewhere are preserved byte for byte, the original is backed
up to <config>%s before anything is written, and re-running this command
changes nothing. A config file that is not valid JSON is refused rather than
replaced.

Use --dry-run to see the exact diff without touching anything.`,
			strings.Join(ideconfig.ClientNames(), ", "), ideconfig.BackupSuffix),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			if listOnly {
				return listClients(cmd)
			}
			if client == "" {
				return fmt.Errorf("--client is required; supported clients are %s",
					strings.Join(ideconfig.ClientNames(), ", "))
			}

			target, err := ideconfig.Lookup(client)
			if err != nil {
				return err
			}

			chosen, err := resolveScope(target, scope)
			if err != nil {
				return err
			}

			home, homeErr := os.UserHomeDir()
			path, err := target.ConfigPath(chosen, opts.Root, home)
			if err != nil {
				if homeErr != nil && chosen == ideconfig.ScopeGlobal {
					return fmt.Errorf("%w: %w", err, homeErr)
				}
				return err
			}

			entry, err := serverEntry(binPath, opts.Root)
			if err != nil {
				return err
			}

			change, err := ideconfig.Plan(target, path, entry)
			if err != nil {
				return err
			}

			if change.NoOp() {
				fmt.Fprintf(out, "%s is already configured to use this repository.\n  %s\n",
					target.Title, path)
				return nil
			}

			if dryRun {
				fmt.Fprintf(out, "Would update %s:\n\n%s\n", path, change.Diff())
				if len(change.Before) > 0 {
					fmt.Fprintf(out, "The original would be backed up to %s.\n", path+ideconfig.BackupSuffix)
				}
				fmt.Fprintf(out, "Nothing was written. Re-run without --dry-run to apply.\n")
				return nil
			}

			backup, err := ideconfig.Apply(change)
			if err != nil {
				return err
			}

			switch {
			case change.Created:
				fmt.Fprintf(out, "Created %s\n", path)
			case change.Replaced:
				fmt.Fprintf(out, "Updated the existing structura entry in %s\n", path)
			default:
				fmt.Fprintf(out, "Added structura to %s\n", path)
			}
			if backup != "" {
				fmt.Fprintf(out, "Backed up the original to %s\n", backup)
			}
			fmt.Fprintf(out, "\nRestart %s to pick up the change, then ask it about this repository's architecture.\n",
				target.Title)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&client, "client", "", "which tool to configure: "+strings.Join(ideconfig.ClientNames(), ", "))
	f.StringVar(&scope, "scope", "", "project (this repository only) or global (every project)")
	f.BoolVar(&dryRun, "dry-run", false, "print the diff without writing anything")
	f.StringVar(&binPath, "binary", "", "path to the structura binary (default: this executable)")
	f.BoolVar(&listOnly, "list", false, "list the supported clients and where each config lives")

	return cmd
}

// resolveScope picks a scope, defaulting to the narrowest one the client
// supports.
//
// Project scope is the default wherever it exists because it is the smaller
// blast radius: a config inside the repository affects one project and can be
// deleted by deleting a file the user can see.
func resolveScope(client ideconfig.Client, requested string) (ideconfig.Scope, error) {
	supported := client.Scopes()
	if requested == "" {
		return supported[0], nil
	}
	for _, s := range supported {
		if strings.EqualFold(string(s), requested) {
			return s, nil
		}
	}
	names := make([]string, 0, len(supported))
	for _, s := range supported {
		names = append(names, string(s))
	}
	return "", fmt.Errorf("%s does not support --scope %q; it supports %s",
		client.Title, requested, strings.Join(names, ", "))
}

// serverEntry builds the command the editor will launch.
//
// The binary is recorded by absolute path and the repository by absolute
// path, because the editor launches this process from a working directory we
// do not control and cannot predict.
func serverEntry(binPath, root string) (ideconfig.Entry, error) {
	if binPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return ideconfig.Entry{}, fmt.Errorf("could not determine this binary's path; "+
				"pass --binary explicitly: %w", err)
		}
		binPath = exe
	}
	abs, err := filepath.Abs(binPath)
	if err != nil {
		return ideconfig.Entry{}, fmt.Errorf("resolving %s: %w", binPath, err)
	}
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ideconfig.Entry{}, fmt.Errorf("no binary at %s; pass --binary with the path to structura", abs)
		}
		return ideconfig.Entry{}, err
	}
	// Symlinks are resolved so that a Homebrew shim moving does not silently
	// break every config we wrote.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ideconfig.Entry{}, fmt.Errorf("resolving %s: %w", root, err)
	}
	return ideconfig.Entry{Command: abs, Args: []string{"mcp", "--root", absRoot}}, nil
}

func listClients(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	home, _ := os.UserHomeDir()

	for _, c := range ideconfig.Clients() {
		fmt.Fprintf(out, "%s  (--client %s)\n", c.Title, c.Name)
		for _, scope := range c.Scopes() {
			path, err := c.ConfigPath(scope, ".", home)
			if err != nil {
				continue
			}
			marker := " "
			if _, err := os.Stat(path); err == nil {
				marker = "*"
			}
			fmt.Fprintf(out, "  %s %-8s %s\n", marker, scope, path)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "* marks a file that already exists.")
	return nil
}
