package resolve

import (
	"fmt"
	"path"
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
// The order below matters. Files in one directory are folded first, because
// every step after it asks how many components live somewhere and gets the
// wrong answer while a codebase is still described twice; aliases run next
// because they are what makes a Service name resolvable at all; the join from
// a codebase to the container that runs it follows, so that references land on
// one component rather than two halves of one; directory-owned hints bind
// after merging, so that a
// .env file attaches to the component that survived it; ConfigMap indirection
// runs before matching so that values reached through a mount are available;
// and matching runs last, over a complete picture.
func Resolve(in Input) Result {
	r := &resolver{
		index:        NewIndex(in.Nodes),
		nodes:        cloneNodes(in.Nodes),
		edges:        append([]schema.Edge(nil), in.Edges...),
		byID:         map[string]*schema.Node{},
		merges:       map[string]string{},
		crossProject: map[string]bool{},
	}
	for i := range r.nodes {
		r.byID[r.nodes[i].ID] = &r.nodes[i]
	}

	r.mergeDirectoryDuplicates()
	r.reindexSurvivors()

	r.applyAliases(in.Aliases)
	r.mergeCodeIntoDeployments()

	hints := r.bindDirectoryHints(in.Hints)
	hints = r.expandConfigMaps(hints)
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
	// crossProject records basename joins refused because the two nodes live
	// in different project trees, keyed "code\x00deployment".
	crossProject map[string]bool
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
	// would connect a dev Service to a prod Deployment. It is scoped to its
	// project for the same reason one step up: two projects each declaring a
	// "prod" namespace are not one namespace.
	candidates = filterByProject(r.index, candidates, alias.Project)
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
	candidates := filterByProject(r.index, r.index.byName[strings.ToLower(alias.TargetName)], alias.Project)
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
	// Collected first, applied second. A join has to be unique in both
	// directions: one deployment claiming one codebase is a convention, but
	// ten deployments all named "web" claiming the same web/package.json is
	// a name collision between unrelated projects. Checking only that a
	// deployment found one codebase misses that entirely, and fuses two
	// sample stacks that share nothing.
	claims := map[string][]string{} // code node -> deployments claiming it
	byDirBase := r.codeByDirectoryBase()
	for _, identity := range r.index.AllIdentities() {
		if code, ok := r.codeFor(identity, byDirBase); ok {
			claims[code] = append(claims[code], identity.NodeID)
		}
	}

	for _, identity := range r.index.AllIdentities() {
		code, ok := r.codeFor(identity, byDirBase)
		if !ok || len(claims[code]) != 1 {
			continue
		}
		deployment := identity.NodeID
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
		contributeInto(deploymentNode, codeNode)
		r.merges[code] = deployment

		// Inferred joins are reported; a build context is not, because it
		// names the directory outright and infers nothing.
		if identity.BuildContext == "" {
			r.diags = append(r.diags, schema.Diagnostic{
				Severity: schema.SeverityInfo,
				Code:     "code_joined_to_deployment",
				Message: fmt.Sprintf(
					"%s and %s were treated as one component, because the workload is named "+
						"after the directory its code lives in; they are drawn as one box",
					r.displayName(code), r.displayName(deployment)),
			})
		}
	}

	r.reportCrossProjectJoins()
}

// reportCrossProjectJoins says which basename matches were refused.
//
// A refusal is not silence. The two nodes really do share a name, and a
// reader looking at two boxes that they believe are one needs to know the
// scan considered it and decided they were in different projects -- otherwise
// the only visible outcome is a duplicate they cannot explain.
//
// Only refusals that left the codebase unmerged are worth saying: if some
// other deployment in the right project claimed it, nothing is missing.
func (r *resolver) reportCrossProjectJoins() {
	keys := make([]string, 0, len(r.crossProject))
	for key := range r.crossProject {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		code, deployment, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if _, merged := r.merges[code]; merged {
			continue
		}
		r.diags = append(r.diags, schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "code_join_crosses_projects",
			Message: fmt.Sprintf(
				"%s and %s share a name but sit in different project trees, so they were "+
					"left as two components; if they are one, a build context or an image "+
					"the manifest names would say so",
				r.displayName(code), r.displayName(deployment)),
		})
	}
}

// codeByDirectoryBase indexes source components by the names a deployment
// might look them up under: the last segment of their directory, which is what
// a repository usually names after the service, and every name the component
// is otherwise known by.
//
// The directory alone is not enough, and the case that showed it is
// src/cartservice/src. A directory named for its role names no component, so
// the component is named after the directory above -- and indexing only the
// last segment left it filed under "src", where the Deployment called
// cartservice could never find it. The result was the service drawn twice,
// once as a workload and once as the code it runs.
//
// Only components that carry a directory and no image are considered: those
// are codebases. A deployment has an image and is the thing being merged
// into, never the thing merged away.
func (r *resolver) codeByDirectoryBase() map[string][]string {
	out := map[string][]string{}
	for _, id := range r.index.AllIdentities() {
		if id.Directory == "" || len(id.Images) > 0 {
			continue
		}
		keys := map[string]bool{}
		if base := strings.ToLower(path.Base(id.Directory)); base != "" && base != "." && base != "/" {
			keys[base] = true
		}
		for _, name := range id.Names {
			if lower := strings.ToLower(strings.TrimSpace(name)); lower != "" {
				keys[lower] = true
			}
		}

		// Sorted so that the lists this builds are in a fixed order whatever
		// the map handed back, which the uniqueness checks above depend on.
		sorted := make([]string, 0, len(keys))
		for key := range keys {
			sorted = append(sorted, key)
		}
		sort.Strings(sorted)
		for _, key := range sorted {
			out[key] = append(out[key], id.NodeID)
		}
	}
	return out
}

// codeFor finds the codebase that a deployment runs, if exactly one is
// identifiable.
//
// Two joins, in descending order of how much they prove:
//
// A Compose build context names the directory outright, so nothing is being
// inferred. Kubernetes has no equivalent -- a Deployment references an image,
// not a path -- so a manifest-only repository never merged at all, and the
// common case went unhandled: the graph drew every service twice, once as a
// container with no language and once as a codebase with no deployment.
//
// The second join closes that, on the convention a monorepo almost always
// follows: the image or workload is named after the directory the code lives
// in. src/checkoutservice builds the image checkoutservice. It is inference,
// so it demands a unique match and is reported as a diagnostic -- fusing two
// unrelated services into one box is worse than drawing two.
func (r *resolver) codeFor(deployment *Identity, byDirBase map[string][]string) (code string, ok bool) {
	if deployment.BuildContext != "" {
		candidates := r.index.NodesInDirectory(deployment.BuildContext)
		if len(candidates) == 1 && candidates[0] != deployment.NodeID {
			return candidates[0], true
		}
		return "", false
	}
	if len(deployment.Images) == 0 {
		return "", false
	}

	// The names this deployment might be known by: its own, and the base name
	// of each image it runs.
	keys := map[string]bool{}
	for _, name := range deployment.Names {
		keys[strings.ToLower(name)] = true
	}
	for _, image := range deployment.Images {
		keys[strings.ToLower(path.Base(image))] = true
	}

	var matches []string
	for key := range keys {
		for _, id := range byDirBase[key] {
			if id == deployment.NodeID {
				continue
			}
			// Filtered before the uniqueness check, not after: a candidate
			// from another project is not a candidate at all, and counting
			// it would let one unrelated stack suppress a correct join.
			if !r.sameProjectTree(deployment.NodeID, id) {
				r.crossProject[id+"\x00"+deployment.NodeID] = true
				continue
			}
			matches = append(matches, id)
		}
	}
	matches = dedupe(matches)
	if len(matches) != 1 {
		return "", false
	}

	return matches[0], true
}

// sameProjectTree reports whether a deployment and a codebase are near enough
// in the tree for "they share a directory name" to mean they are one thing.
//
// The basename join is inference on a very common word. "worker", "web",
// "api" and "app" name a directory in most repositories that have one, so in
// a repository holding more than one project the join will find a match in a
// project it has nothing to do with — and it did: a Compose service in one
// sample stack absorbed the package.json of an unrelated stack three
// directories away, and every dependency that codebase had moved with it.
//
// Two shapes are accepted, and both mean "one project":
//
// The file that declares the deployment sits at or above the code. A Compose
// file at the repository root, or a chart beside the service it deploys,
// governs everything beneath it.
//
// The two diverge at the repository root. This is how a single-project
// repository is laid out — deploy/ beside src/, infra/ beside services/ —
// and it is the case the basename join was written for.
//
// Anything else diverges below the root, which means a directory is grouping
// several things, and a name matched across that grouping is a coincidence.
//
// All of that is the fallback. A repository that declares its projects gets
// an exact answer instead, and none of these shapes are consulted.
// The cost of being wrong here is not symmetric: refusing a real join draws
// one component as two boxes, while accepting a false one silently moves a
// service's dependencies onto someone else's service.
//
// The known cost is a project nested inside a directory that also holds its
// code -- packages/a/deploy beside packages/a/src -- which loses the
// inference and is drawn as two boxes. A build context still joins it, and
// two boxes is the failure worth having.
func (r *resolver) sameProjectTree(deploymentID, codeID string) bool {
	codeNode, ok := r.byID[codeID]
	if !ok {
		return false
	}

	// A declared project boundary is the answer when there is one, and it is
	// exact: no path heuristic runs, and nothing below this can overrule it.
	// The path rules that follow are what a repository gets when it has not
	// said where its projects are, which is the default.
	if a, aok := r.index.Identity(deploymentID); aok {
		if b, bok := r.index.Identity(codeID); bok && a.Project != b.Project {
			return false
		} else if bok && a.Project != "." && a.Project == b.Project {
			return true
		}
	}
	codeDir, _ := codeNode.Attrs["directory"].(string)
	codeDir = normalizeDir(codeDir)

	declaring := r.declaringDirs(deploymentID)
	// A node with no recorded source cannot be placed, and refusing every
	// join on that basis would disable the inference rather than narrow it.
	if len(declaring) == 0 {
		return true
	}
	for _, dir := range declaring {
		if dir == codeDir || isAncestorDir(dir, codeDir) {
			return true
		}
		if commonDirPrefix(dir, codeDir) == 0 {
			return true
		}
	}
	return false
}

// declaringDirs returns the directories of the files that declared a node.
func (r *resolver) declaringDirs(nodeID string) []string {
	node, ok := r.byID[nodeID]
	if !ok {
		return nil
	}
	var out []string
	for _, s := range node.Sources {
		if s.Path == "" {
			continue
		}
		out = appendUnique(out, normalizeDir(path.Dir(s.Path)))
	}
	return out
}

// isAncestorDir reports whether dir contains child, with "." meaning the
// repository root and therefore containing everything.
func isAncestorDir(dir, child string) bool {
	if dir == "." {
		return true
	}
	return strings.HasPrefix(child, dir+"/")
}

// commonDirPrefix counts the leading path segments two directories share. A
// count of zero means they diverge at the repository root.
func commonDirPrefix(a, b string) int {
	if a == "." || b == "." {
		return 0
	}
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
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

// bindDirectoryHints attaches hints carried by a file that is not a component
// to the component whose directory it sits in.
//
// A .env file is the case this exists for. It states what a service talks to
// without naming the service, because the service is implied by where the
// file is. The extractor cannot resolve that — it sees one file — so it
// records the directory and leaves the binding until the node set is
// complete.
//
// Attaching to the wrong owner would be worse than not attaching at all: the
// dependencies of one service would appear on another. So a directory that
// holds no component, or more than one, produces a diagnostic and no edge.
func (r *resolver) bindDirectoryHints(hints []Hint) []Hint {
	out := make([]Hint, 0, len(hints))
	// One diagnostic per file, not per variable: a .env file with six
	// references in an unowned directory has one problem, not six.
	reported := map[string]bool{}
	// Indexed once rather than per hint: a monorepo with a .env file beside
	// every service would otherwise walk the whole node set for each variable
	// in each of them.
	byBuildContext := r.buildContextIndex()
	owned := map[string][]string{}

	for _, h := range hints {
		if h.FromNode != "" || h.OwnerDir == "" {
			out = append(out, h)
			continue
		}

		dir := normalizeDir(h.OwnerDir)
		owners, cached := owned[dir]
		if !cached {
			owners = r.componentsOwning(dir, byBuildContext)
			owned[dir] = owners
		}
		switch len(owners) {
		case 1:
			h.FromNode = owners[0]
			out = append(out, h)
		case 0:
			if !reported[h.Source.Path] {
				reported[h.Source.Path] = true
				r.diag(schema.SeverityInfo, "env_file_unowned", h.Source.Path, 0,
					fmt.Sprintf("no component is declared in %q, so the references in this file "+
						"were not attached to anything; a manifest or an image in that directory "+
						"is what identifies the service they belong to", h.OwnerDir))
			}
		default:
			if !reported[h.Source.Path] {
				reported[h.Source.Path] = true
				r.diag(schema.SeverityWarn, "env_file_ambiguous_owner", h.Source.Path, 0,
					fmt.Sprintf("%d components are declared in %q (%s), so there is no single "+
						"service these references belong to and none were attached",
						len(owners), h.OwnerDir, strings.Join(r.displayNames(owners), ", ")))
			}
		}
	}
	return out
}

// componentsOwning returns the components whose code lives in a directory.
//
// Both halves matter. A language manifest records the directory it was read
// from, which is how a repository with no orchestrator is found at all. A
// container records the build context it is built from, which is how a .env
// file beside a Dockerfile reaches the service that Compose declares
// elsewhere. Merged-away nodes are followed to their survivor so that the
// hint lands on the box the graph will actually draw.
func (r *resolver) componentsOwning(dir string, byBuildContext map[string][]string) []string {
	var owners []string
	add := func(nodeID string) {
		if merged, ok := r.merges[nodeID]; ok {
			nodeID = merged
		}
		node, ok := r.byID[nodeID]
		// Only something that runs can hold a reference. A datastore
		// declared in the same directory is not the owner of its neighbour's
		// configuration.
		if !ok || node.Kind != schema.KindService {
			return
		}
		owners = appendUnique(owners, nodeID)
	}

	for _, nodeID := range r.index.NodesInDirectory(dir) {
		add(nodeID)
	}
	for _, nodeID := range byBuildContext[dir] {
		add(nodeID)
	}
	sort.Strings(owners)
	return owners
}

// buildContextIndex groups nodes by the normalized directory their image is
// built from.
func (r *resolver) buildContextIndex() map[string][]string {
	out := map[string][]string{}
	for _, identity := range r.index.AllIdentities() {
		if identity.BuildContext == "" {
			continue
		}
		dir := normalizeDir(identity.BuildContext)
		out[dir] = appendUnique(out[dir], identity.NodeID)
	}
	return out
}

// normalizeDir collapses the ways a repository-relative directory gets
// written: "", ".", "./web", and "web/" are all one place.
func normalizeDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "."
	}
	return path.Clean(dir)
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

	if candidates := withoutSelf(r.index, r.index.byTech[tech], h.FromNode); len(candidates) == 1 {
		target := candidates[0]
		// Corroborate. If nothing already connects these two, a technology
		// name is not enough to say they are connected.
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

	// A technology that is one company can be drawn on its own. Which
	// project, tenant or region is unknown, and that is the same thing the
	// resolver already accepts when it synthesizes api.stripe.com from a URL:
	// the company is the component, and the account is a detail no
	// configuration file states either.
	if !h.Vendor {
		return
	}
	if reached := r.reachesVendor(h.FromNode, tech); reached != "" {
		// Configuration named the endpoint, which is strictly better than the
		// dependency knowing the vendor. Corroborate it and draw nothing.
		for i := range r.edges {
			if r.edges[i].From != h.FromNode || r.edges[i].To != reached {
				continue
			}
			evidence := h.Source
			evidence.Rule = "library_corroborates"
			r.edges[i].Evidence = append(r.edges[i].Evidence, evidence)
			return
		}
		return
	}

	vendor := h.Vendor
	h.Vendor = false
	// The protocol field holds the technology name for a library hint, which
	// would render as "over supabase". These are all HTTPS APIs.
	h.Protocol = "https"
	id := r.ensureVendor(tech, h.Source)
	r.addEdge(h, Match{
		NodeID:     id,
		Rule:       "dependency_names_vendor",
		Confidence: h.Kind.BaseConfidence(),
	})
	h.Vendor = vendor
}

// reachesVendor returns the node this component already reaches that stands
// for a vendor, or "" if there is none.
//
// A managed service's hostname contains its name -- *.supabase.co,
// *.upstash.io, *.auth0.com -- so an endpoint read out of a .env file or a pod
// spec is recognizable as the same system the dependency names. Finding one
// means the graph already has the better answer, with the account in it.
func (r *resolver) reachesVendor(from, tech string) string {
	for _, e := range r.edges {
		if e.From != from {
			continue
		}
		target := r.byID[e.To]
		if target == nil {
			continue
		}
		if strings.Contains(strings.ToLower(target.Name), tech) {
			return e.To
		}
	}
	return ""
}

// ensureVendor returns the node for a third-party company, creating it once.
//
// It is deliberately not ensureExternal. That function takes a hostname and
// records one, because something named an endpoint. Here nothing did: a
// dependency names the company and stays silent about where it is reached.
// Giving the node a plausible-looking hostname would be inventing the one
// fact the evidence does not support.
func (r *resolver) ensureVendor(tech string, source schema.Evidence) string {
	id := schema.NewNodeID(schema.KindExternal, "", tech)
	if _, exists := r.byID[id]; exists {
		return id
	}

	node := schema.Node{
		ID:        id,
		Kind:      schema.KindExternal,
		Layer:     schema.LayerContext,
		Name:      tech,
		Namespace: "vendor",
		Attrs:     schema.Attrs{"vendor": tech},
		Sources:   []schema.Source{{Extractor: source.Extractor, Path: source.Path, Line: source.Line}},
		// The dependency is in the manifest beyond doubt. What is inferred is
		// that the service reaches this company over the network at all.
		Confidence: schema.ConfWeak,
	}
	r.nodes = append(r.nodes, node)
	r.byID = rebuildByID(r.nodes)
	r.index.add(identityOf(node))
	return id
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
	id := schema.NewNodeID(schema.KindExternal, "", host)
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

// reindexSurvivors rebuilds the identity index over the nodes that are still
// standing.
//
// A merge recorded early has to be visible to the steps that follow, and they
// read the index rather than the node list: leaving an absorbed codebase in it
// would leave every later question about "how many components are in this
// directory" answered as it was before the fold, which is the thing the fold
// exists to correct.
//
// Nothing has attached to the index yet at this point -- aliases run after --
// so rebuilding loses no accumulated knowledge.
func (r *resolver) reindexSurvivors() {
	if len(r.merges) == 0 {
		return
	}
	surviving := make([]schema.Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if _, merged := r.merges[n.ID]; !merged {
			surviving = append(surviving, n)
		}
	}
	r.index = NewIndex(surviving)
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

func (r *resolver) diag(severity schema.Severity, code, file string, line int, message string) {
	r.diags = append(r.diags, schema.Diagnostic{
		Severity: severity, Code: code, Path: file, Line: line, Message: message,
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
