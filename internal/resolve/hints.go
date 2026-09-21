// Package resolve turns the unresolved references extractors noticed into
// graph edges, by building an identity index over every known node and
// matching reference tokens against it.
//
// This is where a pile of disconnected boxes becomes an architecture map.
// Configuration files state what exists; they almost never state what calls
// what. No field in a Kubernetes manifest says "api-gateway calls
// user-service" — that has to be inferred from an environment variable, a
// label selector, or a connection string, by matching a name against the set
// of names the repository defines.
package resolve

import "github.com/kamronarabi/structura/pkg/schema"

// HintKind names the sort of reference an extractor noticed, which determines
// how it is matched and how much confidence a match earns.
type HintKind string

const (
	// HintDependsOn is an explicit dependency declaration, such as Compose
	// depends_on. Nothing is being inferred.
	HintDependsOn HintKind = "depends_on"
	// HintTraversal is a direct expression reference between two declared
	// resources, such as a Terraform traversal.
	HintTraversal HintKind = "traversal"
	// HintSelector is a Kubernetes label selector or ingress backend, where
	// the chain from one object to another is deterministic.
	HintSelector HintKind = "selector"
	// HintEnvURL is an environment variable whose value parses as a URL.
	HintEnvURL HintKind = "env_url"
	// HintEnvHost is an environment variable whose value looks like a bare
	// hostname or host:port.
	HintEnvHost HintKind = "env_host"
	// HintConnString is a database or broker connection string.
	HintConnString HintKind = "conn_string"
	// HintConfigValue is a value reached through indirection, such as a
	// ConfigMap entry mounted into a pod.
	HintConfigValue HintKind = "config_value"
	// HintImage is a container image reference, the weakest signal: it
	// suggests two definitions describe the same component, nothing more.
	HintImage HintKind = "image"
	// HintLibrary is a dependency that implies infrastructure: a Postgres
	// driver in go.mod means this service talks to a Postgres somewhere.
	// It says what technology is involved but never which instance, so it
	// corroborates an edge rather than establishing one.
	HintLibrary HintKind = "library"
)

// BaseConfidence is the confidence a hint kind earns on an exact match,
// before any penalty for a fuzzier match rule. The values are the calibration
// table from the plan, in one place so the resolver can be retuned without
// hunting through call sites.
func (k HintKind) BaseConfidence() float64 {
	switch k {
	case HintDependsOn:
		return schema.ConfDeclared
	case HintTraversal, HintSelector:
		return schema.ConfReference
	case HintEnvURL, HintEnvHost, HintConnString:
		return schema.ConfHostMatch
	case HintConfigValue:
		return schema.ConfIndirect
	case HintImage, HintLibrary:
		return schema.ConfWeak
	default:
		return schema.ConfWeak
	}
}

// Hint is a reference one extractor saw and could not resolve on its own.
//
// Extractors see a single file and nothing else, by design: that is what
// makes them independently testable, parallelizable, and order-independent.
// Anything requiring knowledge of another file leaves the extractor as a
// Hint and is resolved later, globally, against the finished node set.
type Hint struct {
	// FromNode is the node that holds the reference.
	FromNode string

	// OwnerDir is set instead of FromNode when the file carrying the
	// reference is not itself a component and does not name one: a .env file
	// belongs to whatever service lives in its directory, which the
	// extractor cannot know because it sees one file. The resolver binds it
	// once the node set is complete, and reports the reference rather than
	// attaching it if the directory turns out to hold no component, or more
	// than one.
	OwnerDir string

	Kind HintKind

	// Raw is the reference as written. It is redacted before serialization;
	// connection strings routinely contain passwords.
	Raw string

	// Tokens are the candidate identifiers extracted from Raw — usually a
	// hostname, sometimes a bare service name. The resolver matches these
	// against the identity index in order, most specific first.
	Tokens []string

	// Port and Protocol narrow the match and describe the resulting edge.
	Port     int
	Protocol string

	// SuggestedEdge is what the relationship would be if this resolves. The
	// extractor knows the semantics of the field it read better than the
	// resolver does — DATABASE_URL implies persists_to regardless of what
	// the target turns out to be — but the resolver may override it based on
	// the matched node's kind.
	SuggestedEdge schema.EdgeKind

	// Source locates the reference for the evidence record.
	Source schema.Evidence
}
