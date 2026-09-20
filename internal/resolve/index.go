package resolve

import (
	"sort"
	"strings"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Identity is everything a node can be referred to by.
//
// The resolver's whole job is answering "does this string name that
// component?", and the answer depends on where the string was written. A
// service is "checkout" in a manifest beside it, "checkout.prod" from another
// namespace, "checkout.prod.svc.cluster.local" in generated configuration,
// "aws_ecs_service.checkout" in Terraform, and possibly "acme/checkout" as an
// image. All of those have to lead back to one node.
type Identity struct {
	NodeID    string
	Kind      schema.NodeKind
	Namespace string

	// Names are the forms this node is called, unnormalized.
	Names []string
	// DNS are network addresses that route here, contributed by aliases.
	DNS []string
	// Images are container image references, full and basename.
	Images []string
	// Ports are the ports this node listens on.
	Ports []int
	// Labels are Kubernetes pod labels, which Service selectors match.
	Labels map[string]string
	// Tech names the technology, for corroborating a library hint.
	Tech string
	// Directory is where this component's source lives, when known.
	Directory string
	// BuildContext is the directory an image is built from, when known.
	BuildContext string
}

// Index maps every known identity form to the nodes that claim it.
//
// A form can be claimed by more than one node, and that is the interesting
// case rather than an edge case: a repository with dev, staging, and prod
// overlays declares the same service names three times. Resolving to whichever
// one happened to be indexed first would produce an edge that is wrong two
// times in three, so the index keeps every claimant and lets the matcher
// decide, or decline.
type Index struct {
	identities map[string]*Identity

	byDNS        map[string][]string
	byName       map[string][]string
	byNormalized map[string][]string
	byImage      map[string][]string
	byTech       map[string][]string
	byDirectory  map[string][]string
}

// NewIndex builds an identity index over a node set.
func NewIndex(nodes []schema.Node) *Index {
	idx := &Index{
		identities:   make(map[string]*Identity, len(nodes)),
		byDNS:        map[string][]string{},
		byName:       map[string][]string{},
		byNormalized: map[string][]string{},
		byImage:      map[string][]string{},
		byTech:       map[string][]string{},
		byDirectory:  map[string][]string{},
	}
	for _, n := range nodes {
		idx.add(identityOf(n))
	}
	return idx
}

func identityOf(n schema.Node) *Identity {
	id := &Identity{
		NodeID:    n.ID,
		Kind:      n.Kind,
		Namespace: n.Namespace,
		Names:     []string{n.Name},
		Labels:    map[string]string{},
	}

	// The last segment of the node ID is a normalized form of the name, and
	// is sometimes the only clean identifier a node has.
	if parsed, err := schema.ParseNodeID(n.ID); err == nil {
		id.Names = append(id.Names, lastSegment(parsed.Name))
		if id.Namespace == "" {
			id.Namespace = parsed.Namespace
		}
	}

	for _, key := range []string{"name", "address", "terraformModule"} {
		if v, ok := n.Attrs[key].(string); ok && v != "" {
			id.Names = append(id.Names, v)
		}
	}
	if img, ok := n.Attrs["image"].(string); ok && img != "" {
		id.addImage(img)
	}
	if images, ok := n.Attrs["images"].([]string); ok {
		for _, img := range images {
			id.addImage(img)
		}
	}
	if ports, ok := n.Attrs["ports"].([]int); ok {
		id.Ports = append(id.Ports, ports...)
	}
	if labels, ok := n.Attrs["labels"].(map[string]string); ok {
		for k, v := range labels {
			id.Labels[k] = v
		}
	}
	if dir, ok := n.Attrs["directory"].(string); ok {
		id.Directory = dir
	}
	if ctx, ok := n.Attrs["buildContext"].(string); ok {
		id.BuildContext = ctx
	}
	if n.Tech != nil {
		id.Tech = n.Tech.Framework
	}

	id.Names = dedupe(id.Names)
	sort.Ints(id.Ports)
	return id
}

func (id *Identity) addImage(image string) {
	ref := classify.ParseImage(image)
	id.Images = append(id.Images, strings.ToLower(image))
	if ref.Repository != "" {
		id.Images = append(id.Images, strings.ToLower(ref.Repository))
	}
	if ref.Name != "" {
		id.Images = append(id.Images, strings.ToLower(ref.Name))
		// An image basename is also a plausible name for the component: a
		// Deployment running ghcr.io/acme/checkout is very likely the thing
		// other files call "checkout".
		id.Names = append(id.Names, ref.Name)
	}
	id.Images = dedupe(id.Images)
}

func (idx *Index) add(id *Identity) {
	idx.identities[id.NodeID] = id
	idx.reindex(id)
}

// reindex rebuilds the lookup entries for one identity. It is called again
// after aliases attach DNS names, which is the point at which most network
// addresses become known.
func (idx *Index) reindex(id *Identity) {
	for _, name := range id.Names {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" {
			continue
		}
		idx.byName[lower] = appendUnique(idx.byName[lower], id.NodeID)
		idx.byNormalized[NormalizeName(lower)] = appendUnique(idx.byNormalized[NormalizeName(lower)], id.NodeID)
	}
	for _, dns := range id.DNS {
		lower := strings.ToLower(dns)
		idx.byDNS[lower] = appendUnique(idx.byDNS[lower], id.NodeID)
		// A DNS name is also a name, so that a bare reference to a Service
		// resolves even when the workload is called something else.
		idx.byName[lower] = appendUnique(idx.byName[lower], id.NodeID)
		idx.byNormalized[NormalizeName(lower)] = appendUnique(idx.byNormalized[NormalizeName(lower)], id.NodeID)
	}
	for _, img := range id.Images {
		idx.byImage[img] = appendUnique(idx.byImage[img], id.NodeID)
	}
	if id.Tech != "" {
		idx.byTech[id.Tech] = appendUnique(idx.byTech[id.Tech], id.NodeID)
	}
	if id.Directory != "" {
		idx.byDirectory[id.Directory] = appendUnique(idx.byDirectory[id.Directory], id.NodeID)
	}
}

// Identity returns a node's identity record.
func (idx *Index) Identity(nodeID string) (*Identity, bool) {
	id, ok := idx.identities[nodeID]
	return id, ok
}

// AttachDNS records that a name routes to a node.
func (idx *Index) AttachDNS(nodeID string, names ...string) {
	id, ok := idx.identities[nodeID]
	if !ok {
		return
	}
	before := len(id.DNS)
	id.DNS = dedupe(append(id.DNS, names...))
	if len(id.DNS) != before {
		idx.reindex(id)
	}
}

// AttachPorts records ports an alias exposes on a node's behalf.
func (idx *Index) AttachPorts(nodeID string, ports ...int) {
	id, ok := idx.identities[nodeID]
	if !ok {
		return
	}
	id.Ports = dedupeInts(append(id.Ports, ports...))
}

// NodesBySelector returns every node whose labels satisfy a selector, sorted.
func (idx *Index) NodesBySelector(selector map[string]string) []string {
	if len(selector) == 0 {
		return nil
	}
	var out []string
	for nodeID, id := range idx.identities {
		if len(id.Labels) == 0 {
			continue
		}
		matched := true
		for k, want := range selector {
			if got, ok := id.Labels[k]; !ok || got != want {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, nodeID)
		}
	}
	sort.Strings(out)
	return out
}

// NodesInDirectory returns nodes whose source lives in a directory.
func (idx *Index) NodesInDirectory(dir string) []string {
	return idx.byDirectory[dir]
}

// AllIdentities returns every identity, in a fixed order.
func (idx *Index) AllIdentities() []*Identity {
	ids := make([]string, 0, len(idx.identities))
	for id := range idx.identities {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]*Identity, len(ids))
	for i, id := range ids {
		out[i] = idx.identities[id]
	}
	return out
}

// NormalizeName collapses the separators that distinguish otherwise identical
// names across ecosystems: Kubernetes prefers hyphens, Go and Python prefer
// underscores, and DNS uses dots. "user-service", "user_service", and
// "userservice" are one component written three ways.
//
// This is the loosest match the resolver makes, and it is scored lower than
// an exact one precisely because collapsing separators can bring genuinely
// different names together.
func NormalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case '-', '_', '.', ' ':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func dedupeInts(in []int) []int {
	sort.Ints(in)
	out := in[:0]
	for i, v := range in {
		if i == 0 || in[i-1] != v {
			out = append(out, v)
		}
	}
	return out
}
