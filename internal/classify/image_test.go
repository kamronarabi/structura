package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/pkg/schema"
)

func TestParseImage(t *testing.T) {
	tests := []struct {
		ref                        string
		registry, repository, name string
		tag, digest                string
	}{
		{
			ref:        "postgres:16",
			repository: "postgres", name: "postgres", tag: "16",
		},
		{
			ref:      "ghcr.io/acme/api:1.2.0",
			registry: "ghcr.io", repository: "acme/api", name: "api", tag: "1.2.0",
		},
		{
			ref:        "bitnami/postgresql:16.2.0-debian-12-r8",
			repository: "bitnami/postgresql", name: "postgresql", tag: "16.2.0-debian-12-r8",
		},
		{
			// A colon before the last slash is a registry port, not a tag.
			ref:      "registry.internal:5000/team/svc:v2",
			registry: "registry.internal:5000", repository: "team/svc", name: "svc", tag: "v2",
		},
		{
			ref:        "acme/api@sha256:9f86d081884c7d659a2feaa0c55ad015",
			repository: "acme/api", name: "api", digest: "sha256:9f86d081884c7d659a2feaa0c55ad015",
		},
		{
			ref:      "localhost/dev:latest",
			registry: "localhost", repository: "dev", name: "dev", tag: "latest",
		},
		{
			ref:        "redis",
			repository: "redis", name: "redis",
		},
		{ref: ""},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got := classify.ParseImage(tt.ref)
			if got.Registry != tt.registry {
				t.Errorf("Registry = %q, want %q", got.Registry, tt.registry)
			}
			if got.Repository != tt.repository {
				t.Errorf("Repository = %q, want %q", got.Repository, tt.repository)
			}
			if got.Name != tt.name {
				t.Errorf("Name = %q, want %q", got.Name, tt.name)
			}
			if got.Tag != tt.tag {
				t.Errorf("Tag = %q, want %q", got.Tag, tt.tag)
			}
			if got.Digest != tt.digest {
				t.Errorf("Digest = %q, want %q", got.Digest, tt.digest)
			}
		})
	}
}

func TestImageKind(t *testing.T) {
	tests := []struct {
		ref            string
		kind           schema.NodeKind
		wantRecognized bool
	}{
		{"postgres:16", schema.KindDatastore, true},
		{"postgres:16-alpine", schema.KindDatastore, true},
		{"bitnami/postgresql:16", schema.KindDatastore, true},
		{"redis:7-alpine", schema.KindDatastore, true},
		{"mongo:7", schema.KindDatastore, true},
		{"elasticsearch:8.13.0", schema.KindDatastore, true},
		{"minio/minio:latest", schema.KindDatastore, true},
		{"rabbitmq:3-management", schema.KindQueue, true},
		{"confluentinc/cp-kafka:7.5.0", schema.KindQueue, true},
		{"confluentinc/cp-zookeeper:7.5.0", schema.KindDatastore, true},
		{"bitnami/kafka:3.7", schema.KindQueue, true},
		{"nats:2-alpine", schema.KindQueue, true},
		{"nginx:1.27", schema.KindCloudResource, true},
		{"traefik:v3", schema.KindCloudResource, true},

		// A first-party image is a service, which is the conservative answer
		// and the right one far more often than not.
		{"ghcr.io/acme/checkout:1.0", schema.KindService, false},
		{"acme/api", schema.KindService, false},
		{"", schema.KindService, false},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			kind, _, recognized := classify.ImageKind(tt.ref)
			if kind != tt.kind {
				t.Errorf("ImageKind(%q) = %q, want %q", tt.ref, kind, tt.kind)
			}
			if recognized != tt.wantRecognized {
				t.Errorf("recognized = %v, want %v", recognized, tt.wantRecognized)
			}
		})
	}
}

func TestImageKindIsCaseInsensitive(t *testing.T) {
	kind, _, _ := classify.ImageKind("PostgreSQL:16")
	if kind != schema.KindDatastore {
		t.Errorf("ImageKind() = %q for a differently-cased name, want datastore", kind)
	}
}

// Image references come out of user-controlled YAML.
func FuzzParseImage(f *testing.F) {
	for _, s := range []string{
		"postgres:16", "ghcr.io/a/b:c@sha256:d", ":::", "///", "",
		"a:b:c:d", "@@@", "localhost:5000/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		img := classify.ParseImage(ref)
		// Name is what the identity index matches on, so it must never
		// contain a separator that would make it match the wrong thing.
		if img.Name != "" {
			for _, r := range img.Name {
				if r == '/' || r == '@' {
					t.Fatalf("ParseImage(%q).Name = %q contains a separator", ref, img.Name)
				}
			}
		}
		kind, _, _ := classify.ImageKind(ref)
		if !kind.Valid() {
			t.Fatalf("ImageKind(%q) = %q, which is not a valid node kind", ref, kind)
		}
	})
}
