package resolve

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Reference is what a configuration value turned out to point at.
type Reference struct {
	Kind     HintKind
	Tokens   []string
	Port     int
	Protocol string
	Edge     schema.EdgeKind
}

// schemeInfo maps a URL scheme to the protocol name, the relationship it
// implies, and its conventional port. The relationship comes from the scheme
// rather than from the target, because a DATABASE_URL means "persists to"
// whatever is on the other end.
var schemeInfo = map[string]struct {
	protocol string
	edge     schema.EdgeKind
	port     int
}{
	"postgres":      {"postgres", schema.EdgePersistsTo, 5432},
	"postgresql":    {"postgres", schema.EdgePersistsTo, 5432},
	"mysql":         {"mysql", schema.EdgePersistsTo, 3306},
	"mariadb":       {"mysql", schema.EdgePersistsTo, 3306},
	"mongodb":       {"mongodb", schema.EdgePersistsTo, 27017},
	"mongodb+srv":   {"mongodb", schema.EdgePersistsTo, 27017},
	"redis":         {"redis", schema.EdgePersistsTo, 6379},
	"rediss":        {"redis", schema.EdgePersistsTo, 6379},
	"cassandra":     {"cassandra", schema.EdgePersistsTo, 9042},
	"clickhouse":    {"clickhouse", schema.EdgePersistsTo, 8123},
	"elasticsearch": {"elasticsearch", schema.EdgePersistsTo, 9200},
	"s3":            {"s3", schema.EdgePersistsTo, 0},
	"sqlserver":     {"sqlserver", schema.EdgePersistsTo, 1433},
	"memcached":     {"memcached", schema.EdgePersistsTo, 11211},

	"amqp":   {"amqp", schema.EdgePublishesTo, 5672},
	"amqps":  {"amqp", schema.EdgePublishesTo, 5671},
	"kafka":  {"kafka", schema.EdgePublishesTo, 9092},
	"nats":   {"nats", schema.EdgePublishesTo, 4222},
	"pulsar": {"pulsar", schema.EdgePublishesTo, 6650},
	"mqtt":   {"mqtt", schema.EdgePublishesTo, 1883},
	"sqs":    {"sqs", schema.EdgePublishesTo, 0},

	"http":  {"http", schema.EdgeCalls, 80},
	"https": {"https", schema.EdgeCalls, 443},
	"grpc":  {"grpc", schema.EdgeCalls, 0},
	"grpcs": {"grpc", schema.EdgeCalls, 0},
	"ws":    {"ws", schema.EdgeCalls, 80},
	"wss":   {"wss", schema.EdgeCalls, 443},
	"tcp":   {"tcp", schema.EdgeCalls, 0},
}

// referenceKeySuffixes mark an environment variable whose value is meant to
// locate something else. Without this check a bare word like "production"
// sitting in APP_ENV would be matched against every service name in the
// repository, which is how a resolver starts inventing edges.
var referenceKeySuffixes = []string{
	"_URL", "_URI", "_HOST", "_HOSTNAME", "_ADDR", "_ADDRESS", "_ENDPOINT",
	"_SERVER", "_SERVERS", "_BROKER", "_BROKERS", "_SERVICE", "_SVC",
	"_DSN", "_CONNECTION", "_CONNECTION_STRING", "_BACKEND", "_UPSTREAM",
	"_TARGET", "_PEER", "_NODES", "_CLUSTER", "_BASE_URL", "_API",
}

// referenceKeyExact covers conventional names that carry no suffix.
var referenceKeyExact = map[string]bool{
	"DATABASE_URL": true, "DB_URL": true, "REDIS_URL": true,
	"HOST": true, "HOSTNAME": true, "ENDPOINT": true, "UPSTREAM": true,
	"BOOTSTRAP_SERVERS": true, "URL": true, "URI": true, "DSN": true,
}

// IsReferenceKey reports whether a configuration key names a location.
func IsReferenceKey(key string) bool {
	upper := strings.ToUpper(key)
	if referenceKeyExact[upper] {
		return true
	}
	for _, suffix := range referenceKeySuffixes {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// ParseValue interprets a configuration value as a reference to another
// component, returning false when it does not look like one.
//
// The key is consulted as well as the value, because the two disambiguate
// each other: "redis" in CACHE_HOST is a reference, "redis" in LOG_PREFIX is
// a word. Being wrong here is expensive — a false edge is invisible to the
// user and flows into the model's reasoning as fact — so anything that is not
// clearly a location is left alone.
func ParseValue(key, value string) (Reference, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Reference{}, false
	}
	// An unresolved interpolation placeholder is not a hostname.
	if strings.Contains(value, "${") || strings.Contains(value, "$(") {
		return Reference{}, false
	}

	if ref, ok := parseURL(value); ok {
		return ref, true
	}
	// A comma-separated broker list is conventional for Kafka and friends.
	if strings.Contains(value, ",") {
		if ref, ok := parseHostList(key, value); ok {
			return ref, true
		}
		return Reference{}, false
	}
	return parseHostPort(key, value)
}

func parseURL(value string) (Reference, bool) {
	if !strings.Contains(value, "://") {
		return Reference{}, false
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return Reference{}, false
	}
	info, known := schemeInfo[strings.ToLower(u.Scheme)]
	if !known {
		return Reference{}, false
	}

	host := u.Hostname()
	if host == "" || !plausibleHost(host) {
		return Reference{}, false
	}
	port := info.port
	if p, ok := parsePort(u.Port()); ok {
		port = p
	}

	kind := HintEnvURL
	if info.edge == schema.EdgePersistsTo || info.edge == schema.EdgePublishesTo {
		kind = HintConnString
	}
	return Reference{
		Kind:     kind,
		Tokens:   hostTokens(host),
		Port:     port,
		Protocol: info.protocol,
		Edge:     info.edge,
	}, true
}

func parseHostList(key, value string) (Reference, bool) {
	// Every entry in a broker list points at the same logical cluster, so
	// the first resolvable one is enough to draw the edge.
	for _, part := range strings.Split(value, ",") {
		if ref, ok := parseHostPort(key, strings.TrimSpace(part)); ok {
			return ref, true
		}
	}
	return Reference{}, false
}

func parseHostPort(key, value string) (Reference, bool) {
	if !IsReferenceKey(key) {
		return Reference{}, false
	}

	host, portStr := value, ""
	if h, p, err := net.SplitHostPort(value); err == nil {
		host, portStr = h, p
	}
	host = strings.TrimSuffix(host, ".")
	if !plausibleHost(host) {
		return Reference{}, false
	}

	port, _ := parsePort(portStr)
	ref := Reference{
		Kind:   HintEnvHost,
		Tokens: hostTokens(host),
		Port:   port,
		Edge:   schema.EdgeCalls,
	}
	// A conventional port is a strong enough signal about the relationship
	// to override the default of "calls".
	if proto, edge, ok := portConvention(port); ok {
		ref.Protocol, ref.Edge = proto, edge
	}
	return ref, true
}

// parsePort accepts only a value that could be a real TCP port. A
// configuration file is allowed to contain nonsense, and a port of 100000
// would flow into an edge and into the model's answer as though it were real.
func parsePort(s string) (port int, ok bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// portConvention maps well-known ports to what runs on them.
func portConvention(port int) (protocol string, edge schema.EdgeKind, ok bool) {
	switch port {
	case 5432:
		return "postgres", schema.EdgePersistsTo, true
	case 3306:
		return "mysql", schema.EdgePersistsTo, true
	case 27017:
		return "mongodb", schema.EdgePersistsTo, true
	case 6379:
		return "redis", schema.EdgePersistsTo, true
	case 11211:
		return "memcached", schema.EdgePersistsTo, true
	case 9200, 9300:
		return "elasticsearch", schema.EdgePersistsTo, true
	case 9042:
		return "cassandra", schema.EdgePersistsTo, true
	case 5672, 5671:
		return "amqp", schema.EdgePublishesTo, true
	case 9092:
		return "kafka", schema.EdgePublishesTo, true
	case 4222:
		return "nats", schema.EdgePublishesTo, true
	default:
		return "", "", false
	}
}

// plausibleHost rejects values that cannot be a host: empty strings, paths,
// wildcards, and the loopback and bind-all addresses, which say nothing about
// which component is on the other end.
func plausibleHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, " \t/\\?#@*\"'") {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "0.0.0.0", "::", "::1", "host.docker.internal":
		return false
	}
	// A bare IP identifies a machine, not a component the repository names.
	if net.ParseIP(host) != nil {
		return false
	}
	// At least one character has to be a letter, or this is a number that
	// happens to be in a URL-shaped string.
	hasLetter := false
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// hostTokens expands a hostname into the identifiers it could match, most
// specific first. A Kubernetes service address such as
// "user-service.prod.svc.cluster.local" has to be matchable both in full and
// as the bare service name, because which one the repository declares depends
// on which file declared it.
//
// Public domains are deliberately not decomposed. Splitting "api.stripe.com"
// into "api.stripe" and "api" would let any service in the repository named
// "api" match Stripe, producing an edge that is entirely fabricated. A wrong
// edge is worse than a missing one: the user never sees it to correct it, and
// the model downstream treats it as a fact about their system.
func hostTokens(host string) []string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return nil
	}
	tokens := []string{host}

	trimmed := strings.TrimSuffix(host, ".svc.cluster.local")
	trimmed = strings.TrimSuffix(trimmed, ".cluster.local")
	trimmed = strings.TrimSuffix(trimmed, ".svc")
	if trimmed != host && trimmed != "" {
		tokens = append(tokens, trimmed)
	}

	labels := strings.Split(trimmed, ".")
	if len(labels) > 1 && isPublicSuffix(labels[len(labels)-1]) {
		return dedupe(tokens)
	}

	// Progressively shorter prefixes: "a.b.c" also offers "a.b" and "a".
	for i := len(labels) - 1; i >= 1; i-- {
		if candidate := strings.Join(labels[:i], "."); candidate != "" {
			tokens = append(tokens, candidate)
		}
	}
	return dedupe(tokens)
}

// commonGTLDs covers the generic top-level domains that turn up in service
// configuration. It does not need to be exhaustive: an unknown suffix simply
// means a host gets decomposed, which at worst offers the resolver an extra
// candidate that matches nothing.
var commonGTLDs = map[string]bool{
	"com": true, "net": true, "org": true, "edu": true, "gov": true,
	"mil": true, "int": true, "info": true, "biz": true, "dev": true,
	"app": true, "cloud": true, "tech": true, "xyz": true, "online": true,
	"site": true, "store": true, "shop": true, "blog": true, "page": true,
	"run": true, "systems": true, "services": true, "network": true,
	"digital": true, "software": true, "tools": true, "email": true,
	"live": true, "world": true, "space": true, "website": true,
}

// isPublicSuffix reports whether a final label belongs to the public DNS
// rather than to a cluster's internal namespace. Two-letter labels are
// country-code TLDs, which covers io, ai, co, sh, me, and the rest.
func isPublicSuffix(label string) bool {
	if len(label) == 2 {
		return true
	}
	return commonGTLDs[label]
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
