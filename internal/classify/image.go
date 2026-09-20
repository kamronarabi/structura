// Package classify turns container image references and hostnames into graph
// node kinds and technology labels.
//
// A Compose service and a Kubernetes Deployment both declare "an image runs
// here" and nothing about what that image *is*. Recognizing that
// postgres:16 is a datastore rather than a service is what makes the
// difference between a diagram of identical boxes and one that reads like an
// architecture. The mapping is a lookup table because that is honest: it is
// pattern recognition over well-known images, not inference, and anything
// unrecognized stays a plain service rather than being guessed at.
package classify

import (
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// ImageRef is a parsed container image reference.
type ImageRef struct {
	// Registry is the host portion, "" when the reference is implicitly
	// Docker Hub.
	Registry string
	// Repository is the path without the registry, e.g. "acme/api".
	Repository string
	// Name is the final path element, e.g. "api". This is what the identity
	// index matches against.
	Name string
	// Tag is the version label, "" when none was given.
	Tag string
	// Digest is the content digest, "" when none was given.
	Digest string
}

// ParseImage splits a container image reference into its parts. It is lenient
// by design: a reference it cannot fully understand still yields a usable
// Name, and the caller gets something to match on rather than nothing.
func ParseImage(ref string) ImageRef {
	out := ImageRef{}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return out
	}

	if at := strings.Index(ref, "@"); at >= 0 {
		out.Digest = ref[at+1:]
		ref = ref[:at]
	}

	// A colon before the last slash is a registry port, not a tag.
	if colon := strings.LastIndex(ref, ":"); colon >= 0 && colon > strings.LastIndex(ref, "/") {
		out.Tag = ref[colon+1:]
		ref = ref[:colon]
	}

	// The first element is a registry only if it looks like a host: it
	// contains a dot or a colon, or is exactly "localhost".
	parts := strings.Split(ref, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		out.Registry = parts[0]
		parts = parts[1:]
	}
	out.Repository = strings.Join(parts, "/")
	if len(parts) > 0 {
		out.Name = parts[len(parts)-1]
	}
	return out
}

// knownImages maps a recognizable image name to what it is. Keys are matched
// against the final path element of a reference, so "bitnami/postgresql" and
// "postgresql" both resolve.
var knownImages = map[string]struct {
	kind schema.NodeKind
	tech string
}{
	// Relational
	"postgres":    {schema.KindDatastore, "postgresql"},
	"postgresql":  {schema.KindDatastore, "postgresql"},
	"timescaledb": {schema.KindDatastore, "postgresql"},
	"pgvector":    {schema.KindDatastore, "postgresql"},
	"mysql":       {schema.KindDatastore, "mysql"},
	"mariadb":     {schema.KindDatastore, "mysql"},
	"percona":     {schema.KindDatastore, "mysql"},
	"cockroachdb": {schema.KindDatastore, "cockroachdb"},
	"cockroach":   {schema.KindDatastore, "cockroachdb"},
	"yugabytedb":  {schema.KindDatastore, "yugabyte"},
	"mssql":       {schema.KindDatastore, "sqlserver"},
	"sqlserver":   {schema.KindDatastore, "sqlserver"},

	// Document, key-value, and cache
	"mongo":     {schema.KindDatastore, "mongodb"},
	"mongodb":   {schema.KindDatastore, "mongodb"},
	"redis":     {schema.KindDatastore, "redis"},
	"valkey":    {schema.KindDatastore, "redis"},
	"keydb":     {schema.KindDatastore, "redis"},
	"memcached": {schema.KindDatastore, "memcached"},
	"etcd":      {schema.KindDatastore, "etcd"},
	"consul":    {schema.KindDatastore, "consul"},
	"couchdb":   {schema.KindDatastore, "couchdb"},
	"couchbase": {schema.KindDatastore, "couchbase"},
	"dynamodb":  {schema.KindDatastore, "dynamodb"},
	"scylladb":  {schema.KindDatastore, "cassandra"},
	"scylla":    {schema.KindDatastore, "cassandra"},
	"cassandra": {schema.KindDatastore, "cassandra"},

	// Search and analytics
	"elasticsearch": {schema.KindDatastore, "elasticsearch"},
	"opensearch":    {schema.KindDatastore, "opensearch"},
	"solr":          {schema.KindDatastore, "solr"},
	"clickhouse":    {schema.KindDatastore, "clickhouse"},
	"druid":         {schema.KindDatastore, "druid"},
	"influxdb":      {schema.KindDatastore, "influxdb"},
	"questdb":       {schema.KindDatastore, "questdb"},
	"neo4j":         {schema.KindDatastore, "neo4j"},
	"typesense":     {schema.KindDatastore, "typesense"},
	"meilisearch":   {schema.KindDatastore, "meilisearch"},
	"qdrant":        {schema.KindDatastore, "qdrant"},
	"weaviate":      {schema.KindDatastore, "weaviate"},
	"milvus":        {schema.KindDatastore, "milvus"},
	"chroma":        {schema.KindDatastore, "chroma"},

	// Object storage
	"minio": {schema.KindDatastore, "s3"},
	"ceph":  {schema.KindDatastore, "ceph"},

	// Messaging
	"rabbitmq":  {schema.KindQueue, "amqp"},
	"kafka":     {schema.KindQueue, "kafka"},
	"redpanda":  {schema.KindQueue, "kafka"},
	"nats":      {schema.KindQueue, "nats"},
	"pulsar":    {schema.KindQueue, "pulsar"},
	"activemq":  {schema.KindQueue, "amqp"},
	"artemis":   {schema.KindQueue, "amqp"},
	"zookeeper": {schema.KindDatastore, "zookeeper"},
	"mosquitto": {schema.KindQueue, "mqtt"},
	"emqx":      {schema.KindQueue, "mqtt"},

	// Edge and infrastructure
	"nginx":      {schema.KindCloudResource, "nginx"},
	"traefik":    {schema.KindCloudResource, "traefik"},
	"haproxy":    {schema.KindCloudResource, "haproxy"},
	"envoy":      {schema.KindCloudResource, "envoy"},
	"caddy":      {schema.KindCloudResource, "caddy"},
	"kong":       {schema.KindCloudResource, "kong"},
	"varnish":    {schema.KindCloudResource, "varnish"},
	"prometheus": {schema.KindService, "prometheus"},
	"grafana":    {schema.KindService, "grafana"},
	"jaeger":     {schema.KindService, "jaeger"},
	"vault":      {schema.KindService, "vault"},
	"keycloak":   {schema.KindService, "keycloak"},
}

// ImageKind classifies an image reference. Anything unrecognized is a
// service: the conservative answer, and the one that is right far more often
// than not for a first-party image.
func ImageKind(ref string) (kind schema.NodeKind, tech string, recognized bool) {
	img := ParseImage(ref)
	if img.Name == "" {
		return schema.KindService, "", false
	}
	if known, ok := knownImages[strings.ToLower(img.Name)]; ok {
		return known.kind, known.tech, true
	}
	// Publishers decorate the product name in predictable ways: Bitnami and
	// Confluent prefix it, and many images append a base-image variant.
	// Stripping those is what lets confluentinc/cp-kafka and
	// bitnami/postgresql resolve to the same products as kafka and postgres.
	base := strings.ToLower(img.Name)
	for _, prefix := range []string{"cp-", "docker-", "library-", "os-"} {
		if trimmed, ok := strings.CutPrefix(base, prefix); ok {
			if known, ok := knownImages[trimmed]; ok {
				return known.kind, known.tech, true
			}
			base = trimmed
			break
		}
	}
	for _, suffix := range []string{"-alpine", "-slim", "-bookworm", "-bullseye", "-server", "-oss", "-ce"} {
		if trimmed, ok := strings.CutSuffix(base, suffix); ok {
			if known, ok := knownImages[trimmed]; ok {
				return known.kind, known.tech, true
			}
		}
	}
	return schema.KindService, "", false
}
