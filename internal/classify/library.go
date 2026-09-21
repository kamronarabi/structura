package classify

import (
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// A dependency list is mostly noise for architectural purposes. A Go service
// with forty modules in go.mod does not have forty architectural
// relationships, and emitting a node per dependency would bury the six things
// that matter under logging libraries and test helpers — while making the
// graph far too large to hand to a model, which is the whole point of
// building one.
//
// A small number of dependencies are different: a Postgres driver means the
// service talks to Postgres, an AWS SDK means it talks to AWS. Those are
// architecture, stated in a file that is easier to read than any manifest.
// This table is that subset, and nothing outside it produces a node.

// Library is what depending on a package implies.
type Library struct {
	// Tech is the technology the dependency indicates, matching the names
	// used for datastore and queue protocols elsewhere.
	Tech string
	// Kind is what the implied component would be.
	Kind schema.NodeKind
	// Edge is the relationship the dependency implies.
	Edge schema.EdgeKind
}

// Vendor reports whether the technology names one company rather than a kind
// of thing.
//
// The distinction decides whether a dependency can put a component on the
// diagram by itself. A Postgres driver says this service talks to a Postgres
// and never which one, so it can only corroborate an instance something else
// found -- inventing "a Postgres" would be a box standing for nothing. The
// Supabase client says it talks to Supabase, and there is exactly one
// Supabase: the same standing as the api.stripe.com the resolver already
// synthesizes from a URL, and enough to draw.
//
// Which project, tenant, or region remains unknown, so what it draws is the
// weakest edge that still asserts something.
func (l Library) Vendor() bool { return vendorTech[l.Tech] }

// vendorTech is the set of technologies that are a company. It is keyed by
// technology rather than by package so that every client for one service --
// the JavaScript SDK, the Python SDK, the Go module -- agrees without the
// fact being repeated.
var vendorTech = map[string]bool{
	// Payments, messaging, and error reporting.
	"stripe": true, "twilio": true, "sendgrid": true, "sentry": true,
	"resend": true, "postmark": true, "mailgun": true,
	"slack": true, "pusher": true, "ably": true,

	// Managed data platforms. Self-hostable products are deliberately absent:
	// Meilisearch, Typesense, Qdrant, Weaviate and Chroma name software that
	// may be running in the cluster next door, so they stay a technology that
	// an instance can corroborate rather than a company to draw.
	"supabase": true, "firebase": true, "neon": true, "planetscale": true,
	"vercel-postgres": true, "vercel-kv": true, "vercel-blob": true,
	"upstash": true, "turso": true, "xata": true, "fauna": true, "convex": true,

	// Identity.
	"clerk": true, "auth0": true, "workos": true, "okta": true, "descope": true,

	// Analytics and observability.
	"algolia": true, "posthog": true, "segment": true, "mixpanel": true,
	"amplitude": true, "launchdarkly": true, "datadog": true, "newrelic": true,
	"honeycomb": true, "betterstack": true,

	// Model providers and managed vector stores.
	"openai": true, "anthropic": true, "google-ai": true, "cohere": true,
	"replicate": true, "mistral": true, "pinecone": true,

	// Cloud providers already name a company; the SDK says which one.
	"aws": true, "gcp": true, "azure": true,
}

// libraries maps a dependency name to the infrastructure it implies. Keys are
// matched as prefixes, so a module path covers its subpackages.
var libraries = map[string]Library{
	// Postgres
	"github.com/lib/pq":    {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"github.com/jackc/pgx": {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"psycopg2":             {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"psycopg":              {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"asyncpg":              {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"pg":                   {"postgres", schema.KindDatastore, schema.EdgePersistsTo},
	"postgres":             {"postgres", schema.KindDatastore, schema.EdgePersistsTo},

	// MySQL
	"github.com/go-sql-driver/mysql": {"mysql", schema.KindDatastore, schema.EdgePersistsTo},
	"mysqlclient":                    {"mysql", schema.KindDatastore, schema.EdgePersistsTo},
	"pymysql":                        {"mysql", schema.KindDatastore, schema.EdgePersistsTo},
	"mysql2":                         {"mysql", schema.KindDatastore, schema.EdgePersistsTo},

	// Redis
	"github.com/redis/go-redis":  {"redis", schema.KindDatastore, schema.EdgePersistsTo},
	"github.com/go-redis/redis":  {"redis", schema.KindDatastore, schema.EdgePersistsTo},
	"github.com/gomodule/redigo": {"redis", schema.KindDatastore, schema.EdgePersistsTo},
	"redis":                      {"redis", schema.KindDatastore, schema.EdgePersistsTo},
	"ioredis":                    {"redis", schema.KindDatastore, schema.EdgePersistsTo},

	// MongoDB
	"go.mongodb.org/mongo-driver": {"mongodb", schema.KindDatastore, schema.EdgePersistsTo},
	"pymongo":                     {"mongodb", schema.KindDatastore, schema.EdgePersistsTo},
	"motor":                       {"mongodb", schema.KindDatastore, schema.EdgePersistsTo},
	"mongoose":                    {"mongodb", schema.KindDatastore, schema.EdgePersistsTo},
	"mongodb":                     {"mongodb", schema.KindDatastore, schema.EdgePersistsTo},

	// Search and analytics
	"github.com/elastic/go-elasticsearch": {"elasticsearch", schema.KindDatastore, schema.EdgePersistsTo},
	"elasticsearch":                       {"elasticsearch", schema.KindDatastore, schema.EdgePersistsTo},
	"opensearch-py":                       {"opensearch", schema.KindDatastore, schema.EdgePersistsTo},
	"clickhouse-driver":                   {"clickhouse", schema.KindDatastore, schema.EdgePersistsTo},

	// Cassandra
	"github.com/gocql/gocql": {"cassandra", schema.KindDatastore, schema.EdgePersistsTo},
	"cassandra-driver":       {"cassandra", schema.KindDatastore, schema.EdgePersistsTo},

	// Messaging
	"github.com/segmentio/kafka-go":              {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/confluentinc/confluent-kafka-go": {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/IBM/sarama":                      {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/Shopify/sarama":                  {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"kafka-python":                               {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"confluent-kafka":                            {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"kafkajs":                                    {"kafka", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/rabbitmq/amqp091-go":             {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/streadway/amqp":                  {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"pika":                                       {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"amqplib":                                    {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"kombu":                                      {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"celery":                                     {"amqp", schema.KindQueue, schema.EdgePublishesTo},
	"github.com/nats-io/nats.go":                 {"nats", schema.KindQueue, schema.EdgePublishesTo},
	"nats-py":                                    {"nats", schema.KindQueue, schema.EdgePublishesTo},

	// Cloud SDKs. These name a provider rather than one service, so the
	// resolver treats the result as an external boundary.
	"github.com/aws/aws-sdk-go":         {"aws", schema.KindExternal, schema.EdgeCalls},
	"boto3":                             {"aws", schema.KindExternal, schema.EdgeCalls},
	"aws-sdk":                           {"aws", schema.KindExternal, schema.EdgeCalls},
	"@aws-sdk/client-s3":                {"s3", schema.KindDatastore, schema.EdgePersistsTo},
	"@aws-sdk/client-sqs":               {"sqs", schema.KindQueue, schema.EdgePublishesTo},
	"@aws-sdk/client-dynamodb":          {"dynamodb", schema.KindDatastore, schema.EdgePersistsTo},
	"cloud.google.com/go":               {"gcp", schema.KindExternal, schema.EdgeCalls},
	"google-cloud-storage":              {"gcs", schema.KindDatastore, schema.EdgePersistsTo},
	"github.com/Azure/azure-sdk-for-go": {"azure", schema.KindExternal, schema.EdgeCalls},
	"azure-storage-blob":                {"azure", schema.KindExternal, schema.EdgeCalls},

	// Third-party APIs that are architecture in their own right.
	"github.com/stripe/stripe-go": {"stripe", schema.KindExternal, schema.EdgeCalls},
	"stripe":                      {"stripe", schema.KindExternal, schema.EdgeCalls},
	"twilio":                      {"twilio", schema.KindExternal, schema.EdgeCalls},
	"sendgrid":                    {"sendgrid", schema.KindExternal, schema.EdgeCalls},
	"@sendgrid/":                  {"sendgrid", schema.KindExternal, schema.EdgeCalls},
	"@sentry/":                    {"sentry", schema.KindExternal, schema.EdgeCalls},
	"sentry-sdk":                  {"sentry", schema.KindExternal, schema.EdgeCalls},

	// Managed backends. An application deployed to a managed platform keeps
	// most of its architecture here rather than in any manifest: there is no
	// container to inspect and no connection string to read, only a
	// dependency and a dashboard. Without these the scan sees a React app
	// and nothing it talks to.
	//
	// A scope prefix covers a vendor's whole family -- @supabase/supabase-js
	// and @supabase/auth-helpers-nextjs are one service -- so a new package
	// from a known vendor needs no entry.
	"@supabase/": {"supabase", schema.KindExternal, schema.EdgeCalls},
	"supabase":   {"supabase", schema.KindExternal, schema.EdgeCalls},
	"github.com/supabase-community/supabase-go": {"supabase", schema.KindExternal, schema.EdgeCalls},
	"firebase":               {"firebase", schema.KindExternal, schema.EdgeCalls},
	"firebase-admin":         {"firebase", schema.KindExternal, schema.EdgeCalls},
	"@firebase/":             {"firebase", schema.KindExternal, schema.EdgeCalls},
	"firebase.google.com/go": {"firebase", schema.KindExternal, schema.EdgeCalls},
	"@neondatabase/":         {"neon", schema.KindExternal, schema.EdgePersistsTo},
	"@planetscale/":          {"planetscale", schema.KindExternal, schema.EdgePersistsTo},
	"@vercel/postgres":       {"vercel-postgres", schema.KindExternal, schema.EdgePersistsTo},
	"@vercel/kv":             {"vercel-kv", schema.KindExternal, schema.EdgePersistsTo},
	"@vercel/blob":           {"vercel-blob", schema.KindExternal, schema.EdgePersistsTo},
	"@upstash/":              {"upstash", schema.KindExternal, schema.EdgePersistsTo},
	"@libsql/":               {"turso", schema.KindExternal, schema.EdgePersistsTo},
	"@xata.io/client":        {"xata", schema.KindExternal, schema.EdgePersistsTo},
	"faunadb":                {"fauna", schema.KindExternal, schema.EdgePersistsTo},
	"@convex-dev/":           {"convex", schema.KindExternal, schema.EdgePersistsTo},
	"convex":                 {"convex", schema.KindExternal, schema.EdgePersistsTo},

	// Identity. A sign-in provider is on the request path of nearly every
	// page, and none of it appears in infrastructure configuration.
	"@clerk/":                       {"clerk", schema.KindExternal, schema.EdgeCalls},
	"clerk-backend-api":             {"clerk", schema.KindExternal, schema.EdgeCalls},
	"github.com/clerk/clerk-sdk-go": {"clerk", schema.KindExternal, schema.EdgeCalls},
	"@auth0/":                       {"auth0", schema.KindExternal, schema.EdgeCalls},
	"auth0":                         {"auth0", schema.KindExternal, schema.EdgeCalls},
	"@workos-inc/":                  {"workos", schema.KindExternal, schema.EdgeCalls},
	"@okta/":                        {"okta", schema.KindExternal, schema.EdgeCalls},
	"@descope/":                     {"descope", schema.KindExternal, schema.EdgeCalls},

	// Email and messaging.
	"resend":     {"resend", schema.KindExternal, schema.EdgeCalls},
	"postmark":   {"postmark", schema.KindExternal, schema.EdgeCalls},
	"mailgun.js": {"mailgun", schema.KindExternal, schema.EdgeCalls},
	"@slack/":    {"slack", schema.KindExternal, schema.EdgeCalls},
	"slack-sdk":  {"slack", schema.KindExternal, schema.EdgeCalls},
	"@pusher/":   {"pusher", schema.KindExternal, schema.EdgeCalls},
	"pusher":     {"pusher", schema.KindExternal, schema.EdgeCalls},
	"ably":       {"ably", schema.KindExternal, schema.EdgeCalls},

	// Search, analytics, and product telemetry.
	"algoliasearch":                {"algolia", schema.KindExternal, schema.EdgeCalls},
	"@algolia/":                    {"algolia", schema.KindExternal, schema.EdgeCalls},
	"typesense":                    {"typesense", schema.KindDatastore, schema.EdgePersistsTo},
	"meilisearch":                  {"meilisearch", schema.KindDatastore, schema.EdgePersistsTo},
	"posthog-js":                   {"posthog", schema.KindExternal, schema.EdgeCalls},
	"posthog-node":                 {"posthog", schema.KindExternal, schema.EdgeCalls},
	"posthog":                      {"posthog", schema.KindExternal, schema.EdgeCalls},
	"@segment/":                    {"segment", schema.KindExternal, schema.EdgeCalls},
	"analytics-node":               {"segment", schema.KindExternal, schema.EdgeCalls},
	"mixpanel":                     {"mixpanel", schema.KindExternal, schema.EdgeCalls},
	"@amplitude/":                  {"amplitude", schema.KindExternal, schema.EdgeCalls},
	"launchdarkly-node-server-sdk": {"launchdarkly", schema.KindExternal, schema.EdgeCalls},
	"@launchdarkly/":               {"launchdarkly", schema.KindExternal, schema.EdgeCalls},

	// Observability.
	"dd-trace":           {"datadog", schema.KindExternal, schema.EdgeCalls},
	"datadog-api-client": {"datadog", schema.KindExternal, schema.EdgeCalls},
	"ddtrace":            {"datadog", schema.KindExternal, schema.EdgeCalls},
	"newrelic":           {"newrelic", schema.KindExternal, schema.EdgeCalls},
	"@honeycombio/":      {"honeycomb", schema.KindExternal, schema.EdgeCalls},
	"@logtail/":          {"betterstack", schema.KindExternal, schema.EdgeCalls},

	// Model providers and vector stores. A service that calls a model has a
	// dependency on somebody else's capacity, latency, and pricing, which is
	// architecture by any definition that matters during an incident.
	"openai":                {"openai", schema.KindExternal, schema.EdgeCalls},
	"@anthropic-ai/":        {"anthropic", schema.KindExternal, schema.EdgeCalls},
	"anthropic":             {"anthropic", schema.KindExternal, schema.EdgeCalls},
	"@google/generative-ai": {"google-ai", schema.KindExternal, schema.EdgeCalls},
	"google-generativeai":   {"google-ai", schema.KindExternal, schema.EdgeCalls},
	"cohere-ai":             {"cohere", schema.KindExternal, schema.EdgeCalls},
	"replicate":             {"replicate", schema.KindExternal, schema.EdgeCalls},
	"@mistralai/":           {"mistral", schema.KindExternal, schema.EdgeCalls},
	"@pinecone-database/":   {"pinecone", schema.KindExternal, schema.EdgePersistsTo},
	"pinecone-client":       {"pinecone", schema.KindExternal, schema.EdgePersistsTo},
	"weaviate-ts-client":    {"weaviate", schema.KindDatastore, schema.EdgePersistsTo},
	"weaviate-client":       {"weaviate", schema.KindDatastore, schema.EdgePersistsTo},
	"chromadb":              {"chroma", schema.KindDatastore, schema.EdgePersistsTo},
	"@qdrant/":              {"qdrant", schema.KindDatastore, schema.EdgePersistsTo},
	"qdrant-client":         {"qdrant", schema.KindDatastore, schema.EdgePersistsTo},
}

// frameworks maps a dependency to the web framework it indicates. This does
// not create a node — a framework is not a component — but it labels the
// service, which is what a reader wants to know about it.
var frameworks = map[string]string{
	"github.com/gin-gonic/gin":      "gin",
	"github.com/labstack/echo":      "echo",
	"github.com/gofiber/fiber":      "fiber",
	"github.com/go-chi/chi":         "chi",
	"github.com/gorilla/mux":        "gorilla",
	"google.golang.org/grpc":        "grpc",
	"github.com/graphql-go/graphql": "graphql",
	"express":                       "express",
	"fastify":                       "fastify",
	"@nestjs/core":                  "nestjs",
	"next":                          "nextjs",
	"react":                         "react",
	"vue":                           "vue",
	"svelte":                        "svelte",
	"@angular/core":                 "angular",
	"koa":                           "koa",
	"hapi":                          "hapi",
	"apollo-server":                 "apollo",
	"django":                        "django",
	"flask":                         "flask",
	"fastapi":                       "fastapi",
	"starlette":                     "starlette",
	"tornado":                       "tornado",
	"pyramid":                       "pyramid",
	"sanic":                         "sanic",
}

// LibraryImplies reports the infrastructure a dependency indicates.
func LibraryImplies(dependency string) (lib Library, ok bool) {
	name := normalizeDependency(dependency)
	if lib, ok := libraries[name]; ok {
		return lib, true
	}
	// Module paths carry a version suffix and subpackages; match on the
	// longest registered prefix so that a submodule resolves like its parent.
	best, bestLen := Library{}, 0
	for prefix, lib := range libraries {
		if !strings.Contains(prefix, "/") {
			continue
		}
		if strings.HasPrefix(name, prefix) && len(prefix) > bestLen {
			best, bestLen = lib, len(prefix)
		}
	}
	return best, bestLen > 0
}

// VendorTechnologies lists the technologies that name a company, sorted. It
// exists so a test can check the set against the table rather than trusting
// that the two were kept in step.
func VendorTechnologies() []string {
	out := make([]string, 0, len(vendorTech))
	for tech := range vendorTech {
		out = append(out, tech)
	}
	sort.Strings(out)
	return out
}

// FrameworkOf reports the web framework a dependency indicates.
func FrameworkOf(dependency string) (framework string, ok bool) {
	name := normalizeDependency(dependency)
	if fw, ok := frameworks[name]; ok {
		return fw, true
	}
	best, bestLen := "", 0
	for prefix, fw := range frameworks {
		if strings.Contains(prefix, "/") && strings.HasPrefix(name, prefix) && len(prefix) > bestLen {
			best, bestLen = fw, len(prefix)
		}
	}
	return best, bestLen > 0
}

// normalizeDependency strips the decorations each ecosystem adds: Go's major
// version suffix, Python's extras and case folding, npm's scope is kept
// because it is part of the identity.
func normalizeDependency(dep string) string {
	dep = strings.TrimSpace(dep)
	// Python extras: "celery[redis]" is still celery.
	if i := strings.Index(dep, "["); i > 0 {
		dep = dep[:i]
	}
	// Go major-version suffix: ".../v5" is the same module as "...".
	if i := strings.LastIndex(dep, "/v"); i > 0 {
		if rest := dep[i+2:]; rest != "" && isAllDigits(rest) {
			dep = dep[:i]
		}
	}
	// Python normalizes underscores and dots to hyphens and is case
	// insensitive; Go module paths are neither, but lowercasing a Go path
	// only risks a miss on a mixed-case vanity domain.
	if !strings.Contains(dep, "/") || strings.HasPrefix(dep, "@") {
		dep = strings.ToLower(strings.ReplaceAll(dep, "_", "-"))
	}
	return dep
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
