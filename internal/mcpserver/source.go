package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kamronarabi/structura/internal/graphio"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// staleCheckInterval bounds how often the filesystem is consulted.
//
// The scan itself takes tens of milliseconds, but a model can fire several
// tool calls in a row, and walking a large monorepo on each of them would be
// felt. Checking at most this often keeps a stale graph's lifetime short
// without making every call pay for it.
const staleCheckInterval = 3 * time.Second

// Source provides the graph a tool call answers from, scanning when it has to.
//
// A model must never be given a missing or stale graph. It has no way to tell
// that the architecture it is reasoning about is three commits out of date,
// and the resulting answer is wrong in a way that looks exactly like a
// correct one. So the graph is rebuilt whenever a file an extractor cares
// about is newer than the stored graph.
type Source struct {
	root     string
	registry *scan.Registry
	log      *slog.Logger

	mu        sync.Mutex
	graph     schema.Graph
	loaded    bool
	lastCheck time.Time
}

// NewSource returns a Source rooted at a repository.
func NewSource(root string, registry *scan.Registry, log *slog.Logger) *Source {
	return &Source{root: root, registry: registry, log: log}
}

// Graph returns the current graph, scanning first if the stored one is
// missing or out of date.
func (s *Source) Graph(ctx context.Context) (schema.Graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.loaded && time.Since(s.lastCheck) < staleCheckInterval {
		return s.graph, nil
	}

	stored, err := graphio.Load(s.root)
	switch {
	case errors.Is(err, graphio.ErrNotFound):
		s.log.Info("no stored graph; scanning", "root", s.root)
		return s.rescan(ctx)
	case err != nil:
		// A corrupt graph is recoverable by rebuilding it, and a model
		// waiting on an answer is better served by a scan than an error.
		s.log.Warn("stored graph could not be read; rescanning", "error", err)
		return s.rescan(ctx)
	}

	stale, reason, err := s.isStale(ctx, stored)
	if err != nil {
		s.log.Warn("staleness check failed; using the stored graph", "error", err)
	} else if stale {
		s.log.Info("stored graph is out of date; rescanning", "reason", reason)
		return s.rescan(ctx)
	}

	s.graph, s.loaded, s.lastCheck = stored, true, time.Now()
	return s.graph, nil
}

// isStale reports whether anything an extractor would read is newer than the
// stored graph.
func (s *Source) isStale(ctx context.Context, stored schema.Graph) (stale bool, reason string, err error) {
	info, err := os.Stat(graphio.Path(s.root))
	if err != nil {
		return true, "the stored graph is unreadable", nil //nolint:nilerr // rebuilding is the remedy
	}

	walk, err := scan.Walk(ctx, s.root, s.registry, scan.WalkOptions{})
	if err != nil {
		return false, "", err
	}
	if walk.Newest > info.ModTime().Unix() {
		return true, "a manifest has changed since the graph was written", nil
	}
	// A graph written by a different build of the binary may not mean what
	// this one expects it to. What to do about that depends on which
	// direction the difference runs; see pkg/schema/compat.go.
	switch compat := schema.CompareVersion(stored.SchemaVersion); compat.Relation {
	case schema.RelationSame:
	case schema.RelationOlder, schema.RelationIncompatible:
		// Older: rescanning picks up whatever this build learned to see
		// since. Incompatible: nothing about it can be trusted, and a fresh
		// scan is the only honest answer.
		return true, compat.Reason(), nil
	case schema.RelationNewer:
		// Deliberately not stale. Rescanning would overwrite a graph that
		// holds more than this build can represent, and graph.json is meant
		// to be committed, so the loss would land in someone's history
		// looking like an architecture change. Serving it as parsed is
		// honest -- every field this build understands still means what it
		// always did -- so the only thing owed is saying so.
		s.log.Warn("serving a graph from a newer schema; some of it is invisible to this build",
			"stored", compat.Stored, "current", compat.Current,
			"remedy", "upgrade structura, or delete .structura/graph.json to rebuild it")
	}
	return false, "", nil
}

func (s *Source) rescan(ctx context.Context) (schema.Graph, error) {
	started := time.Now()
	res, err := scan.Run(ctx, s.registry, scan.Options{Root: s.root})
	if err != nil {
		return schema.Graph{}, fmt.Errorf("scanning %s: %w", s.root, err)
	}
	if _, err := graphio.Save(s.root, res.Graph); err != nil {
		// Being unable to cache the result is not a reason to fail the call:
		// the graph in memory is perfectly good.
		s.log.Warn("could not write the graph", "error", err)
	}
	s.log.Info("scan complete",
		"nodes", len(res.Graph.Nodes), "edges", len(res.Graph.Edges),
		"duration", time.Since(started).Round(time.Millisecond))

	s.graph, s.loaded, s.lastCheck = res.Graph, true, time.Now()
	return s.graph, nil
}

// Invalidate forces the next call to recheck.
func (s *Source) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
}
