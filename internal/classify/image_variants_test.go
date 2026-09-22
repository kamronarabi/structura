package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Publishers decorate a product name, and the decoration is what stands
// between "a Kafka broker" and "an unidentified service". These branches had
// no coverage, so a repository that used Confluent's images rather than
// Apache's would have quietly produced a diagram of plain boxes.

func TestPublisherPrefixesAreStripped(t *testing.T) {
	tests := []struct {
		ref  string
		kind schema.NodeKind
		tech string
	}{
		{"confluentinc/cp-kafka:7.5.0", schema.KindQueue, "kafka"},
		{"confluentinc/cp-zookeeper:7.5.0", schema.KindDatastore, "zookeeper"},
		{"docker-redis", schema.KindDatastore, "redis"},
		{"library-postgres", schema.KindDatastore, "postgresql"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			kind, tech, ok := classify.ImageKind(tt.ref)
			if !ok || kind != tt.kind || tech != tt.tech {
				t.Errorf("ImageKind(%q) = (%s, %q, %v), want (%s, %q, true)",
					tt.ref, kind, tech, ok, tt.kind, tt.tech)
			}
		})
	}
}

func TestBaseImageSuffixesAreStripped(t *testing.T) {
	tests := []struct {
		ref  string
		kind schema.NodeKind
		tech string
	}{
		{"postgres-alpine", schema.KindDatastore, "postgresql"},
		{"redis-slim", schema.KindDatastore, "redis"},
		{"cassandra-bookworm", schema.KindDatastore, "cassandra"},
		{"opensearch-oss", schema.KindDatastore, "opensearch"},
		{"mysql-server", schema.KindDatastore, "mysql"},
		// mariadb is a MySQL-compatible product and reports the protocol a
		// caller would speak to it, not the brand on the box.
		{"mariadb-server", schema.KindDatastore, "mysql"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			kind, tech, ok := classify.ImageKind(tt.ref)
			if !ok || kind != tt.kind || tech != tt.tech {
				t.Errorf("ImageKind(%q) = (%s, %q, %v), want (%s, %q, true)",
					tt.ref, kind, tech, ok, tt.kind, tt.tech)
			}
		})
	}
}

// Unrecognized is the common case and has to stay boring: a first-party image
// is a service, and guessing anything else would put a made-up datastore in
// someone's architecture diagram.
func TestUnrecognizedImagesStayPlainServices(t *testing.T) {
	for _, ref := range []string{
		"acme/api:1.0",
		"registry.example.com:5000/team/worker@sha256:abc123",
		"ghcr.io/acme/billing:v2",
		// Confluent Server is a real image, and stripping its "cp-" prefix
		// leaves "server", which names no product. It stays a service. A
		// Kafka distribution arguably deserves better, but inventing the
		// answer from a prefix that happened to match is how a table starts
		// guessing.
		"confluentinc/cp-server:7.5.0",
		"",
		"   ",
	} {
		kind, tech, ok := classify.ImageKind(ref)
		if ok || kind != schema.KindService || tech != "" {
			t.Errorf("ImageKind(%q) = (%s, %q, %v), want (service, \"\", false)", ref, kind, tech, ok)
		}
	}
}
