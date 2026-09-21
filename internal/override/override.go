// Package override applies the corrections a repository has written down.
//
// Everything else in Structura is inference over configuration files, and
// inference is wrong sometimes. Until now there was nothing a person could do
// about it. A false edge stayed; a component drawn as two boxes stayed two; a
// dependency that exists only in application code -- which a configuration
// scan cannot see by construction -- could not be added at all. The only
// remedies were to change the infrastructure files or to live with it.
//
// The projects key was the first of these: a fact about the repository that
// no file states and no heuristic can recover, written down once and then
// obeyed exactly. This is the general form. A rule here is not a hint and not
// a weighting; it is the answer, and the inference that disagreed with it
// loses.
//
// Three things follow from that.
//
// A declared relationship carries confidence 1.00 and evidence naming the
// configuration file, because it is declared -- by a person, which is a
// better source than a matched string. A reader asking "why is this arrow
// here" gets "someone said so, for this reason", which is truthful and
// checkable.
//
// A rule that matches nothing is reported, and loudly. A correction that
// silently does nothing is worse than no correction: the person believes the
// graph has been fixed, stops looking, and the stale rule survives every
// refactor that made it stale.
//
// Nothing here guesses. A reference that could mean two components is refused
// and reported rather than resolved to one of them, on the same reasoning as
// everywhere else: a wrong edge is invisible to the person who would correct
// it.
package override

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// SourcePath is what evidence records as the origin of a declared fact. It is
// the config file's conventional name rather than the path actually loaded,
// which may be absolute and must never reach a committed graph.
const SourcePath = ".structura.yaml"

// Rules are the corrections a repository declares.
type Rules struct {
	Relationships []Relationship `mapstructure:"relationships"`
	Components    []Component    `mapstructure:"components"`
}

// Relationship declares an edge that configuration does not express, or
// removes one that inference got wrong.
type Relationship struct {
	From string `mapstructure:"from"`
	To   string `mapstructure:"to"`
	// Kind is an edge kind such as calls or persists_to. Empty defaults to
	// depends_on, the weakest claim that is still a relationship.
	Kind string `mapstructure:"kind"`
	// Protocol is optional and descriptive.
	Protocol string `mapstructure:"protocol"`
	// Remove deletes the matching edge instead of adding one.
	Remove bool `mapstructure:"remove"`
	// Note is why, and is carried into the evidence. It is the most useful
	// field here and the one a reader of the graph actually needs.
	Note string `mapstructure:"note"`
}

// Component declares facts about nodes that inference got wrong.
type Component struct {
	// Same names components that are one component. The first survives and
	// the rest are folded into it, as the resolver folds a codebase into the
	// container that runs it.
	Same []string `mapstructure:"same"`
	Note string   `mapstructure:"note"`
}

// Empty reports whether there is nothing to apply.
func (r Rules) Empty() bool { return len(r.Relationships) == 0 && len(r.Components) == 0 }

// Result is the corrected node and edge set.
type Result struct {
	Nodes []schema.Node
	Edges []schema.Edge
}

// Apply rewrites a node and edge set according to the rules, returning the
// corrected set and whatever could not be done.
//
// It works on the sets rather than on a finished graph so that the corrected
// content is what gets hashed, sorted, and validated, instead of being patched
// into a graph that was already declared complete.
func Apply(nodes []schema.Node, edges []schema.Edge, rules Rules) (Result, []schema.Diagnostic) {
	result := Result{Nodes: nodes, Edges: edges}
	if rules.Empty() {
		return result, nil
	}
	a := &applier{nodes: nodes, edges: edges, index: newIndex(nodes)}

	// Merges first: a relationship naming a component that is about to be
	// folded away should land on the node that survives.
	for _, c := range rules.Components {
		a.merge(c)
	}
	for _, rel := range rules.Relationships {
		a.relationship(rel)
	}
	a.dropOrphanedExternals()
	return Result{Nodes: a.nodes, Edges: a.edges}, a.diags
}

type applier struct {
	nodes []schema.Node
	edges []schema.Edge
	index *index
	diags []schema.Diagnostic
}

func (a *applier) diag(severity schema.Severity, code, message string) {
	a.diags = append(a.diags, schema.Diagnostic{
		Severity: severity,
		Code:     code,
		Path:     SourcePath,
		Message:  message,
	})
}

// merge folds several nodes into the first one named.
func (a *applier) merge(c Component) {
	if len(c.Same) < 2 {
		if len(c.Same) == 1 {
			a.diag(schema.SeverityWarn, "override_incomplete",
				fmt.Sprintf("%q is the only component listed under \"same\"; two or more are needed to say they are one thing",
					c.Same[0]))
		}
		return
	}

	ids := make([]string, 0, len(c.Same))
	for _, ref := range c.Same {
		id, ok := a.resolve(ref, "same")
		if !ok {
			return
		}
		ids = append(ids, id)
	}

	survivor := ids[0]
	folded := map[string]bool{}
	for _, id := range ids[1:] {
		if id != survivor {
			folded[id] = true
		}
	}
	if len(folded) == 0 {
		a.diag(schema.SeverityInfo, "override_no_effect",
			fmt.Sprintf("every component listed under \"same\" resolves to %s, so there was nothing to merge",
				a.index.display(survivor)))
		return
	}

	kept := make([]schema.Node, 0, len(a.nodes))
	for _, n := range a.nodes {
		if !folded[n.ID] {
			kept = append(kept, n)
			continue
		}
		// What the folded node knew is worth keeping -- where it was declared
		// especially, or the merge erases the file that produced it.
		for i := range kept {
			if kept[i].ID != survivor {
				continue
			}
			kept[i].Sources = append(kept[i].Sources, n.Sources...)
			if kept[i].Tech == nil {
				kept[i].Tech = n.Tech
			}
			for k, v := range n.Attrs {
				if kept[i].Attrs == nil {
					kept[i].Attrs = schema.Attrs{}
				}
				if _, present := kept[i].Attrs[k]; !present {
					kept[i].Attrs[k] = v
				}
			}
		}
	}
	a.nodes = kept

	edges := make([]schema.Edge, 0, len(a.edges))
	for _, e := range a.edges {
		if folded[e.From] {
			e.From = survivor
		}
		if folded[e.To] {
			e.To = survivor
		}
		// A merge routinely turns an edge between the two halves into an edge
		// from a node to itself, which is not a relationship.
		if e.From == e.To {
			continue
		}
		edges = append(edges, e)
	}
	a.edges = edges
	a.index = newIndex(a.nodes)
}

// dropOrphanedExternals removes third-party systems nothing points at any
// more.
//
// An external node exists only because a reference resolved to it; the
// resolver synthesizes it on the strength of that one edge. Remove the edge
// and what is left is a box asserting that a system is part of the
// architecture, with nothing connecting it to anything -- which is the
// opposite of what someone deleting the edge meant.
//
// Only externals are collected. A service with no detected relationships is a
// real finding: it exists, and the scan found nothing it talks to.
func (a *applier) dropOrphanedExternals() {
	referenced := make(map[string]bool, len(a.edges)*2)
	for _, e := range a.edges {
		referenced[e.From] = true
		referenced[e.To] = true
	}

	kept := make([]schema.Node, 0, len(a.nodes))
	for _, n := range a.nodes {
		if n.Kind == schema.KindExternal && !referenced[n.ID] {
			continue
		}
		kept = append(kept, n)
	}
	a.nodes = kept
}

// relationship adds or removes one edge.
func (a *applier) relationship(rel Relationship) {
	from, ok := a.resolve(rel.From, "from")
	if !ok {
		return
	}
	to, ok := a.resolve(rel.To, "to")
	if !ok {
		return
	}
	if from == to {
		a.diag(schema.SeverityWarn, "override_unresolved",
			fmt.Sprintf("%q and %q are the same component, so no relationship between them can be declared",
				rel.From, rel.To))
		return
	}

	kind := schema.EdgeDependsOn
	if rel.Kind != "" {
		parsed := schema.EdgeKind(strings.ToLower(strings.TrimSpace(rel.Kind)))
		if !parsed.Valid() {
			a.diag(schema.SeverityWarn, "override_unknown_kind",
				fmt.Sprintf("%q is not a relationship kind; use one of %s", rel.Kind, edgeKindList()))
			return
		}
		kind = parsed
	}

	if rel.Remove {
		a.remove(rel, from, to, kind)
		return
	}
	a.add(rel, from, to, kind)
}

func (a *applier) remove(rel Relationship, from, to string, kind schema.EdgeKind) {
	kept := make([]schema.Edge, 0, len(a.edges))
	removed := 0
	for _, e := range a.edges {
		// An unspecified kind removes whatever relationship is there, which
		// is what someone deleting a wrong arrow means.
		if e.From == from && e.To == to && (rel.Kind == "" || e.Kind == kind) {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	if removed == 0 {
		// Most likely the scan stopped drawing it, which makes the rule stale
		// rather than wrong. Either way a person who believes a rule is
		// suppressing something needs to know that it is not.
		a.diag(schema.SeverityWarn, "override_no_effect",
			fmt.Sprintf("no relationship from %s to %s was found to remove; the rule may be left over from a scan that drew one",
				a.index.display(from), a.index.display(to)))
		return
	}
	a.edges = kept
}

func (a *applier) add(rel Relationship, from, to string, kind schema.EdgeKind) {
	for i, e := range a.edges {
		if e.From != from || e.To != to || e.Kind != kind {
			continue
		}
		// Inference already found it. The declaration is still the better
		// source, so it raises the confidence and joins the evidence rather
		// than being dropped as redundant.
		a.edges[i].Confidence = schema.ConfDeclared
		a.edges[i].Evidence = append(a.edges[i].Evidence, evidenceFor(rel))
		return
	}

	a.edges = append(a.edges, schema.Edge{
		From:       from,
		To:         to,
		Kind:       kind,
		Protocol:   rel.Protocol,
		Confidence: schema.ConfDeclared,
		Evidence:   []schema.Evidence{evidenceFor(rel)},
	})
}

func evidenceFor(rel Relationship) schema.Evidence {
	detail := strings.TrimSpace(rel.Note)
	if detail == "" {
		// Without a reason the evidence says only that the edge is declared,
		// which the confidence already says.
		detail = "declared in configuration; no reason given"
	}
	return schema.Evidence{
		Extractor: "config",
		Path:      SourcePath,
		Rule:      "declared_override",
		Detail:    detail,
	}
}

// resolve turns a reference in configuration into a node ID.
//
// A full node ID is accepted, and so is anything the tools print: a name, or
// a name and namespace such as "checkout.prod". Making a person transcribe an
// identifier they were shown in a different form is how a correction file goes
// stale without anyone noticing.
func (a *applier) resolve(ref, field string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		a.diag(schema.SeverityWarn, "override_unresolved",
			fmt.Sprintf("a rule leaves %q empty", field))
		return "", false
	}

	matches := a.index.lookup(ref)
	switch len(matches) {
	case 1:
		return matches[0], true
	case 0:
		a.diag(schema.SeverityWarn, "override_unresolved",
			fmt.Sprintf("%q matches no component in this repository, so the rule naming it did nothing; "+
				"it may name something that has since been renamed or removed", ref))
		return "", false
	default:
		a.diag(schema.SeverityWarn, "override_ambiguous",
			fmt.Sprintf("%q matches %d components (%s); qualify it with a namespace, or use the full id",
				ref, len(matches), strings.Join(a.index.displayAll(matches), ", ")))
		return "", false
	}
}

func edgeKindList() string {
	kinds := schema.EdgeKinds()
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// index resolves the forms a person may write a component as.
type index struct {
	byID   map[string]schema.Node
	byForm map[string][]string
}

func newIndex(nodes []schema.Node) *index {
	idx := &index{
		byID:   make(map[string]schema.Node, len(nodes)),
		byForm: map[string][]string{},
	}
	for _, n := range nodes {
		idx.byID[n.ID] = n
		add := func(form string) {
			form = strings.ToLower(strings.TrimSpace(form))
			if form == "" {
				return
			}
			for _, existing := range idx.byForm[form] {
				if existing == n.ID {
					return
				}
			}
			idx.byForm[form] = append(idx.byForm[form], n.ID)
		}
		add(n.ID)
		add(n.Name)
		if n.Namespace != "" {
			add(n.Name + "." + n.Namespace)
		}
	}
	for form := range idx.byForm {
		sort.Strings(idx.byForm[form])
	}
	return idx
}

func (i *index) lookup(ref string) []string {
	return i.byForm[strings.ToLower(strings.TrimSpace(ref))]
}

// display is how a component is named back to the person, and has to be a
// form they can paste into the file that produced the diagnostic.
func (i *index) display(id string) string {
	n, ok := i.byID[id]
	if !ok {
		return id
	}
	// An external system's namespace is "net", and "api.stripe.com.net" is
	// not a form anyone would write or recognize.
	if n.Namespace == "" || n.Kind == schema.KindExternal {
		return n.Name
	}
	return n.Name + "." + n.Namespace
}

func (i *index) displayAll(ids []string) []string {
	out := make([]string, len(ids))
	for j, id := range ids {
		out[j] = i.display(id)
	}
	return out
}
