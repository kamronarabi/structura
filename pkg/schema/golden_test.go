package schema_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kamronarabi/structura/internal/golden"
	"github.com/kamronarabi/structura/pkg/schema"
)

// The serialized shape of the graph is the project's SemVer contract: the MCP
// server, the UI, and the cloud tier all read it. Pinning a full example here
// means any accidental change to a field name, ordering, or omitempty rule
// shows up as a golden diff and forces a deliberate decision about the schema
// version, rather than slipping out in a patch release.
func TestSchemaSerializationIsPinned(t *testing.T) {
	g := build(t, sampleContributions())
	g.GeneratedAt = time.Date(2026, 9, 20, 14, 22, 31, 0, time.UTC)
	g.Stats.DurationMs = 412

	out, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, filepath.Join("testdata", "golden", "graph-v0.1.json"), out)

	if g.SchemaVersion != schema.Version {
		t.Errorf("SchemaVersion = %q, want %q", g.SchemaVersion, schema.Version)
	}
}
