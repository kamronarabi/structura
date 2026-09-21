package resolve

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Input is everything the extractors produced.
type Input struct {
	Nodes   []schema.Node
	Edges   []schema.Edge
	Hints   []Hint
	Aliases []Alias
}

// Result is the graph content after resolution.
type Result struct {
	Nodes       []schema.Node
	Edges       []schema.Edge
	Diagnostics []schema.Diagnostic
}

// Resolve turns unresolved references into edges.
//
// This is the step that decides whether the output is an architecture map or
// a pile of labelled boxes. Configuration files state what exists; they
// almost never state what calls what. No field in a Kubernetes manifest says
// "api-gateway calls user-service" — that has to be recovered by matching
// names across artifacts, and doing it well is most of the product.
//
// The order below matters. Aliases run first because they are what makes a
// Service name resolvable at all; node merging runs next so that references
// land on one component rather than two halves of one; ConfigMap indirection
// runs before matching so that values reached through a mount are available;
// and matching runs last, over a complete picture.
func Resolve(in Input) Result {
	r := &resolver{
		index:  NewIndex(in.Nodes),
		nodes:  cloneNodes(in.Nodes),
		edges:  append([]schema.Edge(nil), in.Edges...),
		byID:   map[string]*schema.Node{},
		merges: map[string]string{},
	}
	for i := range r.nodes {
		r.byID[r.nodes[i].ID] = &r.nodes[i]
	}

	r.applyAliases(in.Aliases)
	r.mergeCodeIntoDeployments()

	hints := r.expandConfigMaps(in.Hints)
	r.matchHints(hints)

	r.applyMerges()
	r.collapseGenericEdges()
	return Result{Nodes: r.nodes, Edges: r.edges, Diagnostics: r.diags}
}

type resolver struct {
	index *Index
	nodes []schema.Node
	edges []schema.Edge
	diags []schema.Diagnostic

	byID map[string]*schema.Node
	// merges maps a node that turned out to be the same component as another
	// onto the one that survives.
	merges map[string]string
}

// applyAliases attaches names that route to a node, and synthesizes external
// nodes for names that route out of the cluster.
func (r *resolver) applyAliases(aliases []Alias) {
	sorted := append([]Alias(nil), aliases...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})

	for _, alias := range sorted {
		switch {
		case alias.External != "":
			r.externalAlias(alias)
		case len(alias.Selector) > 0:
			r.selectorAlias(alias)
		case alias.TargetName != "":
			r.namedAlias(alias)
		}
	}
}

// selectorAlias resolves a Kubernetes Service to the workload it selects.
func (r *resolver) selectorAlias(alias Alias) {
	candidates := r.index.NodesBySelector(alias.Selector)
	// A selector is scoped to its namespace; matching across namespaces
	// would connect a dev Service to a prod Deployment.
	candidates = filterByNamespace(r.index, candidates, alias.Namespace)

	switch len(candidates) {
	case 0:
		r.diag(schema.SeverityInfo, "unmatched_service_selector", alias.Source.Path, alias.Source.Line,
			fmt.Sprintf("Service %q selects %s, which matches no workload in this repository",
				alias.Name, formatSelector(alias.Selector)))
	case 1:
		r.index.AttachDNS(candidates[0], alias.DNS...)
		r.index.AttachPorts(candidates[0], alias.Ports...)
	default:
		// A Service fronting several workloads is legitimate — a canary
		// deployment, a blue/green pair — but it means a reference to the
		// Service name does not identify one component.
		r.diag(schema.SeverityInfo, "service_selects_many", alias.Source.Path, alias.Source.Line,
			fmt.Sprintf("Service %q selects %d workloads (%s); references to it were left unresolved",
				alias.Name, len(candidates), strings.Join(r.displayNames(candidates), ", ")))
	}
}

// namedAlias resolves an alias that names its target outright.
func (r *resolver) namedAlias(alias Alias) {
	candidates := r.index.byName[strings.ToLower(alias.TargetName)]
	if scoped := filterByNamespace(r.index, candidates, alias.Namespace); len(scoped) > 0 {
		candidates = scoped
	}
	if len(candidates) == 1 {
		r.index.AttachDNS(candidates[0], alias.DNS...)
		r.index.AttachPorts(candidates[0], alias.Ports...)
	}
}

// externalAlias creates the node an ExternalName Service points at.
func (r *resolver) externalAlias(alias Alias) {
	id := r.ensureExternal(alias.External, alias.Source)
	r.index.AttachDNS(id, alias.DNS...)
}

// mergeCodeIntoDeployments folds a language manifest's node into the
// container that runs it.
//
// A Compose file saying the image for ./checkout is built here, and a go.mod
// in ./checkout saying it is a module called acme/checkout, describe one
// component. Left alone they are two nodes: a container with no language and
// a codebase with no deployment, and every reference resolves to whichever
// happens to be indexed. Joining them on the build context is what lets the
// graph say a service is Go rather than leaving it an anonymous container.
func (r *resolver) mergeCodeIntoDeployments() {
	for _, identity := range r.index.AllIdentities() {
		if identity.BuildContext == "" {
			continue
		}
		candidates := r.index.NodesInDirectory(identity.BuildContext)
		if len(candidates) != 1 || candidates[0] == identity.NodeID {
			continue
		}

		code, deployment := candidates[0], identity.NodeID
		codeNode, ok := r.byID[code]
		if !ok {
			continue
		}
		deploymentNode, ok := r.byID[deployment]
		if !ok {
			continue
		}

		// The deployment survives: it is the thing that runs, and it is what
		// other infrastructure references. The codebase contributes what
		// only it knows.
		if codeNode.Tech != nil {
			if deploymentNode.Tech == nil {
				deploymentNode.Tech = &schema.Tech{}
			}
			if deploymentNode.Tech.Language == "" {
				deploymentNode.Tech.Language = codeNode.Tech.Language
			}
			if deploymentNode.Tech.Framework == "" {
				deploymentNode.Tech.Framework = codeNode.Tech.Framework
			}
		}
		for _, key := range []string{"module", "package", "manifest", "directory", "dependencyCount",
			"goVersion", "nodeVersion", "pythonVersion", "version"} {
			if v, ok := codeNode.Attrs[key]; ok {
				if deploymentNode.Attrs == nil {
					deploymentNode.Attrs = schema.Attrs{}
				}
				if _, present := deploymentNode.Attrs[key]; !present {
					deploymentNode.Attrs[key] = v
				}
			}
		}
		deploymentNode.Sources = append(deploymentNode.Sources, codeNode.Sources...)
		r.merges[code] = deployment
	}
}

// expandConfigMaps transfers a ConfigMap's references onto the workloads that
// consume it.
//
// A pod that mounts a ConfigMap inherits whatever that ConfigMap points at,
// but neither file knows about the other: the ConfigMap holds the values, the
// pod holds the reference to it. Joining them is the indirection the
// confidence table prices at 0.70 — real, but a step removed from the pod
// declaring the endpoint itself.
func (r *resolver) expandConfigMaps(hints []Hint) []Hint {
	owned := map[string][]Hint{}
	var direct []Hint
	var uses []Hint

	for _, h := range hints {
		switch {
		case strings.HasPrefix(h.FromNode, "configmap:"):
			owned[h.FromNode] = append(owned[h.FromNode], h)
		case h.Kind == HintConfigValue && strings.HasPrefix(h.Raw, "configmap:"):
			uses = append(uses, h)
		default:
			direct = append(direct, h)
		}
	}

	for _, use := range uses {
		for _, value := range owned[use.Raw] {
			expanded := value
			expanded.FromNode = use.FromNode
			expanded.Kind = HintConfigValue
			expanded.Source = schema.Evidence{
				Extractor: use.Source.Extractor,
				Path:      use.Source.Path,
				Line:      use.Source.Line,
				Rule:      "configmap_indirection",
				Detail:    use.Source.Detail + "; " + value.Source.Detail,
			}
			direct = append(direct, expanded)
		}
	}
	return direct
}

// matchHints is the main loop: every unresolved reference either becomes an
// edge, becomes an external node, or is reported.
func (r *resolver) matchHints(hints []Hint) {
	sorted := append([]Hint(nil), hints...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].FromNode != sorted[j].FromNode {
			return sorted[i].FromNode < sorted[j].FromNode
		}
		if sorted[i].Source.Path != sorted[j].Source.Path {
			return sorted[i].Source.Path < sorted[j].Source.Path
		}
		if sorted[i].Source.Line != sorted[j].Source.Line {
			return sorted[i].Source.Line < sorted[j].Source.Line
		}
		return sorted[i].Raw < sorted[j].Raw
	})

	// Two passes. Everything that can establish an edge runs first, then the
	// library hints, which only corroborate — a dependency cannot strengthen
	// an edge that has not been drawn yet.
	var libraries []Hint
	for _, h := range sorted {
		if h.FromNode == "" || r.byID[h.FromNode] == nil {
			continue
		}
		if h.Kind == HintLibrary {
			libraries = append(libraries, h)
			continue
		}

		from, _ := r.index.Identity(h.FromNode)
		match, ok := r.index.Resolve(h, from)
		switch {
		case ok:
			r.addEdge(h, match)
		case len(match.Candidates) > 1:
			// Several components answer to the name. That is the dominant
			// fact and the actionable one -- the reference has to be
			// qualified -- whether or not they also sit outside the
			// referrer's scope.
			r.diags = append(r.diags, ambiguityDiagnostic(h, match, r.displayName))
		case match.OutOfScope:
			// Exactly one component answers, and it belongs to someone else.
			// Refused rather than drawn, and said out loud: silence would
			// leave the reader believing the component has no such
			// dependency, when in fact one was found and rejected.
			r.diags = append(r.diags, outOfScopeDiagnostic(h, match, from, r.displayName))
		default:
			r.unmatched(h)
		}
	}
	for _, h := range libraries {
		r.matchLibrary(h)
	}
}

// matchLibrary records what a dependency implies, without inventing a
// relationship.
//
// A Postgres driver in go.mod says this service talks to a Postgres. It never
// says which one. Turning that into an edge to whichever Postgres happens to
// be in the repository is exactly the move the confidence model exists to
// prevent: the edge looks like a finding, reads as a fact to the model
// downstream, and is a guess.
//
// So a library corroborates an edge that was established some other way —
// raising its confidence and adding its evidence — and otherwise lands on the
// node as a recorded technology. The information survives; the fabrication
// does not.
func (r *resolver) matchLibrary(h Hint) {
	if len(h.Tokens) == 0 {
		return
	}
	tech := h.Tokens[0]

	from := r.byID[h.FromNode]
	if from == nil {
		return
	}
	if from.Attrs == nil {
		from.Attrs = schema.Attrs{}
	}
	existing, _ := from.Attrs["usesTechnology"].([]string)
	from.Attrs["usesTechnology"] = dedupe(append(existing, tech))

	candidates := withoutSelf(r.index, r.index.byTech[tech], h.FromNode)
	if len(candidates) != 1 {
		return
	}
	target := candidates[0]

	// Corroborate only. If nothing already connects these two, the library
	// is not enough to say they are connected.
	for i := range r.edges {
		if r.edges[i].From != h.FromNode || r.edges[i].To != target {
			continue
		}
		evidence := h.Source
		evidence.Rule = "library_corroborates"
		r.edges[i].Evidence = append(r.edges[i].Evidence, evidence)
		return
	}
}

func (r *resolver) addEdge(h Hint, m Match) {
	target := r.byID[m.NodeID]
	if target == nil {
		return
	}
	kind := edgeKindFor(h.SuggestedEdge, target.Kind)

	evidence := h.Source
	evidence.Rule = m.Rule
	if evidence.Detail == "" {
		evidence.Detail = fmt.Sprintf("%q resolves to %s", h.Raw, target.Name)
	} else {
		evidence.Detail += fmt.Sprintf(" (resolves to %s)", target.Name)
	}

	r.edges = append(r.edges, schema.Edge{
		From:       h.FromNode,
		To:         m.NodeID,
		Kind:       kind,
		Protocol:   h.Protocol,
		Confidence: m.Confidence,
		Evidence:   []schema.Evidence{evidence},
	})
}

// unmatched decides what a reference that resolved to nothing means.
func (r *resolver) unmatched(h Hint) {
	if len(h.Tokens) == 0 {
		return
	}
	host := h.Tokens[0]

	// A name with a public suffix is a third-party system: api.stripe.com,
	// sqs.us-east-1.amazonaws.com. Those belong on the context diagram, and
	// synthesizing them is how the outer ring gets populated.
	if isExternalHost(host) {
		id := r.ensureExternal(host, h.Source)
		r.addEdge(h, Match{
			NodeID:     id,
			Rule:       "external_host",
			Confidence: h.Kind.BaseConfidence(),
		})
		return
	}

	// A bare name that matches nothing is more likely a component this scan
	// failed to find than a system on the internet. Inventing an external
	// node for it would put a fictional third party on the diagram.
	r.diag(schema.SeverityInfo, "unresolved_reference", h.Source.Path, h.Source.Line,
		fmt.Sprintf("%q refers to %q, which matches no component in this repository",
			r.displayName(h.FromNode), host))
}

// ensureExternal returns the node for a third-party host, creating it once.
func (r *resolver) ensureExternal(host string, source schema.Evidence) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	id := schema.NewNodeID(schema.KindExternal, "resolver", "net", host)
	if _, exists := r.byID[id]; exists {
		return id
	}

	node := schema.Node{
		ID:        id,
		Kind:      schema.KindExternal,
		Layer:     schema.LayerContext,
		Name:      host,
		Namespace: "net",
		Attrs:     schema.Attrs{"hostname": host},
		Sources:   []schema.Source{{Extractor: source.Extractor, Path: source.Path, Line: source.Line}},
		// The host is written down in the repository; what is inferred is
		// only that it is outside the system.
		Confidence: schema.ConfHostMatch,
	}
	r.nodes = append(r.nodes, node)
	r.byID = rebuildByID(r.nodes)
	r.index.add(identityOf(node))
	r.index.AttachDNS(id, host)
	return id
}

// applyMerges rewrites references to merged-away nodes and drops them.
func (r *resolver) applyMerges() {
	if len(r.merges) == 0 {
		return
	}
	resolveID := func(id string) string {
		// Merge chains are at most one link deep by construction, but
		// following them costs nothing and removes the assumption.
		for range 8 {
			next, ok := r.merges[id]
			if !ok {
				return id
			}
			id = next
		}
		return id
	}

	kept := r.nodes[:0]
	for _, n := range r.nodes {
		if _, merged := r.merges[n.ID]; !merged {
			kept = append(kept, n)
		}
	}
	r.nodes = kept

	edges := r.edges[:0]
	for _, e := range r.edges {
		e.From, e.To = resolveID(e.From), resolveID(e.To)
		// A merge can turn an edge between two halves of one component into
		// a self-loop, which is not a relationship.
		if e.From != e.To {
			edges = append(edges, e)
		}
	}
	r.edges = edges
}

// collapseGenericEdges folds a depends_on into a more specific relationship
// between the same pair.
//
// A Compose file declaring "checkout depends_on orders-db" and a DATABASE_URL
// pointing at the same database are two readings of one relationship. Showing
// both makes the graph look like there are two, and forces a reader to work
// out that "depends_on" and "persists_to" here are the same arrow. The
// specific kind survives, the declaration's evidence comes with it, and the
// confidence becomes the higher of the two — a relationship stated outright
// and corroborated by a connection string is as certain as it gets.
func (r *resolver) collapseGenericEdges() {
	type pair struct{ from, to string }

	specific := map[pair]int{}
	for i, e := range r.edges {
		if e.Kind != schema.EdgeDependsOn && e.Kind != schema.EdgeContains {
			specific[pair{e.From, e.To}] = i
		}
	}
	if len(specific) == 0 {
		return
	}

	kept := r.edges[:0]
	for _, e := range r.edges {
		if e.Kind == schema.EdgeDependsOn {
			if i, ok := specific[pair{e.From, e.To}]; ok {
				r.edges[i].Evidence = append(r.edges[i].Evidence, e.Evidence...)
				if e.Confidence > r.edges[i].Confidence {
					r.edges[i].Confidence = e.Confidence
				}
				continue
			}
		}
		kept = append(kept, e)
	}
	r.edges = kept
}

func (r *resolver) diag(severity schema.Severity, code, path string, line int, message string) {
	r.diags = append(r.diags, schema.Diagnostic{
		Severity: severity, Code: code, Path: path, Line: line, Message: message,
	})
}

func (r *resolver) displayName(nodeID string) string {
	if n, ok := r.byID[nodeID]; ok {
		if n.Namespace != "" {
			return n.Name + "." + n.Namespace
		}
		return n.Name
	}
	return nodeID
}

func (r *resolver) displayNames(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = r.displayName(id)
	}
	sort.Strings(out)
	return out
}

// isExternalHost reports whether a hostname belongs to the public internet
// rather than to something the repository might declare.
func isExternalHost(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	// A cluster-internal suffix is emphatically not external.
	switch last {
	case "local", "internal", "cluster", "svc", "localdomain":
		return false
	}
	return isPublicSuffix(last)
}

func formatSelector(selector map[string]string) string {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + selector[k]
	}
	return strings.Join(parts, ",")
}

func cloneNodes(in []schema.Node) []schema.Node {
	out := make([]schema.Node, len(in))
	for i, n := range in {
		out[i] = n
		if n.Attrs != nil {
			attrs := make(schema.Attrs, len(n.Attrs))
			for k, v := range n.Attrs {
				attrs[k] = v
			}
			out[i].Attrs = attrs
		}
		if n.Tech != nil {
			tech := *n.Tech
			out[i].Tech = &tech
		}
		out[i].Sources = append([]schema.Source(nil), n.Sources...)
	}
	return out
}

func rebuildByID(nodes []schema.Node) map[string]*schema.Node {
	out := make(map[string]*schema.Node, len(nodes))
	for i := range nodes {
		out[nodes[i].ID] = &nodes[i]
	}
	return out
}
