package resolve

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Match is the outcome of looking a reference up in the index.
type Match struct {
	NodeID     string
	Rule       string
	Confidence float64
	// Candidates is set when more than one node claimed the reference and
	// nothing broke the tie.
	Candidates []string
}

// matchRule is one rung of the precedence ladder, in descending order of how
// much a hit is worth.
//
// Precision is what matters here, not recall. A missing edge is a visible
// gap: the diagram looks sparse and the user notices. A wrong edge is
// invisible and actively harmful — it flows into the MCP server, the model
// reasons on top of it, and the Phase 3 profiler ends up giving confident
// advice about a dependency that does not exist. So the ladder stops at the
// first rung that hits, and a rung that cannot distinguish between candidates
// declines rather than guessing.
type matchRule struct {
	name string
	// penalty is subtracted from the hint's base confidence, expressing how
	// much looser this rule is than an exact address match.
	penalty float64
	lookup  func(idx *Index, token string) []string
}

var matchRules = []matchRule{
	{
		name:    "dns_exact",
		penalty: 0,
		lookup:  func(idx *Index, token string) []string { return idx.byDNS[strings.ToLower(token)] },
	},
	{
		name:    "name_exact",
		penalty: 0,
		lookup:  func(idx *Index, token string) []string { return idx.byName[strings.ToLower(token)] },
	},
	{
		name:    "name_normalized",
		penalty: 0.10,
		lookup: func(idx *Index, token string) []string {
			return idx.byNormalized[NormalizeName(token)]
		},
	},
	{
		name:    "image_basename",
		penalty: 0.25,
		lookup:  func(idx *Index, token string) []string { return idx.byImage[strings.ToLower(token)] },
	},
}

// Resolve looks up a hint's tokens and returns the best match, if any.
//
// Tokens are tried most-specific first, and within a token the rules are
// tried strongest first. The first rule that yields exactly one node — after
// tie-breaking — wins. Everything else is reported rather than guessed.
func (idx *Index) Resolve(h Hint, from *Identity) (Match, bool) {
	for _, token := range h.Tokens {
		if token == "" {
			continue
		}
		for _, rule := range matchRules {
			candidates := withoutSelf(rule.lookup(idx, token), h.FromNode)
			if len(candidates) == 0 {
				continue
			}

			confidence := h.Kind.BaseConfidence() - rule.penalty
			if len(candidates) == 1 {
				return Match{NodeID: candidates[0], Rule: rule.name, Confidence: confidence}, true
			}

			// More than one node answers to this name. Tie-breaking is
			// deliberately conservative: only signals that genuinely narrow
			// the field count, and if the field stays wide the reference is
			// reported as ambiguous instead of resolved.
			narrowed := idx.tieBreak(candidates, h, from)
			if len(narrowed) == 1 {
				return Match{
					NodeID:     narrowed[0],
					Rule:       rule.name + "+scoped",
					Confidence: confidence,
				}, true
			}
			return Match{Rule: rule.name, Candidates: candidates}, false
		}
	}
	return Match{}, false
}

// tieBreak narrows a candidate set using context the reference carries.
func (idx *Index) tieBreak(candidates []string, h Hint, from *Identity) []string {
	// A reference from inside a namespace means the thing in that namespace.
	// This is the case a dev/staging/prod repository hits constantly, and
	// the only one where the answer is genuinely unambiguous.
	if from != nil && from.Namespace != "" {
		if scoped := filterByNamespace(idx, candidates, from.Namespace); len(scoped) > 0 {
			candidates = scoped
		}
	}
	if len(candidates) == 1 {
		return candidates
	}

	// A port narrows but never decides on its own: half the services in a
	// repository listen on 8080, so a port-only match would be close to a
	// coin flip.
	if h.Port > 0 {
		if scoped := filterByPort(idx, candidates, h.Port); len(scoped) > 0 {
			candidates = scoped
		}
	}
	if len(candidates) == 1 {
		return candidates
	}

	// The kind the reference implies: a DATABASE_URL points at a datastore,
	// whatever else shares its name.
	if wanted := kindForEdge(h.SuggestedEdge); wanted != "" {
		if scoped := filterByKind(idx, candidates, wanted); len(scoped) > 0 {
			candidates = scoped
		}
	}
	return candidates
}

func filterByNamespace(idx *Index, candidates []string, namespace string) []string {
	var out []string
	for _, id := range candidates {
		if identity, ok := idx.Identity(id); ok && identity.Namespace == namespace {
			out = append(out, id)
		}
	}
	return out
}

func filterByPort(idx *Index, candidates []string, port int) []string {
	var out []string
	for _, id := range candidates {
		identity, ok := idx.Identity(id)
		if !ok {
			continue
		}
		for _, p := range identity.Ports {
			if p == port {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func filterByKind(idx *Index, candidates []string, kind schema.NodeKind) []string {
	var out []string
	for _, id := range candidates {
		if identity, ok := idx.Identity(id); ok && identity.Kind == kind {
			out = append(out, id)
		}
	}
	return out
}

// kindForEdge reports the node kind a relationship implies, or empty when it
// implies nothing in particular.
func kindForEdge(kind schema.EdgeKind) schema.NodeKind {
	switch kind {
	case schema.EdgePersistsTo:
		return schema.KindDatastore
	case schema.EdgePublishesTo, schema.EdgeSubscribesTo:
		return schema.KindQueue
	default:
		return ""
	}
}

func withoutSelf(candidates []string, self string) []string {
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if id != self {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// edgeKindFor chooses the relationship for a resolved reference.
//
// The hint's suggestion usually wins: the extractor read the field and knows
// that DATABASE_URL means persistence regardless of what sits on the other
// end. The target's kind overrides it only where the suggestion was a default
// rather than a reading — a bare hostname resolving to a queue is publishing
// to it, not calling it.
func edgeKindFor(suggested schema.EdgeKind, target schema.NodeKind) schema.EdgeKind {
	if suggested != "" && suggested != schema.EdgeCalls && suggested != schema.EdgeDependsOn {
		return suggested
	}
	switch target {
	case schema.KindDatastore:
		return schema.EdgePersistsTo
	case schema.KindQueue:
		return schema.EdgePublishesTo
	}
	if suggested != "" {
		return suggested
	}
	return schema.EdgeDependsOn
}

// ambiguityDiagnostic reports a reference that more than one node answers to.
//
// This is the multi-environment case: a repository with dev, staging, and
// prod overlays declares the same service names three times, and picking one
// would be wrong two times in three. Saying so is the correct answer — the
// user can see the gap and fix the scoping, while a fabricated edge would
// simply be believed.
func ambiguityDiagnostic(h Hint, m Match, names func(string) string) schema.Diagnostic {
	labels := make([]string, len(m.Candidates))
	for i, id := range m.Candidates {
		labels[i] = names(id)
	}
	sort.Strings(labels)

	return schema.Diagnostic{
		Severity: schema.SeverityWarn,
		Code:     "ambiguous_reference",
		Path:     h.Source.Path,
		Line:     h.Source.Line,
		Message: fmt.Sprintf(
			"%q matches %d components (%s), so no edge was drawn; qualify the reference or scope it to a namespace",
			h.Raw, len(m.Candidates), strings.Join(labels, ", ")),
	}
}
