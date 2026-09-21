package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/pkg/schema"
)

func TestManagedBackendsAreRecognized(t *testing.T) {
	tests := []struct {
		dependency string
		tech       string
		kind       schema.NodeKind
		edge       schema.EdgeKind
	}{
		{"@supabase/supabase-js", "supabase", schema.KindExternal, schema.EdgeCalls},
		{"firebase", "firebase", schema.KindExternal, schema.EdgeCalls},
		{"firebase-admin", "firebase", schema.KindExternal, schema.EdgeCalls},
		{"@clerk/nextjs", "clerk", schema.KindExternal, schema.EdgeCalls},
		{"@auth0/nextjs-auth0", "auth0", schema.KindExternal, schema.EdgeCalls},
		{"@upstash/redis", "upstash", schema.KindExternal, schema.EdgePersistsTo},
		{"@neondatabase/serverless", "neon", schema.KindExternal, schema.EdgePersistsTo},
		{"@planetscale/database", "planetscale", schema.KindExternal, schema.EdgePersistsTo},
		{"@vercel/postgres", "vercel-postgres", schema.KindExternal, schema.EdgePersistsTo},
		{"resend", "resend", schema.KindExternal, schema.EdgeCalls},
		{"openai", "openai", schema.KindExternal, schema.EdgeCalls},
		{"@anthropic-ai/sdk", "anthropic", schema.KindExternal, schema.EdgeCalls},
		{"@pinecone-database/pinecone", "pinecone", schema.KindExternal, schema.EdgePersistsTo},
		{"posthog-js", "posthog", schema.KindExternal, schema.EdgeCalls},
		{"dd-trace", "datadog", schema.KindExternal, schema.EdgeCalls},
		// The drivers that were always there must keep working.
		{"pg", "postgres", schema.KindDatastore, schema.EdgePersistsTo},
		{"ioredis", "redis", schema.KindDatastore, schema.EdgePersistsTo},
	}

	for _, tt := range tests {
		lib, ok := classify.LibraryImplies(tt.dependency)
		if !ok {
			t.Errorf("LibraryImplies(%q) = not found", tt.dependency)
			continue
		}
		if lib.Tech != tt.tech || lib.Kind != tt.kind || lib.Edge != tt.edge {
			t.Errorf("LibraryImplies(%q) = %+v, want tech=%s kind=%s edge=%s",
				tt.dependency, lib, tt.tech, tt.kind, tt.edge)
		}
	}
}

// A scope prefix covers a vendor's whole family, so a package nobody has
// added to the table still resolves.
func TestVendorScopePrefixCoversTheFamily(t *testing.T) {
	for _, dep := range []string{
		"@supabase/auth-helpers-nextjs",
		"@supabase/ssr",
		"@clerk/clerk-sdk-node",
		"@clerk/backend",
		"@upstash/ratelimit",
		"@sentry/nextjs",
		"@anthropic-ai/bedrock-sdk",
	} {
		if _, ok := classify.LibraryImplies(dep); !ok {
			t.Errorf("LibraryImplies(%q) = not found; the scope prefix should cover it", dep)
		}
	}
}

// The distinction that decides whether a dependency can draw a component: a
// company can, a kind of thing cannot.
func TestVendorMarksCompaniesNotTechnologies(t *testing.T) {
	companies := []string{
		"@supabase/supabase-js", "firebase", "@clerk/nextjs", "@auth0/auth0-react",
		"@upstash/redis", "stripe", "openai", "resend", "@pinecone-database/pinecone",
	}
	for _, dep := range companies {
		lib, ok := classify.LibraryImplies(dep)
		if !ok {
			t.Fatalf("LibraryImplies(%q) = not found", dep)
		}
		if !lib.Vendor() {
			t.Errorf("%q implies %q, which should be a vendor", dep, lib.Tech)
		}
	}

	// A driver names a kind of thing. Which Postgres is unknown, so it may
	// only corroborate an instance something else found.
	technologies := []string{"pg", "ioredis", "mongoose", "psycopg2", "kafkajs"}
	for _, dep := range technologies {
		lib, ok := classify.LibraryImplies(dep)
		if !ok {
			t.Fatalf("LibraryImplies(%q) = not found", dep)
		}
		if lib.Vendor() {
			t.Errorf("%q implies %q, which names a technology, not a company", dep, lib.Tech)
		}
	}
}

// Self-hostable products are a technology, not a company: the instance may be
// running in the cluster next door, and drawing a vendor for it would assert a
// third party that is not there.
func TestSelfHostableProductsAreNotVendors(t *testing.T) {
	for _, dep := range []string{"meilisearch", "typesense", "chromadb", "weaviate-ts-client", "@qdrant/js-client-rest"} {
		lib, ok := classify.LibraryImplies(dep)
		if !ok {
			t.Fatalf("LibraryImplies(%q) = not found", dep)
		}
		if lib.Vendor() {
			t.Errorf("%q implies %q, which can be self-hosted and must not be drawn as a company",
				dep, lib.Tech)
		}
	}
}

// Every technology marked a vendor has to be reachable from some dependency,
// or the set has drifted from the table it describes.
func TestEveryVendorTechIsImpliedBySomeDependency(t *testing.T) {
	reached := map[string]bool{}
	for _, dep := range []string{
		"stripe", "twilio", "sendgrid", "@sentry/node", "resend", "postmark",
		"mailgun.js", "@slack/web-api", "pusher", "ably",
		"@supabase/supabase-js", "firebase", "@neondatabase/serverless",
		"@planetscale/database", "@vercel/postgres", "@vercel/kv", "@vercel/blob",
		"@upstash/redis", "@libsql/client", "@xata.io/client", "faunadb", "convex",
		"@clerk/nextjs", "@auth0/auth0-react", "@workos-inc/node", "@okta/okta-sdk-nodejs",
		"@descope/node-sdk", "algoliasearch", "posthog-js", "@segment/analytics-node",
		"mixpanel", "@amplitude/analytics-node", "@launchdarkly/node-server-sdk",
		"dd-trace", "newrelic", "@honeycombio/opentelemetry-sdk", "@logtail/node",
		"openai", "@anthropic-ai/sdk", "@google/generative-ai", "cohere-ai",
		"replicate", "@mistralai/mistralai", "@pinecone-database/pinecone",
		"aws-sdk", "cloud.google.com/go", "azure-storage-blob",
	} {
		if lib, ok := classify.LibraryImplies(dep); ok {
			reached[lib.Tech] = true
		}
	}

	for _, tech := range classify.VendorTechnologies() {
		if !reached[tech] {
			t.Errorf("%q is marked a vendor but no dependency in the table implies it; "+
				"either add the package or drop the entry", tech)
		}
	}
}

// A dependency outside the table must stay out of the graph. The table exists
// because a service with forty dependencies does not have forty architectural
// relationships.
func TestOrdinaryDependenciesImplyNothing(t *testing.T) {
	for _, dep := range []string{
		"lodash", "zod", "typescript", "eslint", "tailwindcss", "vite",
		"@types/node", "clsx", "date-fns", "github.com/spf13/cobra",
	} {
		if lib, ok := classify.LibraryImplies(dep); ok {
			t.Errorf("LibraryImplies(%q) = %+v, want nothing", dep, lib)
		}
	}
}
