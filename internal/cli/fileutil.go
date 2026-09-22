package cli

import (
	"path/filepath"
	"strings"
)

// relativeToRoot shortens a path for display, so the terminal shows
// ".structura/graph.json" rather than the reader's home directory.
func relativeToRoot(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}
