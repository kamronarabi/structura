package project

import (
	"path"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Detection cannot split a repository safely, but it can ask.
//
// Projects are declared because every structural signal tried as grounds for
// splitting also fired on repositories that were genuinely one system, and a
// wrong split breaks joins that were correct. That reasoning holds for acting
// on a signal. It does not hold for mentioning one: a warning that is wrong
// costs a line of text, and a warning that is right saves a reader from a
// graph in which two unrelated stacks have been merged into one.
//
// Without this, the boundary machinery only helps people who already knew
// they needed it, which is nobody.
//
// # What is suspicious
//
// One component declared by files in two distant parts of the tree. That is
// the shape the collision actually took: a "prod" namespace declared under
// both k8s-microservices/deploy/prod and multi-environment/overlays/prod,
// producing a single boundary node containing members from two systems.
//
// Two things keep that from firing on ordinary repositories.
//
// Divergence has to be below the repository root. A single project routinely
// declares one component from several top-level directories -- base/ beside
// overlays/, deploy/ beside services/ -- and that is the layout the graph is
// built for, not a sign of two projects.
//
// The two trees have to describe otherwise different things. This is what
// separates two projects from one project's environment overlays. An overlay
// re-declares the same Deployment the base declares, so the two trees share
// real components. Two independent stacks share only the accident of a
// reused name, and their services and datastores are disjoint.

// Suspect is one pair of directory trees that look like separate projects.
type Suspect struct {
	// Trees are the two directories that both declare something, sorted.
	Trees []string
	// Shared names the components both trees declare, sorted. These are the
	// nodes that were merged, and are what a reader should look at.
	Shared []string
}

// Suspects reports pairs of directory trees that look like separate projects.
//
// nodes must be the merged node set from before resolution, not after. The
// two merges mean opposite things. A builder merge happens because two files
// produced the same identifier, which is the accident this looks for. A
// resolver merge happens because a build context named a directory outright,
// which is a declaration that the two files describe one component -- and
// reading that as evidence of two projects had exactly the wrong sign: it
// flagged a Compose file and the code it builds as separate systems.
//
// set is what the repository has already declared. Two trees inside one
// declared project are not a question anyone needs asked again.
//
// It is advisory in the strictest sense: nothing in the scan behaves
// differently because of what this returns.
func Suspects(nodes []schema.Node, set *Set) []Suspect {
	declaredIn := map[string][]string{} // node ID -> declaring directories
	nodesUnder := map[string]map[string]bool{}

	for _, n := range nodes {
		for _, src := range n.Sources {
			if src.Path == "" {
				continue
			}
			dir := path.Dir(src.Path)
			if dir == "." || dir == "/" {
				dir = Root
			}
			declaredIn[n.ID] = appendUnique(declaredIn[n.ID], dir)
		}
	}

	// A node's kind is needed to tell "two trees describe the same service"
	// from "two trees happen to use one namespace name".
	kind := map[string]schema.NodeKind{}
	name := map[string]string{}
	for _, n := range nodes {
		kind[n.ID] = n.Kind
		name[n.ID] = n.Name
	}

	// pairs maps a tree pair onto the components both trees declare.
	pairs := map[[2]string][]string{}

	for nodeID, dirs := range declaredIn {
		if len(dirs) < 2 {
			continue
		}
		ancestor := commonAncestor(dirs)
		// Divergence at the repository root is how one project is laid out.
		if ancestor == Root {
			continue
		}
		trees := treesUnder(ancestor, dirs)
		if len(trees) < 2 {
			continue
		}
		for i := 0; i < len(trees); i++ {
			for j := i + 1; j < len(trees); j++ {
				key := [2]string{trees[i], trees[j]}
				pairs[key] = appendUnique(pairs[key], nodeID)
			}
		}
	}
	if len(pairs) == 0 {
		return nil
	}

	for _, n := range nodes {
		for _, src := range n.Sources {
			if src.Path == "" {
				continue
			}
			for key := range pairs {
				for _, tree := range key {
					if under(tree, src.Path) {
						if nodesUnder[tree] == nil {
							nodesUnder[tree] = map[string]bool{}
						}
						nodesUnder[tree][n.ID] = true
					}
				}
			}
		}
	}

	keys := make([][2]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})

	var out []Suspect
	for _, key := range keys {
		// Already answered: the repository says these are one project, or
		// says they are two, and either way it is not a question.
		if set.Of(key[0]) == set.Of(key[1]) && set.Of(key[0]) != Root {
			continue
		}
		// An overlay re-declares the base's real components, so the two trees
		// share a service or a datastore. Independent stacks share only the
		// reused name.
		if sharesRealComponent(nodesUnder[key[0]], nodesUnder[key[1]], kind) {
			continue
		}

		shared := make([]string, 0, len(pairs[key]))
		for _, id := range pairs[key] {
			if n := name[id]; n != "" {
				shared = appendUnique(shared, n)
			}
		}
		sort.Strings(shared)
		out = append(out, Suspect{Trees: []string{key[0], key[1]}, Shared: shared})
	}
	return out
}

// sharesRealComponent reports whether two trees both declare a component that
// is more than a grouping -- a service, a datastore, a queue.
//
// The kind is the whole test, including for the components that collided. An
// environment overlay re-declares the base's actual Deployment, and that is
// the thing that says the two trees describe one system; excluding the
// collided nodes from consideration would throw that evidence away and report
// every base/overlay pair as two projects.
//
// A boundary is a grouping, and an external is shared by anyone who calls it.
// Two projects reusing the name "prod", or both calling Stripe, have said
// nothing about being one project.
func sharesRealComponent(a, b map[string]bool, kind map[string]schema.NodeKind) bool {
	for id := range a {
		if !b[id] {
			continue
		}
		switch kind[id] {
		case schema.KindBoundary, schema.KindExternal:
			continue
		default:
			return true
		}
	}
	return false
}

// commonAncestor returns the deepest directory containing every path.
func commonAncestor(dirs []string) string {
	if len(dirs) == 0 {
		return Root
	}
	parts := strings.Split(dirs[0], "/")
	for _, dir := range dirs[1:] {
		if dir == Root {
			return Root
		}
		other := strings.Split(dir, "/")
		n := 0
		for n < len(parts) && n < len(other) && parts[n] == other[n] {
			n++
		}
		parts = parts[:n]
		if len(parts) == 0 {
			return Root
		}
	}
	if len(parts) == 0 {
		return Root
	}
	return strings.Join(parts, "/")
}

// treesUnder returns the distinct immediate children of ancestor that the
// directories fall under.
func treesUnder(ancestor string, dirs []string) []string {
	var out []string
	prefix := ancestor + "/"
	for _, dir := range dirs {
		if !strings.HasPrefix(dir, prefix) {
			continue
		}
		rest := strings.TrimPrefix(dir, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			out = appendUnique(out, ancestor+"/"+rest)
		}
	}
	sort.Strings(out)
	return out
}

func under(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}
