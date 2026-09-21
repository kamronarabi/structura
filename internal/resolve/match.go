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
	// OutOfScope reports that every candidate lived outside the referrer's
	// own project or namespace. Such a match is refused rather than drawn:
	// two unrelated projects in one repository routinely reuse names like
	// "api" or "orders-db", and an edge between them is fiction that scores
	// exactly as high as a real one.
	OutOfScope bool
}

// scopeOf returns the deployment boundary a reference is being made from, or
// "" when the referrer has none and locality cannot be judged.
//
// Returning "" is not a failure. It means this reference has to be matched
// globally, because nothing about the referrer says which part of the
// repository it belongs to — which is the honest answer for a service known
// only from a dependency manifest.
func scopeOf(from *Identity) string {
	if from == nil {
		return ""
	}
	return from.Scope
}

// localCandidates gathers everything in the referrer's own scope that answers
// to a token, across every rule rather than stopping at the strongest.
//
// ok is false when locality cannot be judged at all, in which case the caller
// falls back to the global ladder.
func (idx *Index) localCandidates(token string, h Hint, from *Identity) (byRule []ruleCandidates, ok bool) {
	scope := scopeOf(from)
	if scope == "" {
		return nil, false
	}
	for _, rule := range matchRules {
		candidates := withoutSelf(idx, rule.lookup(idx, token), h.FromNode)
		if len(candidates) == 0 {
			continue
		}
		if scoped := filterInScope(idx, candidates, scope); len(scoped) > 0 {
			byRule = append(byRule, ruleCandidates{rule: rule, ids: scoped})
		}
	}
	return byRule, len(byRule) > 0
}

// ruleCandidates pairs a rule with the nodes it matched.
type ruleCandidates struct {
	rule matchRule
	ids  []string
}

// decide picks a winner from per-rule candidates.
//
// byRule is ordered strongest rule first, and only the strongest rule that
// matched gets to answer. It deliberately does not fall through to a weaker
// rule when the strongest is ambiguous: two nodes answering to the same name
// under dns_exact is a reference the author has to qualify, and quietly
// resolving it with image_basename instead would turn a question into a
// guess. This is the ladder's original semantics, kept.
func (idx *Index) decide(byRule []ruleCandidates, h Hint, from *Identity) (Match, bool) {
	if len(byRule) == 0 {
		return Match{}, false
	}
	rc := byRule[0]
	confidence := h.Kind.BaseConfidence() - rc.rule.penalty

	if len(rc.ids) == 1 {
		return Match{NodeID: rc.ids[0], Rule: rc.rule.name, Confidence: confidence}, true
	}
	if narrowed := idx.tieBreak(rc.ids, h, from); len(narrowed) == 1 {
		// Scored below an unambiguous hit. Several components answered to
		// this name and scope picked the winner -- usually correctly, which
		// is why the tie-break exists, but it is a judgement the single-match
		// case did not have to make, and scoring the two the same asserts a
		// certainty that was not there. The rule name already records it;
		// the number should agree.
		return Match{
			NodeID:     narrowed[0],
			Rule:       rc.rule.name + "+scoped",
			Confidence: confidence - scopedPenalty,
		}, true
	}
	return Match{Rule: rc.rule.name, Candidates: rc.ids}, false
}

// scopedPenalty is what a resolution costs when the name was ambiguous and
// scope decided it.
//
// One step, not a tier: the edge is still the same rule's finding, and the
// tie-break is right far more often than not. It exists so that a reader
// comparing two 0.80 edges can tell which one had to be disambiguated.
const scopedPenalty = 0.05

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

		// Locality is decided across the whole ladder, before any single rule
		// is allowed to answer.
		//
		// Otherwise the strongest rule wins on a global index and a weaker
		// rule never runs. A compose service referring to "orders-db" matched
		// a Kubernetes Service of that name in an unrelated directory,
		// because dns_exact found exactly one node and returned before
		// name_exact could offer the orders-db defined five lines below it in
		// the same file. One candidate is not the same as the right
		// candidate.
		if local, ok := idx.localCandidates(token, h, from); ok {
			return idx.decide(local, h, from)
		}

		for _, rule := range matchRules {
			candidates := withoutSelf(idx, rule.lookup(idx, token), h.FromNode)
			if len(candidates) == 0 {
				continue
			}

			// Reaching here with a scoped referrer means nothing inside its
			// own project answered; every candidate belongs to someone else.
			// Two unrelated projects in one repository reuse names like "api"
			// and "orders-db" constantly, and an edge between them is fiction
			// that scores exactly as high as a real one.
			if scopeOf(from) != "" {
				return Match{Rule: rule.name, Candidates: candidates, OutOfScope: true}, false
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

// filterInScope keeps the candidates a reference from within namespace may
// legitimately reach.
//
// That is its own namespace, plus every external system. An external node is
// a third party -- api.stripe.com, an RDS endpoint, a legacy host -- and does
// not belong to a namespace at all, so scoping it out would drop exactly the
// dependencies a reader most wants to see. Namespaces partition the things a
// repository defines, not the things it calls.
func filterInScope(idx *Index, candidates []string, namespace string) []string {
	var out []string
	for _, id := range candidates {
		identity, ok := idx.Identity(id)
		if !ok {
			continue
		}
		if identity.Kind == schema.KindExternal || identity.Scope == namespace {
			out = append(out, id)
		}
	}
	return out
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

func withoutSelf(idx *Index, candidates []string, self string) []string {
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if id == self {
			continue
		}
		// A boundary is a grouping -- a system, a Compose project, a chart --
		// and takes part in the graph through contains edges only. Letting a
		// reference resolve to one produces a service that "depends on" the
		// very boundary enclosing it, which says nothing and reads as a real
		// finding. Charts routinely name the boundary and the service alike,
		// so this fires whenever a chart references its own release name.
		if identity, ok := idx.Identity(id); ok && identity.Kind == schema.KindBoundary {
			continue
		}
		out = append(out, id)
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
// outOfScopeDiagnostic reports a reference whose only candidates lived in
// another project or namespace.
//
// This is a refusal, and it has to be visible. The alternative that shipped
// before was an edge drawn across unrelated projects at the same confidence
// as a correct one -- which no confidence threshold protects a reader from,
// because the fabricated edge scores exactly as high as the real ones.
func outOfScopeDiagnostic(h Hint, m Match, from *Identity, names func(string) string) schema.Diagnostic {
	labels := make([]string, 0, len(m.Candidates))
	for _, id := range m.Candidates {
		labels = append(labels, names(id))
	}
	sort.Strings(labels)

	scope := scopeOf(from)
	return schema.Diagnostic{
		Severity: schema.SeverityInfo,
		Code:     "out_of_scope_reference",
		Path:     h.Source.Path,
		Line:     h.Source.Line,
		Message: fmt.Sprintf(
			"%q matches only %s, which %s outside %q; no edge was drawn, because a name "+
				"reused by an unrelated project is not a dependency",
			h.Raw, strings.Join(labels, ", "), plural(len(labels), "is", "are"), scope),
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

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
