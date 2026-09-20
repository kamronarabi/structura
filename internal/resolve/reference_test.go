package resolve_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

func TestParseValueRecognizesReferences(t *testing.T) {
	tests := []struct {
		name       string
		key, value string
		wantKind   resolve.HintKind
		wantToken  string
		wantPort   int
		wantProto  string
		wantEdge   schema.EdgeKind
	}{
		{
			name: "postgres connection string",
			key:  "DATABASE_URL", value: "postgres://app:pw@db:5432/orders",
			wantKind: resolve.HintConnString, wantToken: "db", wantPort: 5432,
			wantProto: "postgres", wantEdge: schema.EdgePersistsTo,
		},
		{
			name: "redis url without an explicit port",
			key:  "REDIS_URL", value: "redis://cache",
			wantKind: resolve.HintConnString, wantToken: "cache", wantPort: 6379,
			wantProto: "redis", wantEdge: schema.EdgePersistsTo,
		},
		{
			name: "amqp implies publishing",
			key:  "BROKER_URL", value: "amqp://queue:5672",
			wantKind: resolve.HintConnString, wantToken: "queue", wantPort: 5672,
			wantProto: "amqp", wantEdge: schema.EdgePublishesTo,
		},
		{
			name: "http url is a call",
			key:  "USER_SVC_URL", value: "http://user-service:8080/api",
			wantKind: resolve.HintEnvURL, wantToken: "user-service", wantPort: 8080,
			wantProto: "http", wantEdge: schema.EdgeCalls,
		},
		{
			name: "bare hostname under a host key",
			key:  "SEARCH_HOST", value: "search",
			wantKind: resolve.HintEnvHost, wantToken: "search", wantEdge: schema.EdgeCalls,
		},
		{
			name: "host and port",
			key:  "CACHE_ADDR", value: "redis-primary:6379",
			wantKind: resolve.HintEnvHost, wantToken: "redis-primary", wantPort: 6379,
			wantProto: "redis", wantEdge: schema.EdgePersistsTo,
		},
		{
			name: "kafka broker list takes the first entry",
			key:  "BOOTSTRAP_SERVERS", value: "kafka-0:9092,kafka-1:9092,kafka-2:9092",
			wantKind: resolve.HintEnvHost, wantToken: "kafka-0", wantPort: 9092,
			wantProto: "kafka", wantEdge: schema.EdgePublishesTo,
		},
		{
			name: "fully qualified kubernetes service",
			key:  "USER_SVC", value: "user-service.prod.svc.cluster.local:8080",
			wantKind: resolve.HintEnvHost, wantToken: "user-service.prod.svc.cluster.local",
			wantPort: 8080, wantEdge: schema.EdgeCalls,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolve.ParseValue(tt.key, tt.value)
			if !ok {
				t.Fatalf("ParseValue(%q, %q) was not recognized as a reference", tt.key, tt.value)
			}
			if ref.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", ref.Kind, tt.wantKind)
			}
			if len(ref.Tokens) == 0 || ref.Tokens[0] != tt.wantToken {
				t.Errorf("Tokens = %v, want the first to be %q", ref.Tokens, tt.wantToken)
			}
			if tt.wantPort != 0 && ref.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", ref.Port, tt.wantPort)
			}
			if tt.wantProto != "" && ref.Protocol != tt.wantProto {
				t.Errorf("Protocol = %q, want %q", ref.Protocol, tt.wantProto)
			}
			if ref.Edge != tt.wantEdge {
				t.Errorf("Edge = %q, want %q", ref.Edge, tt.wantEdge)
			}
		})
	}
}

// Everything here would become a false edge if it were treated as a
// reference, and a false edge is the expensive kind of mistake: the user
// never sees it to correct it, and the model reasons on top of it as fact.
func TestParseValueRejectsNonReferences(t *testing.T) {
	tests := []struct {
		name       string
		key, value string
	}{
		{"log level", "LOG_LEVEL", "info"},
		{"environment name", "APP_ENV", "production"},
		{"a plain word under a non-reference key", "NODE_ENV", "development"},
		{"a boolean", "DEBUG", "true"},
		{"a number", "PORT", "8080"},
		{"empty", "DATABASE_URL", ""},
		{"an unresolved interpolation", "DATABASE_URL", "postgres://${DB_HOST}:5432/app"},
		{"a shell substitution", "API_URL", "http://$(hostname):8080"},
		{"localhost says nothing about a component", "API_URL", "http://localhost:8080"},
		{"the bind-all address", "API_HOST", "0.0.0.0"},
		{"a loopback literal", "DB_HOST", "127.0.0.1"},
		{"a bare IP names a machine, not a component", "DB_HOST", "10.0.1.15"},
		{"a filesystem path", "SOCKET_PATH", "/var/run/app.sock"},
		{"an unknown scheme", "STORAGE_URL", "gopher://old:70/x"},
		{"a secret that happens to be url-shaped", "SECRET_KEY", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ref, ok := resolve.ParseValue(tt.key, tt.value); ok {
				t.Errorf("ParseValue(%q, %q) was treated as a reference to %v", tt.key, tt.value, ref.Tokens)
			}
		})
	}
}

// Decomposing a public domain would let any service named "api" match
// api.stripe.com. Cluster-internal names must still decompose, because which
// form a repository writes down varies by file.
func TestHostTokenDecomposition(t *testing.T) {
	tests := []struct {
		name       string
		key, value string
		want       []string
	}{
		{
			name: "a public domain is never split",
			key:  "STRIPE_URL", value: "https://api.stripe.com",
			want: []string{"api.stripe.com"},
		},
		{
			name: "an aws endpoint is never split",
			key:  "QUEUE_URL", value: "https://sqs.us-east-1.amazonaws.com/123/jobs",
			want: []string{"sqs.us-east-1.amazonaws.com"},
		},
		{
			name: "a country-code domain is never split",
			key:  "API_URL", value: "https://api.example.io",
			want: []string{"api.example.io"},
		},
		{
			name: "a cluster-local name offers every form",
			key:  "SVC_HOST", value: "user-service.prod.svc.cluster.local",
			want: []string{"user-service.prod.svc.cluster.local", "user-service.prod", "user-service"},
		},
		{
			name: "a namespaced short name offers both forms",
			key:  "SVC_HOST", value: "postgres.default",
			want: []string{"postgres.default", "postgres"},
		},
		{
			name: "a single label is already minimal",
			key:  "DB_HOST", value: "db",
			want: []string{"db"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolve.ParseValue(tt.key, tt.value)
			if !ok {
				t.Fatalf("ParseValue(%q, %q) was not recognized", tt.key, tt.value)
			}
			if strings.Join(ref.Tokens, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Tokens = %v, want %v", ref.Tokens, tt.want)
			}
		})
	}
}

func TestIsReferenceKey(t *testing.T) {
	yes := []string{
		"DATABASE_URL", "REDIS_URL", "USER_SERVICE_HOST", "API_ENDPOINT",
		"KAFKA_BROKERS", "CACHE_ADDR", "MONGO_URI", "BOOTSTRAP_SERVERS",
		"SEARCH_SVC", "PAYMENTS_BASE_URL", "db_host",
	}
	for _, k := range yes {
		if !resolve.IsReferenceKey(k) {
			t.Errorf("IsReferenceKey(%q) = false, want true", k)
		}
	}
	no := []string{"LOG_LEVEL", "APP_ENV", "NODE_ENV", "DEBUG", "TZ", "REPLICAS", "TAG"}
	for _, k := range no {
		if resolve.IsReferenceKey(k) {
			t.Errorf("IsReferenceKey(%q) = true, want false", k)
		}
	}
}

// Values come straight out of user-controlled YAML and HCL.
func FuzzParseValue(f *testing.F) {
	f.Add("DATABASE_URL", "postgres://app:pw@db:5432/orders")
	f.Add("HOST", "a:b:c")
	f.Add("URL", "://///")
	f.Add("", "")
	f.Add("X_HOST", strings.Repeat("a.", 500))
	f.Add("X_URL", "http://["+strings.Repeat("a", 100)+"]")

	f.Fuzz(func(t *testing.T, key, value string) {
		ref, ok := resolve.ParseValue(key, value)
		if !ok {
			return
		}
		// A reference with no token cannot be matched against anything, so
		// producing one would mean emitting a hint that can only ever
		// resolve by accident.
		if len(ref.Tokens) == 0 {
			t.Fatalf("ParseValue(%q, %q) returned a reference with no tokens", key, value)
		}
		for _, tok := range ref.Tokens {
			if tok == "" {
				t.Fatalf("ParseValue(%q, %q) returned an empty token: %v", key, value, ref.Tokens)
			}
			if strings.ContainsAny(tok, " \t/\\@") {
				t.Fatalf("token %q from (%q, %q) is not a hostname", tok, key, value)
			}
		}
		if ref.Port < 0 || ref.Port > 65535 {
			t.Fatalf("ParseValue(%q, %q) returned port %d", key, value, ref.Port)
		}
	})
}
