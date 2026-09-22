// Package graphio reads and writes graph.json.
package graphio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamronarabi/structura/pkg/schema"
)

const (
	// Dir is the scan output directory, relative to the repository root.
	Dir = ".structura"
	// GraphName is the graph filename inside Dir.
	GraphName = "graph.json"
)

// ErrNotFound reports that a repository has not been scanned yet.
var ErrNotFound = errors.New("no graph found; run `structura scan` first")

// Path returns where a repository's graph lives.
func Path(root string) string {
	return filepath.Join(root, Dir, GraphName)
}

// Save writes the graph atomically and returns the path written.
//
// The write goes to a temporary file and is renamed into place so that a scan
// interrupted halfway through leaves the previous graph intact rather than a
// truncated one. The MCP server reads this file on demand, possibly while a
// scan is running.
func Save(root string, g schema.Graph) (string, error) {
	dest := Path(root)
	data, err := schema.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("serializing graph: %w", err)
	}
	if err := WriteAtomic(dest, data); err != nil {
		return "", err
	}
	return dest, nil
}

// WriteAtomic installs data at dest through a temporary file in the same
// directory, creating the directory if it does not exist.
//
// It is exported because `scan -o somewhere/else.json` writes the same
// artifact to a different path, and it used to do so with a plain
// os.WriteFile. That is the whole atomicity guarantee applying to one of the
// two ways of asking for the same file: an interrupted scan left a truncated
// graph if and only if the user had passed -o.
func WriteAtomic(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".graph-*.json")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best effort on the failure path

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the real one
		return fmt.Errorf("writing graph: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing graph: %w", err)
	}
	// 0644 rather than the 0600 CreateTemp gives us: graph.json is meant to
	// be committed and read by other tools.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("setting graph permissions: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("installing graph: %w", err)
	}
	return nil
}

// Load reads a repository's graph.
func Load(root string) (schema.Graph, error) {
	data, err := os.ReadFile(Path(root))
	if os.IsNotExist(err) {
		return schema.Graph{}, ErrNotFound
	}
	if err != nil {
		return schema.Graph{}, fmt.Errorf("reading graph: %w", err)
	}
	g, err := schema.Unmarshal(data)
	if err != nil {
		return schema.Graph{}, fmt.Errorf("parsing %s: %w", Path(root), err)
	}
	return g, nil
}

// ModTime reports when the stored graph was last written.
func ModTime(root string) (t int64, ok bool) {
	info, err := os.Stat(Path(root))
	if err != nil {
		return 0, false
	}
	return info.ModTime().Unix(), true
}
