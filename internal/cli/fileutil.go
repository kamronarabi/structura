package cli

import (
	"os"
	"path/filepath"
	"strings"
)

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// relativeToRoot shortens a path for display, so the terminal shows
// ".structura/graph.json" rather than the reader's home directory.
func relativeToRoot(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}
