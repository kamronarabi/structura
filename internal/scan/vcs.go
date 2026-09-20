package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// readVCS reports the repository's git state.
//
// The commit and branch are read straight out of .git, with no dependency on
// git being installed — which matters, because the binary's whole pitch is
// that it needs nothing else present. Detecting uncommitted changes is the
// one thing that genuinely requires git, so it is attempted only when git is
// on PATH and left unset otherwise rather than reported as a guess.
func readVCS(ctx context.Context, root string) *schema.VCS {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return nil
	}
	// A worktree or submodule has .git as a file containing "gitdir: <path>".
	if !info.IsDir() {
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return nil
		}
		target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if target == "" {
			return nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		gitDir = target
	}

	v := &schema.VCS{}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return nil
	}
	ref := strings.TrimSpace(string(head))

	if after, ok := strings.CutPrefix(ref, "ref: "); ok {
		refPath := strings.TrimSpace(after)
		v.Branch = strings.TrimPrefix(refPath, "refs/heads/")
		v.Commit = resolveRef(gitDir, refPath)
	} else {
		// Detached HEAD: the file holds the commit directly.
		v.Commit = ref
	}
	if v.Commit == "" && v.Branch == "" {
		return nil
	}
	if dirty, ok := gitIsDirty(ctx, root); ok {
		v.Dirty = &dirty
	}
	return v
}

// resolveRef reads a ref from the loose ref file, falling back to
// packed-refs, which is where git puts refs after `git gc`.
func resolveRef(gitDir, refPath string) string {
	loose, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(refPath)))
	if err == nil {
		return strings.TrimSpace(string(loose))
	}

	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(packed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == refPath {
			return sha
		}
	}
	return ""
}

// gitIsDirty reports whether the working tree has uncommitted changes, and
// whether the question could be answered at all.
func gitIsDirty(ctx context.Context, root string) (dirty, known bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return false, false
	}
	cmd := exec.CommandContext(ctx, git, "status", "--porcelain", "--untracked-files=no")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) != "", true
}
