package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"time"
)

// Normalize puts a graph into canonical form: every collection sorted, every
// duplicate collapsed, every confidence rounded, stats and content hash
// recomputed.
//
// Determinism is a hard requirement rather than a nicety. Go randomizes map
// iteration order, so without an explicit normalization pass two scans of an
// unchanged tree would produce different bytes — which would break golden
// tests, defeat the scan cache, and make every commit look like an
// architecture change to Phase 4's drift detection.
func (g *Graph) Normalize() {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		n.Confidence = roundConfidence(n.Confidence)
		n.Sources = normalizeSources(n.Sources)
		if n.Tech != nil && *n.Tech == (Tech{}) {
			n.Tech = nil
		}
		if len(n.Attrs) == 0 {
			n.Attrs = nil
		}
	}
	for i := range g.Edges {
		e := &g.Edges[i]
		e.Confidence = roundConfidence(e.Confidence)
		e.Evidence = normalizeEvidence(e.Evidence)
		if e.ID == "" {
			e.ID = NewEdgeID(e.From, e.To, e.Kind, e.Protocol)
		}
	}

	sort.SliceStable(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.SliceStable(g.Edges, func(i, j int) bool { return edgeLess(g.Edges[i], g.Edges[j]) })
	sort.SliceStable(g.Diagnostics, func(i, j int) bool { return diagLess(g.Diagnostics[i], g.Diagnostics[j]) })

	if g.SchemaVersion == "" {
		g.SchemaVersion = Version
	}
	// Derived counts are computed here, on the graph as it will be
	// serialized, so every reader judges the same numbers instead of each
	// deriving its own.
	g.Measure()

	// Empty rather than null, so consumers can iterate without a nil check.
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	if g.Diagnostics == nil {
		g.Diagnostics = []Diagnostic{}
	}

	g.ContentHash = ""
	if h, err := contentHash(*g); err == nil {
		g.ContentHash = h
	}
}

// Canonical returns a copy with the volatile fields cleared: the wall-clock
// timestamp, the scan duration, and the content hash itself. It is what
// equality between two scans actually means, and what the content hash is
// computed over.
func (g Graph) Canonical() Graph {
	g.GeneratedAt = time.Time{}
	g.Stats.DurationMs = 0
	g.ContentHash = ""
	// Provenance, not architecture. Two builds that find the same thing
	// describe the same system and must agree on the hash, or every release
	// would look like the architecture changed.
	g.Generator = nil
	return g
}

func contentHash(g Graph) (string, error) {
	b, err := encode(g.Canonical())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// encode is the single JSON encoder used for output, for golden comparison,
// and for hashing, so all three agree by construction.
func encode(g Graph) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Without this, every & and < in an image tag, URL, or connection string
	// becomes a \u escape, which is unreadable in a file people will open.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Marshal renders a graph as deterministic, human-readable JSON with a
// trailing newline. It normalizes a copy first, so callers cannot emit an
// unsorted graph by accident.
func Marshal(g Graph) ([]byte, error) {
	g.Normalize()
	return encode(g)
}

// Unmarshal parses a graph and normalizes it, so that a load/save round trip
// is a fixed point.
func Unmarshal(b []byte) (Graph, error) {
	var g Graph
	if err := json.Unmarshal(b, &g); err != nil {
		return Graph{}, err
	}
	g.Normalize()
	return g, nil
}

// roundConfidence pins confidences to two decimals. Scores are combined
// arithmetically during resolution, and a stray 0.7000000000000001 would
// serialize differently from 0.7 and break byte-equality for no reason.
func roundConfidence(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return math.Round(f*100) / 100
}

func edgeLess(a, b Edge) bool {
	if a.From != b.From {
		return a.From < b.From
	}
	if a.To != b.To {
		return a.To < b.To
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Protocol != b.Protocol {
		return a.Protocol < b.Protocol
	}
	return a.ID < b.ID
}

func diagLess(a, b Diagnostic) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	if a.Code != b.Code {
		return a.Code < b.Code
	}
	if a.Severity != b.Severity {
		return a.Severity < b.Severity
	}
	return a.Message < b.Message
}

func normalizeSources(in []Source) []Source {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Path != in[j].Path {
			return in[i].Path < in[j].Path
		}
		if in[i].Line != in[j].Line {
			return in[i].Line < in[j].Line
		}
		return in[i].Extractor < in[j].Extractor
	})
	out := in[:0]
	var prev Source
	for i, s := range in {
		if i > 0 && s == prev {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}

func normalizeEvidence(in []Evidence) []Evidence {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Rule != in[j].Rule {
			return in[i].Rule < in[j].Rule
		}
		if in[i].Path != in[j].Path {
			return in[i].Path < in[j].Path
		}
		if in[i].Line != in[j].Line {
			return in[i].Line < in[j].Line
		}
		if in[i].Detail != in[j].Detail {
			return in[i].Detail < in[j].Detail
		}
		// Extractor is compared last, and it has to be compared. The dedup
		// below treats two entries differing only here as distinct, so
		// leaving them equal to the sort kept both in whatever order the
		// concurrent pipeline happened to add them -- which changed
		// graph.json, and with it the content hash, between runs of the same
		// scan.
		return in[i].Extractor < in[j].Extractor
	})
	out := in[:0]
	var prev Evidence
	for i, e := range in {
		if i > 0 && e == prev {
			continue
		}
		out = append(out, e)
		prev = e
	}
	return out
}
