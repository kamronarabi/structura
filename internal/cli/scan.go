package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kamronarabi/structura/internal/graphio"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors"
	"github.com/kamronarabi/structura/pkg/schema"
)

type scanFlags struct {
	output           string
	print            bool
	debugDump        bool
	maxFileSize      int64
	followSymlinks   bool
	noDefaultIgnores bool
	concurrency      int
	quiet            bool
}

func newScanCommand(opts *Options) *cobra.Command {
	var f scanFlags

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Build an architecture graph from this repository's manifests",
		Long: `Scan walks the repository, parses every infrastructure and dependency
manifest it recognizes, and writes a Structura Architecture Graph to
.structura/graph.json.

The scan is deterministic: two runs over an unchanged tree produce
byte-identical output, so the graph can be committed and diffed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runScan(cmd, opts, &f)
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&f.output, "output", "o", "", "write the graph here instead of .structura/graph.json ('-' for stdout)")
	fl.BoolVar(&f.print, "print", false, "print a summary of the graph after scanning")
	fl.BoolVar(&f.debugDump, "debug-dump", false, "print the raw pre-resolution output, grouped by source file")
	fl.Int64Var(&f.maxFileSize, "max-file-size", scan.DefaultMaxFileSize, "skip files larger than this many bytes")
	fl.BoolVar(&f.followSymlinks, "follow-symlinks", false, "descend into directory symlinks inside the repository")
	fl.BoolVar(&f.noDefaultIgnores, "no-default-ignores", false, "do not skip node_modules, vendor, and similar directories")
	fl.IntVar(&f.concurrency, "concurrency", 0, "parallel extractor workers (default: number of CPUs)")
	fl.BoolVarP(&f.quiet, "quiet", "q", false, "suppress the summary line")

	return cmd
}

func runScan(cmd *cobra.Command, opts *Options, f *scanFlags) error {
	res, err := scan.Run(cmd.Context(), extractors.Default(), scan.Options{
		Root:                  opts.Root,
		MaxFileSize:           f.maxFileSize,
		FollowSymlinkedDirs:   f.followSymlinks,
		DisableDefaultIgnores: f.noDefaultIgnores,
		Concurrency:           f.concurrency,
		KeepIntermediate:      f.debugDump,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if f.debugDump {
		writeDebugDump(out, res.Intermediate)
	}

	switch f.output {
	case "-":
		data, err := schema.Marshal(res.Graph)
		if err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
	case "":
		path, err := graphio.Save(opts.Root, res.Graph)
		if err != nil {
			return err
		}
		if !f.quiet {
			fmt.Fprintf(out, "%s\n", summary(res.Graph, relativeToRoot(path, opts.Root)))
		}
	default:
		if err := writeTo(f.output, res.Graph); err != nil {
			return err
		}
		if !f.quiet {
			fmt.Fprintf(out, "%s\n", summary(res.Graph, f.output))
		}
	}

	if f.print {
		writeGraphSummary(out, res.Graph)
	}
	return nil
}

func writeTo(path string, g schema.Graph) error {
	data, err := schema.Marshal(g)
	if err != nil {
		return err
	}
	return writeFile(path, data)
}

func summary(g schema.Graph, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d nodes, %d edges", path, len(g.Nodes), len(g.Edges))
	if n := len(g.Diagnostics); n > 0 {
		fmt.Fprintf(&b, ", %d %s", n, plural(n, "diagnostic", "diagnostics"))
	}
	fmt.Fprintf(&b, " (%d files scanned, %d parsed, %dms)",
		g.Stats.FilesScanned, g.Stats.FilesParsed, g.Stats.DurationMs)
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writeGraphSummary renders the graph for a human reading a terminal.
func writeGraphSummary(w io.Writer, g schema.Graph) {
	// An empty graph is when the diagnostics matter most: they are the only
	// thing that distinguishes "this repository has no manifests" from
	// "your deployment is a Helm chart and we did not render it".
	if len(g.Nodes) == 0 {
		fmt.Fprintln(w, "\nNo components found.")
		if len(g.Diagnostics) == 0 {
			fmt.Fprintln(w, "Structura reads Compose, Kubernetes, and Terraform manifests; this repository has none it recognized.")
		} else {
			fmt.Fprintln(w, "Structura found manifests it could not fully read:")
		}
		writeDiagnostics(w, g)
		return
	}

	byKind := map[schema.NodeKind][]schema.Node{}
	for _, n := range g.Nodes {
		byKind[n.Kind] = append(byKind[n.Kind], n)
	}

	fmt.Fprintln(w, "\nComponents")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, kind := range schema.NodeKinds() {
		for _, n := range byKind[kind] {
			detail := ""
			if img, ok := n.Attrs["image"].(string); ok {
				detail = img
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", kind, n.Name, detail)
		}
	}
	tw.Flush()

	// Containment is structural noise in a relationship list; the component
	// list above already shows what exists.
	var relationships []schema.Edge
	for _, e := range g.Edges {
		if e.Kind != schema.EdgeContains {
			relationships = append(relationships, e)
		}
	}
	if len(relationships) > 0 {
		fmt.Fprintln(w, "\nRelationships")
		names := map[string]string{}
		for _, n := range g.Nodes {
			names[n.ID] = n.Name
		}
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, e := range relationships {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t(%.2f)\n", names[e.From], e.Kind, names[e.To], e.Confidence)
		}
		tw.Flush()
	}

	writeDiagnostics(w, g)
}

func writeDiagnostics(w io.Writer, g schema.Graph) {
	if len(g.Diagnostics) == 0 {
		return
	}
	fmt.Fprintln(w, "\nDiagnostics")
	for _, d := range g.Diagnostics {
		location := d.Path
		if location == "" {
			location = "-"
		}
		if d.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, d.Line)
		}
		fmt.Fprintf(w, "  %-5s %-28s %s\n", d.Severity, d.Code, location)
		fmt.Fprintf(w, "        %s\n", d.Message)
	}
}

// writeDebugDump renders the raw extractor output.
//
// Extractors produce nodes and hints well before the resolver exists to turn
// those hints into edges, so without this the only feedback on a new
// extractor is a graph that looks unchanged. It answers a support question
// too: "why didn't Structura find my service?" is nearly always visible here
// as a hint that was emitted and never matched, or never emitted at all.
func writeDebugDump(w io.Writer, dump *scan.Intermediate) {
	if dump == nil {
		return
	}
	fmt.Fprintln(w, "=== raw extractor output (pre-resolution) ===")
	for _, fo := range dump.Files {
		if len(fo.Extractors) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s  [%s]\n", fo.Path, strings.Join(fo.Extractors, ", "))
		if fo.Err != "" {
			fmt.Fprintf(w, "  error: %s\n", fo.Err)
		}
		for _, n := range fo.Nodes {
			fmt.Fprintf(w, "  node  %s  (%s, confidence %.2f)\n", n.ID, n.Kind, n.Confidence)
		}
		for _, e := range fo.Edges {
			fmt.Fprintf(w, "  edge  %s -[%s]-> %s  (%.2f)\n", e.From, e.Kind, e.To, e.Confidence)
		}
		for _, h := range fo.Hints {
			fmt.Fprintf(w, "  hint  %s  kind=%s tokens=%v port=%d proto=%s raw=%q\n",
				h.FromNode, h.Kind, h.Tokens, h.Port, h.Protocol, h.Raw)
		}
		for _, a := range fo.Aliases {
			target := "selector " + formatSelector(a.Selector)
			switch {
			case a.External != "":
				target = "external " + a.External
			case a.TargetName != "":
				target = "target " + a.TargetName
			}
			fmt.Fprintf(w, "  alias %s.%s  dns=%v ports=%v %s\n", a.Name, a.Namespace, a.DNS, a.Ports, target)
		}
		for _, d := range fo.Diagnostics {
			fmt.Fprintf(w, "  diag  %s %s: %s\n", d.Severity, d.Code, d.Message)
		}
	}
	fmt.Fprintln(w)
}

func formatSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + sel[k]
	}
	return strings.Join(parts, ",")
}
