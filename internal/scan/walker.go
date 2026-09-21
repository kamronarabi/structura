package scan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// DefaultMaxFileSize caps how large a file may be before the walker declines
// to hand it to an extractor. Architecture lives in small declarative files;
// anything larger is a lockfile, a vendored bundle, or a data dump, and
// parsing it would cost far more than it is worth.
const DefaultMaxFileSize = 1 << 20 // 1 MiB

// WalkOptions configures a walk.
type WalkOptions struct {
	// MaxFileSize overrides DefaultMaxFileSize when non-zero.
	MaxFileSize int64
	// FollowSymlinkedDirs opts into descending through directory symlinks.
	// Off by default: it invites cycles and lets a scan wander outside the
	// repository.
	FollowSymlinkedDirs bool
	// DisableDefaultIgnores drops the built-in exclusion list, leaving only
	// the repository's own .gitignore and .structuraignore files. It exists
	// so the matcher can be compared against git, which has no such list,
	// and is exposed as --no-default-ignores for repositories that keep
	// something meaningful in an ordinarily-skipped directory.
	DisableDefaultIgnores bool
}

// WalkResult is everything a walk learned.
type WalkResult struct {
	// Newest is the most recent modification time among matched files, which
	// is what tells a long-running MCP server its stored graph has gone
	// stale.
	Newest int64
	// Files are the files at least one extractor claimed, sorted by path.
	// Sorting here is what makes the rest of the pipeline reproducible: the
	// merge step consumes extractor output in this order.
	Files []FileMeta
	// Scanned counts every file the walker considered, ignored or not.
	Scanned int
	// Unsupported are files no extractor claimed but whose name says they
	// describe architecture: a Dockerfile, a Fly configuration. They are kept
	// so the scan can report what it did not read, and only these are kept --
	// retaining every unmatched file would mean holding most of the
	// repository in memory to say something nobody needs said.
	Unsupported []FileMeta
	// Diagnostics report what the walk had to skip.
	Diagnostics []schema.Diagnostic
}

// Walk traverses root, applying ignore rules, and returns the files that at
// least one registered extractor claims.
func Walk(ctx context.Context, root string, reg *Registry, opts WalkOptions) (WalkResult, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return WalkResult{}, fmt.Errorf("resolving root: %w", err)
	}
	// Resolving symlinks in the root itself keeps the containment check
	// below honest when the repository is reached through one, which is
	// routine on macOS (/tmp is a symlink to /private/tmp).
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}

	w := &walker{
		root:     absRoot,
		registry: reg,
		maxSize:  opts.MaxFileSize,
		follow:   opts.FollowSymlinkedDirs,
	}
	if w.maxSize <= 0 {
		w.maxSize = DefaultMaxFileSize
	}

	base := &ignoreStack{}
	if !opts.DisableDefaultIgnores {
		base = base.push(defaultPatternList())
	}
	if err := w.walkDir(ctx, "", base); err != nil {
		return WalkResult{}, err
	}

	sort.SliceStable(w.files, func(i, j int) bool { return w.files[i].Path < w.files[j].Path })

	var newest int64
	for _, f := range w.files {
		if f.ModTime > newest {
			newest = f.ModTime
		}
	}
	return WalkResult{
		Files:       w.files,
		Scanned:     w.scanned,
		Unsupported: w.unsupported,
		Newest:      newest,
		Diagnostics: w.diags,
	}, nil
}

type walker struct {
	root     string
	registry *Registry
	maxSize  int64
	follow   bool

	files       []FileMeta
	scanned     int
	unsupported []FileMeta
	diags       []schema.Diagnostic
}

// ignoreFileNames are read at every directory, lowest precedence first.
// .structuraignore comes last so it can re-include something .gitignore
// excluded: build output a developer keeps out of git may still be the only
// place the deployed topology is written down.
var ignoreFileNames = []string{".gitignore", ".structuraignore"}

func (w *walker) walkDir(ctx context.Context, relDir string, stack *ignoreStack) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	absDir := w.abs(relDir)
	for _, name := range ignoreFileNames {
		stack = w.pushIgnoreFile(stack, relDir, filepath.Join(absDir, name), name)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		w.diag(schema.SeverityWarn, "unreadable_directory", relDir,
			fmt.Sprintf("could not read directory: %v", rootRelativeError(err, w.root)))
		return nil
	}
	// os.ReadDir already sorts by filename, but the guarantee matters enough
	// to the output's reproducibility to state it rather than inherit it.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		relPath := path.Join(relDir, entry.Name())

		isDir, skip := w.classify(entry, relPath)
		if skip {
			continue
		}
		if ignored, _ := stack.ignored(relPath, isDir); ignored {
			continue
		}
		if isDir {
			if err := w.walkDir(ctx, relPath, stack); err != nil {
				return err
			}
			continue
		}
		w.consider(entry, relPath)
	}
	return nil
}

// classify resolves what an entry is, following a symlink only as far as is
// safe, and reports whether the walk should skip it entirely.
func (w *walker) classify(entry fs.DirEntry, relPath string) (isDir, skip bool) {
	if entry.Type()&fs.ModeSymlink == 0 {
		if entry.IsDir() {
			return true, false
		}
		// Sockets, devices, and FIFOs are not files we can usefully parse,
		// and reading a FIFO would block the scan forever.
		if !entry.Type().IsRegular() {
			return false, true
		}
		return false, false
	}

	target, err := filepath.EvalSymlinks(w.abs(relPath))
	if err != nil {
		// A broken symlink is normal in a working tree; it is not worth a
		// diagnostic of its own.
		return false, true
	}
	// A symlink pointing outside the repository would pull unrelated — and
	// possibly sensitive — files into the graph.
	if !w.contains(target) {
		w.diag(schema.SeverityInfo, "symlink_escapes_root", relPath,
			"symlink points outside the repository and was not followed")
		return false, true
	}

	info, err := os.Stat(target)
	if err != nil {
		return false, true
	}
	if info.IsDir() {
		if !w.follow {
			// Within the repository a directory symlink is usually a
			// duplicate view of a tree we will walk anyway, and following it
			// risks a cycle.
			w.diag(schema.SeverityInfo, "symlink_dir_skipped", relPath,
				"directory symlink not followed; re-run with --follow-symlinks to descend")
			return false, true
		}
		return true, false
	}
	if !info.Mode().IsRegular() {
		return false, true
	}
	return false, false
}

func (w *walker) consider(entry fs.DirEntry, relPath string) {
	w.scanned++

	info, err := entry.Info()
	if err != nil {
		return
	}
	size, modTime := info.Size(), info.ModTime().Unix()
	// A symlink's own Info describes the link, not the target.
	if entry.Type()&fs.ModeSymlink != 0 {
		if target, err := os.Stat(w.abs(relPath)); err == nil {
			size, modTime = target.Size(), target.ModTime().Unix()
		}
	}

	meta := newFileMeta(relPath, size, modTime)
	if len(w.registry.MatchAll(meta)) == 0 {
		if _, ok := matchUnsupported(meta); ok {
			w.unsupported = append(w.unsupported, meta)
		}
		return
	}
	// The size check runs after matching so that the diagnostic is only
	// raised for files we would actually have parsed. Reporting every large
	// binary in the tree would bury the one that mattered.
	if size > w.maxSize {
		w.diag(schema.SeverityWarn, "file_too_large", relPath,
			fmt.Sprintf("file is %d bytes, over the %d byte limit; skipped", size, w.maxSize))
		return
	}
	w.files = append(w.files, meta)
}

func (w *walker) pushIgnoreFile(stack *ignoreStack, relDir, absPath, name string) *ignoreStack {
	f, err := os.Open(absPath)
	if err != nil {
		return stack
	}
	defer f.Close() //nolint:errcheck // read-only

	source := name
	if relDir != "" {
		source = relDir + "/" + name
	}
	pl, err := compilePatterns(f, relDir, source)
	if err != nil {
		w.diag(schema.SeverityWarn, "unreadable_ignore_file", source,
			fmt.Sprintf("could not read ignore file: %v", rootRelativeError(err, w.root)))
		return stack
	}
	return stack.push(pl)
}

func (w *walker) abs(relPath string) string {
	if relPath == "" {
		return w.root
	}
	return filepath.Join(w.root, filepath.FromSlash(relPath))
}

// contains reports whether an absolute path lies inside the repository root.
func (w *walker) contains(absPath string) bool {
	rel, err := filepath.Rel(w.root, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *walker) diag(sev schema.Severity, code, relPath, msg string) {
	w.diags = append(w.diags, schema.Diagnostic{
		Severity: sev, Code: code, Path: relPath, Message: msg,
	})
}

// rootRelativeError strips the absolute repository path out of an error
// message. Diagnostics end up in graph.json, which is committed and pasted
// into LLM context, and an absolute path there leaks the author's home
// directory for no benefit.
func rootRelativeError(err error, root string) string {
	return strings.ReplaceAll(err.Error(), root+string(filepath.Separator), "")
}
