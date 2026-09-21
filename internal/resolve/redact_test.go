package resolve_test

import (
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/pkg/schema"
)

// secretFixtures are credential-shaped values that must never survive into
// graph.json. The file is committed to repositories and pasted into model
// context, so a leak here propagates into git history and a provider's logs
// and cannot be walked back.
//
// The provider tokens below deliberately read as EXAMPLENOTAREAL rather than
// as plausible random strings. They only have to match the patterns in
// redact.go, and a fixture with realistic entropy trips GitHub's push
// protection — which blocks the push for every contributor who forks this
// repository, over a credential that never existed. Keep new fixtures
// entropy-free for the same reason.
var secretFixtures = []struct {
	name        string
	key, value  string
	mustNotLeak []string
}{
	{
		name: "postgres password in a connection string",
		key:  "DATABASE_URL", value: "postgres://app:s3cr3tP4ss@db:5432/orders",
		mustNotLeak: []string{"s3cr3tP4ss"},
	},
	{
		name: "amqp credentials",
		key:  "BROKER_URL", value: "amqp://guest:guestpassword@queue:5672//",
		mustNotLeak: []string{"guestpassword"},
	},
	{
		name: "mongodb srv credentials",
		key:  "MONGO_URI", value: "mongodb+srv://admin:TopSecret99@cluster0.mongodb.net/app",
		mustNotLeak: []string{"TopSecret99"},
	},
	{
		name: "value under a password key",
		key:  "POSTGRES_PASSWORD", value: "hunter2",
		mustNotLeak: []string{"hunter2"},
	},
	{
		name: "value under a secret key",
		key:  "DJANGO_SECRET_KEY", value: "django-insecure-abc123xyz",
		mustNotLeak: []string{"django-insecure-abc123xyz"},
	},
	{
		name: "aws access key id",
		key:  "AWS_ACCESS_KEY_ID", value: "AKIAIOSFODNN7EXAMPLE",
		mustNotLeak: []string{"AKIAIOSFODNN7EXAMPLE"},
	},
	{
		name: "aws key embedded in prose",
		key:  "NOTES", value: "deploy with AKIAIOSFODNN7EXAMPLE then rotate",
		mustNotLeak: []string{"AKIAIOSFODNN7EXAMPLE"},
	},
	{
		name: "stripe live key",
		key:  "PAYMENTS", value: "sk_live_EXAMPLENOTAREALKEY",
		mustNotLeak: []string{"sk_live_EXAMPLENOTAREALKEY"},
	},
	{
		name: "github personal access token",
		key:  "CI_NOTE", value: "token ghp_EXAMPLENOTAREALTOKEN",
		mustNotLeak: []string{"ghp_EXAMPLENOTAREALTOKEN"},
	},
	{
		name: "slack bot token",
		key:  "NOTIFY", value: "xoxb-EXAMPLE-NOTAREALTOKEN",
		mustNotLeak: []string{"xoxb-EXAMPLE-NOTAREALTOKEN"},
	},
	{
		name: "gitlab token",
		key:  "MIRROR", value: "glpat-EXAMPLENOTAREALTOKEN",
		mustNotLeak: []string{"glpat-EXAMPLENOTAREALTOKEN"},
	},
	{
		name: "json web token",
		key:  "BEARER", value: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.dBjftJeZ4CVPmB92K27uhbUJU1p1r",
		mustNotLeak: []string{"dBjftJeZ4CVPmB92K27uhbUJU1p1r"},
	},
	{
		name: "private key block",
		key:  "TLS", value: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----",
		mustNotLeak: []string{"MIIEowIBAAKCAQEA1234"},
	},
	{
		name: "api key under a token key name",
		key:  "SENTRY_AUTH_TOKEN", value: "0123456789abcdef0123456789abcdef",
		mustNotLeak: []string{"0123456789abcdef0123456789abcdef"},
	},
}

func TestRedactValueMasksCredentials(t *testing.T) {
	for _, tt := range secretFixtures {
		t.Run(tt.name, func(t *testing.T) {
			masked, did := resolve.RedactValue(tt.key, tt.value)
			if !did {
				t.Errorf("RedactValue(%q, ...) reported no redaction", tt.key)
			}
			for _, secret := range tt.mustNotLeak {
				if strings.Contains(masked, secret) {
					t.Errorf("credential survived redaction: %q is still in %q", secret, masked)
				}
			}
		})
	}
}

// Redaction that eats the data is as much of a failure as redaction that
// misses a secret: a connection string's host is exactly what the graph is
// built from.
func TestRedactKeepsTheArchitecturalContent(t *testing.T) {
	masked := resolve.RedactString("postgres://app:s3cr3t@db:5432/orders")
	want := "postgres://" + resolve.Mask + ":" + resolve.Mask + "@db:5432/orders"
	if masked != want {
		t.Errorf("RedactString() = %q, want %q", masked, want)
	}
	for _, keep := range []string{"postgres", "db", "5432", "orders"} {
		if !strings.Contains(masked, keep) {
			t.Errorf("redaction removed %q, which the graph needs: %q", keep, masked)
		}
	}
}

func TestRedactLeavesOrdinaryValuesAlone(t *testing.T) {
	// Values that must pass through untouched, including several that a
	// naive entropy check would flag: image digests, content hashes, and
	// base64-looking configuration.
	unchanged := []struct{ key, value string }{
		{"LOG_LEVEL", "info"},
		{"APP_ENV", "production"},
		{"DATABASE_URL", "postgres://db:5432/orders"},
		{"REDIS_URL", "redis://cache:6379/0"},
		{"IMAGE", "ghcr.io/acme/api@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
		{"COMMIT", "5bfd3178b88740cb5764826fa93ee9f9142128a2"},
		{"AUTH_URL", "https://auth.example.com/oauth/token"},
		{"AUTH_ENABLED", "true"},
		{"SECRET_NAME", "db-credentials"},
		{"PRIVATE_KEY_PATH", "/etc/tls/server.key"},
		{"REPLICAS", "3"},
		{"BOOTSTRAP_SERVERS", "kafka-0:9092,kafka-1:9092"},
	}
	for _, tt := range unchanged {
		t.Run(tt.key, func(t *testing.T) {
			masked, did := resolve.RedactValue(tt.key, tt.value)
			if did || masked != tt.value {
				t.Errorf("RedactValue(%q, %q) = %q; an ordinary value was mangled", tt.key, tt.value, masked)
			}
		})
	}
}

func TestIsSecretKey(t *testing.T) {
	secret := []string{
		"PASSWORD", "DB_PASSWORD", "POSTGRES_PASSWORD", "API_KEY", "APIKEY",
		"SECRET", "CLIENT_SECRET", "ACCESS_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"PRIVATE_KEY", "JWT_SIGNING_KEY", "SESSION_SALT", "PASSPHRASE",
	}
	for _, k := range secret {
		if !resolve.IsSecretKey(k) {
			t.Errorf("resolve.IsSecretKey(%q) = false, want true", k)
		}
	}

	// Location-shaped keys that merely contain a trigger word. Blanking
	// these would delete the very values the resolver matches on.
	notSecret := []string{
		"AUTH_URL", "AUTH_SERVICE", "AUTH_HOST", "TOKEN_ENDPOINT",
		"SECRET_NAME", "SECRET_ARN", "KEY_VAULT_URL", "DATABASE_URL",
		"REDIS_URL", "REPLICAS", "PORT",
	}
	for _, k := range notSecret {
		if resolve.IsSecretKey(k) {
			t.Errorf("resolve.IsSecretKey(%q) = true, want false", k)
		}
	}
}

// The guarantee that matters is not "each producer remembers to redact" but
// "nothing is serialized without passing through redaction".
func TestRedactGraphCoversEverySerializedString(t *testing.T) {
	b := schema.NewBuilder()
	from := schema.NewNodeID(schema.KindService, "compose", "app", "web")
	to := schema.NewNodeID(schema.KindDatastore, "compose", "app", "db")

	b.AddNode(schema.Node{
		ID: from, Kind: schema.KindService, Layer: schema.LayerContainer,
		Name: "web", Confidence: 1,
		Attrs: schema.Attrs{
			"DATABASE_URL":   "postgres://app:s3cr3t@db:5432/orders",
			"ADMIN_PASSWORD": "hunter2",
			"image":          "ghcr.io/acme/web:1.0",
		},
	})
	b.AddNode(schema.Node{
		ID: to, Kind: schema.KindDatastore, Layer: schema.LayerContainer,
		Name: "db", Confidence: 1,
	})
	b.AddEdge(schema.Edge{
		From: from, To: to, Kind: schema.EdgePersistsTo, Confidence: 0.8,
		Evidence: []schema.Evidence{{
			Extractor: "resolver", Rule: "env_host_match",
			Detail: "env DATABASE_URL=postgres://app:s3cr3t@db:5432/orders",
		}},
	})
	b.Diag(schema.Diagnostic{
		Severity: schema.SeverityWarn, Code: "example",
		Message: "could not reach amqp://guest:guestpassword@queue:5672",
	})

	g, err := b.Build(schema.Root{Name: "x"}, schema.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	resolve.RedactGraph(&g)

	out, err := schema.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cr3t", "hunter2", "guestpassword"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("credential %q reached the serialized graph:\n%s", secret, out)
		}
	}
	// The values the graph is for are still there.
	for _, keep := range []string{"db:5432/orders", "ghc" + "r.io/acme/web:1.0", "queue:5672"} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("redaction removed %q, which the graph needs", keep)
		}
	}
}

// Redaction runs on values that came out of arbitrary files. It must not
// panic, and masking an already-masked value must be a fixed point — the
// pipeline applies it at the hint and again at serialization.
func FuzzRedactString(f *testing.F) {
	for _, tt := range secretFixtures {
		f.Add(tt.value)
	}
	f.Add("")
	f.Add("postgres://@:/")
	f.Add(strings.Repeat("a:b@", 500))

	f.Fuzz(func(t *testing.T, value string) {
		once := resolve.RedactString(value)
		twice := resolve.RedactString(once)
		if once != twice {
			t.Fatalf("redaction is not idempotent:\n %q\n %q\n %q", value, once, twice)
		}
	})
}

// TestIsSecretKeyDoesNotMatchInsideOrdinaryWords guards the redactor's
// appetite.
//
// Over-redaction is not a safe default here. The graph is built out of these
// values, and an edge whose evidence reads *** cannot be checked by the
// person it is shown to — which is the opposite of the claim the project
// makes about every edge showing its work. This was found by scanning a real
// repository: SHIPPING_SERVICE_ADDR contains "PIN".
func TestIsSecretKeyDoesNotMatchInsideOrdinaryWords(t *testing.T) {
	for _, key := range []string{
		"SHIPPING_SERVICE_ADDR", // SHIP-PIN-G
		"SHOPPING_CART_URL",     // SHOP-PIN-G
		"PING_INTERVAL",         // PIN-G
		"GRPC_PING_TIMEOUT",     // PIN-G
		"MAPPING_FILE",          // MAP-PIN-G
		"SPINNAKER_URL",         // S-PIN-NAKER
		"AUTHOR_NAME",           // AUTH-OR
		"AUTHORITY_URL",         // AUTH-ORITY
		"shippingServiceAddr",   // the camelCase spelling of the same thing
		"stepping_stone_host",   // STEP-PIN-G
	} {
		if resolve.IsSecretKey(key) {
			t.Errorf("%q was treated as a credential; a trigger word inside an "+
				"ordinary word is not a credential", key)
		}
	}
}

// TestIsSecretKeyStillCatchesCredentials is the other half: loosening the
// match must not let a real secret through.
func TestIsSecretKeyStillCatchesCredentials(t *testing.T) {
	for _, key := range []string{
		"DB_PASSWORD", "PASSWORD", "MYSQL_PASSWD",
		"API_KEY", "APIKEY", "AWS_ACCESS_KEY_ID",
		"JWT_SECRET", "CLIENT_SECRET", "DJANGO_SECRET_KEY",
		"AUTH_TOKEN", "GITHUB_TOKEN", "AUTHORIZATION",
		"AUTH", "SALT", "PIN", "PASSWORD_HASH_SALT",
		"TLS_CERTIFICATE", "REQUEST_SIGNING_KEY", "SSH_PASSPHRASE",
		"dbPassword", "apiKey", "clientSecret", // camelCase spellings
		"db-password", "api-key", // kebab-case spellings
	} {
		if !resolve.IsSecretKey(key) {
			t.Errorf("%q was not treated as a credential", key)
		}
	}
}

// TestSecretKeyExceptionsStillApply covers keys that carry a trigger word but
// hold a location or a flag.
func TestSecretKeyExceptionsStillApply(t *testing.T) {
	for _, key := range []string{
		"SECRET_NAME", "SECRET_ARN", "DB_SECRET_ARN",
		"AUTH_URL", "AUTH_HOST", "AUTH_SERVICE", "AUTH_ENABLED",
		"TOKEN_URL", "PRIVATE_KEY_PATH",
	} {
		if resolve.IsSecretKey(key) {
			t.Errorf("%q names a location or a flag, not a credential", key)
		}
	}
}
