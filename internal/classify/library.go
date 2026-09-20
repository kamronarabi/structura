package classify

import (
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
	"@sentry/node":                {"sentry", schema.KindExternal, schema.EdgeCalls},
	"sentry-sdk":                  {"sentry", schema.KindExternal, schema.EdgeCalls},
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
