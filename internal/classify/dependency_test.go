package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Every ecosystem decorates a dependency name differently, and the graph is
// only as good as the table's ability to see through the decoration. These
// pin the normalization, which had no direct test: a regression here does not
// produce a wrong answer, it produces a missing one.

func TestFrameworkOf(t *testing.T) {
	tests := []struct {
		dep  string
		want string
	}{
		// Exact matches, one per ecosystem.
		{"django", "django"},
		{"express", "express"},
		{"github.com/gin-gonic/gin", "gin"},

		// A module path resolves like its parent, so a subpackage import
		// still names the framework.
		{"github.com/go-chi/chi/middleware", "chi"},
		{"@nestjs/core", "nestjs"},

		// Case and separator folding, which is how these appear in a real
		// requirements.txt.
		{"Flask", "flask"},
		{"FastAPI", "fastapi"},

		// Go's major-version suffix names the same module.
		{"github.com/go-chi/chi/v5", "chi"},
		{"github.com/labstack/echo/v4", "echo"},
	}
	for _, tt := range tests {
		t.Run(tt.dep, func(t *testing.T) {
			got, ok := classify.FrameworkOf(tt.dep)
			if !ok || got != tt.want {
				t.Errorf("FrameworkOf(%q) = (%q, %v), want (%q, true)", tt.dep, got, ok, tt.want)
			}
		})
	}

	// A library that is not a web framework must not be reported as one.
	for _, dep := range []string{"requests", "lodash", "github.com/lib/pq", "numpy", ""} {
		if got, ok := classify.FrameworkOf(dep); ok {
			t.Errorf("FrameworkOf(%q) = (%q, true), want not found", dep, got)
		}
	}
}

func TestDependencyNormalization(t *testing.T) {
	tests := []struct {
		name string
		dep  string
		tech string
	}{
		{"go major version suffix", "github.com/lib/pq/v2", "postgres"},
		{"go subpackage resolves like its parent", "github.com/jackc/pgx/v5/pgxpool", "postgres"},
		{"python extras are decoration", "psycopg[binary]", "postgres"},
		{"python extras on a queue client", "celery[redis]", "amqp"},
		{"case is folded for flat names", "PyMongo", "mongodb"},
		{"underscores fold to hyphens", "aws_sdk", "aws"},
		{"npm scope is part of the identity", "@aws-sdk/client-dynamodb", "dynamodb"},
		{"surrounding whitespace", "  redis  ", "redis"},
		{
			// A Go module path is left cased, because a vanity domain can be
			// mixed case and lowercasing it would miss the table entry.
			name: "mixed-case go module path", dep: "github.com/Azure/azure-sdk-for-go/sdk/azcore", tech: "azure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib, ok := classify.LibraryImplies(tt.dep)
			if !ok || lib.Tech != tt.tech {
				t.Errorf("LibraryImplies(%q) = (%q, %v), want (%q, true)", tt.dep, lib.Tech, ok, tt.tech)
			}
		})
	}
}

// The major-version trim only fires on digits. "/v" begins plenty of ordinary
// path elements, and trimming those would silently truncate a module path to
// something that matches the wrong table entry or nothing at all.
func TestOnlyADigitSuffixIsAMajorVersion(t *testing.T) {
	for _, dep := range []string{
		"github.com/acme/vendor",
		"github.com/acme/validator",
		"github.com/acme/v",
		"github.com/acme/v2x",
	} {
		if lib, ok := classify.LibraryImplies(dep); ok {
			t.Errorf("LibraryImplies(%q) = (%q, true); a non-numeric /v element was treated as a version", dep, lib.Tech)
		}
	}
	// The control: with digits, it is a version and the parent resolves.
	if lib, ok := classify.LibraryImplies("github.com/lib/pq/v10"); !ok || lib.Tech != "postgres" {
		t.Errorf("LibraryImplies(.../v10) = (%q, %v), want (postgres, true)", lib.Tech, ok)
	}
}

// A key ending in "-" or "/" marks a package family: every member names the
// same system, and spelling them all out would be a list nobody maintains.
func TestPackageFamiliesResolveToOneSystem(t *testing.T) {
	tests := []struct {
		dep  string
		tech string
	}{
		// The same SDK family in three ecosystems. Before these, whether a
		// service drew a GCP edge depended on the language it was written in.
		{"google-cloud-secret-manager", "gcp"},
		{"google-cloud-trace", "gcp"},
		{"google-cloud-alloydb-connector[asyncpg]", "gcp"},
		{"@google-cloud/profiler", "gcp"},
		{"@google-cloud/trace-agent", "gcp"},
		{"cloud.google.com/go/profiler", "gcp"},

		// psycopg distributes its binary build and its pool separately.
		{"psycopg-binary", "postgres"},
		{"psycopg-pool", "postgres"},
		{"psycopg2-binary", "postgres"},
	}
	for _, tt := range tests {
		t.Run(tt.dep, func(t *testing.T) {
			lib, ok := classify.LibraryImplies(tt.dep)
			if !ok || lib.Tech != tt.tech {
				t.Errorf("LibraryImplies(%q) = (%q, %v), want (%q, true)", tt.dep, lib.Tech, ok, tt.tech)
			}
		})
	}
}

// A more specific key wins, so naming one member of a family does not get
// flattened into the family's answer.
func TestAnExactKeyBeatsItsFamily(t *testing.T) {
	// A bucket is a datastore you persist to. The provider is an external
	// system you call. Collapsing the first into the second would lose the
	// only storage relationship the dependency states.
	lib, ok := classify.LibraryImplies("google-cloud-storage")
	if !ok || lib.Tech != "gcs" || lib.Kind != schema.KindDatastore {
		t.Fatalf("LibraryImplies(google-cloud-storage) = (%q, %s, %v), want (gcs, datastore, true)",
			lib.Tech, lib.Kind, ok)
	}
}

// The trailing separator is the opt-in, and it is the only thing standing
// between a lookup table and a guessing one. "pg" and "redis" are keys; if
// bare keys prefix-matched, a PGP library and a Redis-inspired search engine
// would both be reported as infrastructure this service talks to.
func TestBareKeysDoNotPrefixMatch(t *testing.T) {
	for _, dep := range []string{
		"pgp",
		"pgpy",
		"redisearch-fake",
		"mysqlish",
		"postgrest-py",
	} {
		if lib, ok := classify.LibraryImplies(dep); ok {
			t.Errorf("LibraryImplies(%q) = (%q, true); a bare key prefix-matched", dep, lib.Tech)
		}
	}
}
