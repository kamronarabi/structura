package resolve

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Credentials must never reach graph.json.
//
// The file gets committed to repositories and pasted into LLM context, so a
// password that leaks through it does not stay in one place — it ends up in
// git history, in a model provider's logs, and in whatever the model quoted
// it into. Of everything this tool can get wrong, it is the only failure that
// cannot be walked back, which is why redaction runs before serialization
// rather than at the point each value is produced, and why it is tested with
// its own fixtures and its own CI gate.
//
// The rules are deliberately pattern-based rather than entropy-based.
// Entropy heuristics flag image digests, content hashes, and base64 config
// blobs, and a redactor that mangles ordinary values gets switched off.

// Mask replaces a redacted value.
const Mask = "***"

var (
	// userinfo in a URL: scheme://user:password@host
	credentialURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/\s:@]+):([^/\s@]*)@`)

	// AWS access key IDs have a fixed, recognizable shape.
	awsAccessKey = regexp.MustCompile(`\b((?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ABIA|ACCA)[A-Z0-9]{16})\b`)

	// Provider tokens with distinctive prefixes.
	prefixedToken = regexp.MustCompile(`\b((?:gh[pousr]_|github_pat_|xox[baprs]-|sk-[A-Za-z0-9-]*|sk_live_|pk_live_|rk_live_|glpat-|npm_|dop_v1_|shpat_|AIza)[A-Za-z0-9_\-]{8,})`)

	// JSON Web Tokens.
	jwt = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)

	// Private key blocks.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// secretKeySubstrings mark a configuration key whose value is a credential.
// The whole value goes, not part of it: there is nothing in a password worth
// keeping in an architecture graph.
//
// These are matched anywhere in the key, because each is long and
// distinctive enough that an accidental match is implausible.
var secretKeySubstrings = []string{
	"PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "API_KEY",
	"ACCESS_KEY", "PRIVATE_KEY", "CLIENT_SECRET", "CREDENTIAL",
	"SIGNING", "CERTIFICATE", "PASSPHRASE",
}

// secretKeySegments are matched only as a whole delimited segment of the key.
//
// Each is short enough to occur inside an ordinary word. PIN appears in
// SHIPPING, PING, MAPPING, SHOPPING and SPINNAKER; AUTH appears in AUTHOR
// and AUTHORITY. Matched as substrings they blank out exactly the values the
// graph is built from -- a SHIPPING_SERVICE_ADDR redacted to *** leaves an
// edge whose evidence cannot be checked, which is worse than useless when
// the whole claim is that every edge shows its work.
var secretKeySegments = []string{"AUTH", "AUTHORIZATION", "SALT", "PIN"}

// secretKeyExceptions are keys that contain a trigger word but hold a
// location or a flag rather than a credential. Without these, redaction
// would blank out exactly the values the graph is built from.
var secretKeyExceptions = []string{
	"SECRET_NAME", "SECRET_ARN", "SECRET_KEY_REF", "SECRETS_MANAGER_ENDPOINT",
	"TOKEN_URL", "TOKEN_ENDPOINT", "AUTH_URL", "AUTH_ENDPOINT", "AUTH_HOST",
	"AUTH_SERVICE", "AUTH_SERVER", "AUTH_ENABLED", "AUTH_TYPE", "AUTH_MODE",
	"KEY_VAULT_URL", "PRIVATE_KEY_PATH", "CERTIFICATE_PATH", "SECRET_PATH",
	"CREDENTIAL_PATH", "API_KEY_HEADER",
}

// IsSecretKey reports whether a key's value should be masked outright.
func IsSecretKey(key string) bool {
	upper := normalizeKey(key)
	if upper == "" {
		return false
	}
	for _, exception := range secretKeyExceptions {
		if upper == exception || strings.HasSuffix(upper, "_"+exception) {
			return false
		}
	}
	for _, part := range secretKeySubstrings {
		if strings.Contains(upper, part) {
			return true
		}
	}
	for _, segment := range keySegments(upper) {
		for _, part := range secretKeySegments {
			if segment == part {
				return true
			}
		}
	}
	return false
}

// normalizeKey uppercases a key and reduces every separator to an
// underscore, so that api-key, api.key and API_KEY are one spelling.
//
// Without this, the separator a project happens to use decides whether its
// credentials are redacted.
func normalizeKey(key string) string {
	upper := strings.ToUpper(strings.TrimSpace(key))
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, upper)
}

// keySegments splits a configuration key into its words, on both punctuation
// and camelCase, so that DB_PASSWORD, db-password and dbPassword all yield
// the same segments.
func keySegments(key string) []string {
	var spaced strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) &&
			(unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			spaced.WriteByte('_')
		}
		spaced.WriteRune(r)
	}
	segments := strings.FieldsFunc(spaced.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i := range segments {
		segments[i] = strings.ToUpper(segments[i])
	}
	return segments
}

// RedactValue masks a single key/value pair.
func RedactValue(key, value string) (masked string, redacted bool) {
	if value == "" {
		return value, false
	}
	if IsSecretKey(key) {
		return Mask, true
	}
	out := RedactString(value)
	return out, out != value
}

// RedactString masks any credential embedded in a free-form string, leaving
// the rest intact. A connection string keeps its host, port, and database
// name — which is what the graph is built from — and loses only the userinfo.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	out := pemBlock.ReplaceAllString(s, "-----BEGIN PRIVATE KEY----- "+Mask+" -----END PRIVATE KEY-----")
	out = credentialURL.ReplaceAllString(out, "${1}"+Mask+":"+Mask+"@")
	out = awsAccessKey.ReplaceAllString(out, Mask)
	out = prefixedToken.ReplaceAllString(out, Mask)
	out = jwt.ReplaceAllString(out, Mask)
	return out
}

// RedactGraph masks every string that reaches disk: attribute values,
// evidence details, and diagnostic messages.
//
// It runs over the finished graph rather than at each producer, because
// "every extractor remembered to redact" is not a property anyone can verify,
// while "nothing is serialized without passing through here" is.
func RedactGraph(g *schema.Graph) int {
	count := 0
	for i := range g.Nodes {
		for k, v := range g.Nodes[i].Attrs {
			if s, ok := v.(string); ok {
				if masked, did := RedactValue(k, s); did {
					g.Nodes[i].Attrs[k] = masked
					count++
				}
			}
		}
	}
	for i := range g.Edges {
		for j := range g.Edges[i].Evidence {
			ev := &g.Edges[i].Evidence[j]
			if masked := RedactString(ev.Detail); masked != ev.Detail {
				ev.Detail = masked
				count++
			}
		}
	}
	for i := range g.Diagnostics {
		if masked := RedactString(g.Diagnostics[i].Message); masked != g.Diagnostics[i].Message {
			g.Diagnostics[i].Message = masked
			count++
		}
	}
	return count
}

// RedactHint returns a copy of a hint with its raw value masked, for display
// and for evidence records.
func RedactHint(h Hint) Hint {
	h.Raw = RedactString(h.Raw)
	h.Source.Detail = RedactString(h.Source.Detail)
	return h
}
