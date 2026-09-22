package project

import (
	"path"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/internal/classify"
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
// One component declared by files in two distant parts of the tree. There are
// two separate reasons to doubt that they are one component, and they catch
// different repositories.
//
// **The trees share nothing else.** This is the shape the original collision
// took: a "prod" namespace declared under both k8s-microservices/deploy/prod
// and multi-environment/overlays/prod, producing a single boundary node with
// members from two systems. A namespace carries no evidence of its own, so
// the only thing to go on is company: an environment overlay re-declares the
// base's actual Deployment, and two independent stacks share only the
// accident of a reused name.
//
// That test is weak enough to need a second restriction. Divergence has to be
// below the repository root, because a single project routinely declares one
// component from several top-level directories -- base/ beside overlays/,
// deploy/ beside services/ -- and those trees share nothing else either.
//
// **The trees describe it differently.** A component that one tree builds
// from angular/angular and another builds from apache-php/app is two
// components with one name, whatever the directory layout says. This needs no
// restriction on where the trees sit, because it is evidence about the
// component rather than an inference from the tree shape, and it is the only
// thing that fires on a repository of sample stacks laid out at the root.

// Ground is why a set of trees is suspected of being separate projects. The
// two grounds warrant different sentences, so the caller is told which fired
// rather than being left to write one message that covers both badly.
type Ground string

const (
	// GroundUnrelated means the trees share nothing but the name that
	// collided.
	GroundUnrelated Ground = "unrelated"
	// GroundDescribedDifferently means the trees disagree about what the
	// collided component is -- a different image, or a different build
	// directory.
	GroundDescribedDifferently Ground = "described-differently"
)

// Suspect is one set of directory trees that look like separate projects.
type Suspect struct {
	// Trees are the directories that all declare something, sorted.
	Trees []string
	// Shared names the components every tree declares, sorted. These are the
	// nodes that were merged, and are what a reader should look at.
	Shared []string
	// Ground is why they are suspected.
	Ground Ground
}

// Suspects reports sets of directory trees that look like separate projects.
//
// readings must be every extractor's output from before merge, not the merged
// node set: two readings of one identifier are what this looks for, and once
// they are merged the disagreement between them is gone. It must also be from
// before resolution, because the two merges mean opposite things. A builder
// merge happens because two files produced the same identifier, which is the
// accident this looks for. A resolver merge happens because a build context
// named a directory outright, which is a declaration that the two files
// describe one component -- and reading that as evidence of two projects had
// exactly the wrong sign: it flagged a Compose file and the code it builds as
// separate systems.
//
// set is what the repository has already declared. Trees inside one declared
// project are not a question anyone needs asked again.
//
// It is advisory in the strictest sense: nothing in the scan behaves
// differently because of what this returns.
func Suspects(readings []schema.Node, set *Set) []Suspect {
	byID := map[string][]schema.Node{}
	for _, n := range readings {
		byID[n.ID] = append(byID[n.ID], n)
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// First pass: which trees declare each collided component. Collected
	// before anything is judged, because the judgement for one component
	// needs to know what else its trees contain.
	type collision struct {
		id       string
		name     string
		trees    []string
		atRoot   bool
		readings []schema.Node
	}
	var collisions []collision
	allTrees := map[string]bool{}

	for _, id := range ids {
		group := byID[id]
		var dirs []string
		for _, n := range group {
			for _, src := range n.Sources {
				if src.Path == "" {
					continue
				}
				dir := path.Dir(src.Path)
				if dir == "." || dir == "/" {
					dir = Root
				}
				dirs = append(dirs, dir)
			}
		}
		dirs = unique(dirs)
		if len(dirs) < 2 {
			continue
		}
		ancestor := commonAncestor(dirs)
		trees := treesUnder(ancestor, dirs)
		if len(trees) < 2 {
			continue
		}
		for _, t := range trees {
			allTrees[t] = true
		}
		collisions = append(collisions, collision{
			id: id, name: nameOf(group), trees: trees,
			atRoot: ancestor == Root, readings: group,
		})
	}
	if len(collisions) == 0 {
		return nil
	}

	kind := map[string]schema.NodeKind{}
	for _, n := range readings {
		if _, ok := kind[n.ID]; !ok {
			kind[n.ID] = n.Kind
		}
	}
	nodesUnder := declarationsByTree(readings, allTrees)

	// Second pass: judge each collision, and group by the set of trees so a
	// repository of fifty sample stacks produces one question per collided
	// name rather than one per pair of directories.
	type group struct {
		trees  []string
		ground Ground
		shared []string
	}
	groups := map[string]*group{}
	var order []string

	for _, c := range collisions {
		// Already answered: the repository says these trees are one project,
		// and either way it is not a question.
		if declaredTogether(c.trees, set) {
			continue
		}

		var ground Ground
		switch {
		case describedDifferently(c.readings, c.trees):
			ground = GroundDescribedDifferently
		case !c.atRoot && shareNoRealComponent(c.trees, nodesUnder, kind):
			ground = GroundUnrelated
		default:
			continue
		}

		key := strings.Join(c.trees, "\x00") + "\x00" + string(ground)
		g, ok := groups[key]
		if !ok {
			g = &group{trees: c.trees, ground: ground}
			groups[key] = g
			order = append(order, key)
		}
		if c.name != "" {
			g.shared = unique(append(g.shared, c.name))
		}
	}

	sort.Strings(order)
	out := make([]Suspect, 0, len(order))
	for _, key := range order {
		g := groups[key]
		shared := append([]string(nil), g.shared...)
		sort.Strings(shared)
		out = append(out, Suspect{Trees: g.trees, Shared: shared, Ground: g.ground})
	}
	return out
}

// substance is what one reading says a component actually is, reduced to the
// parts two readings of the same component have to agree on.
//
// The image is compared by name alone. A Helm chart writes a bare repository
// and a Kubernetes manifest writes a registry path and a tag for the same
// build, so anything finer reports every chart-beside-manifest repository as
// two projects. The name is what survives both spellings.
type substance struct{ image, build string }

func substanceOf(n schema.Node) substance {
	var s substance
	if image, _ := n.Attrs["image"].(string); image != "" {
		s.image = strings.ToLower(classify.ParseImage(image).Name)
	}
	// buildContext is what Compose declares; directory is what an extractor
	// that found a component by its files records. Either says where the
	// component's source lives.
	if ctx, _ := n.Attrs["buildContext"].(string); ctx != "" {
		s.build = ctx
	} else if dir, _ := n.Attrs["directory"].(string); dir != "" {
		s.build = dir
	}
	return s
}

// describedDifferently reports whether two trees disagree about what the
// component is.
//
// Readings within one tree are folded together first: a Compose file naming
// an image and the Dockerfile beside it are one tree's account, and comparing
// them to each other would report every repository against itself.
func describedDifferently(readings []schema.Node, trees []string) bool {
	byTree := map[string]substance{}
	for _, n := range readings {
		tree := treeContaining(n.Sources, trees)
		if tree == "" {
			continue
		}
		s := substanceOf(n)
		known := byTree[tree]
		if known.image == "" {
			known.image = s.image
		}
		if known.build == "" {
			known.build = s.build
		}
		byTree[tree] = known
	}

	var seen []substance
	for _, tree := range trees {
		s, ok := byTree[tree]
		if !ok {
			continue
		}
		for _, other := range seen {
			if conflicts(s.image, other.image) || conflicts(s.build, other.build) {
				return true
			}
		}
		seen = append(seen, s)
	}
	return false
}

// conflicts reports whether two accounts of the same thing disagree. Silence
// is not disagreement: a file that named no image has not contradicted one
// that did.
func conflicts(a, b string) bool { return a != "" && b != "" && a != b }

func treeContaining(sources []schema.Source, trees []string) string {
	for _, src := range sources {
		if src.Path == "" {
			continue
		}
		for _, tree := range trees {
			if under(tree, src.Path) {
				return tree
			}
		}
	}
	return ""
}

// shareNoRealComponent reports whether no two of the trees declare a common
// component that is more than a grouping.
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
func shareNoRealComponent(trees []string, nodesUnder map[string]map[string]bool, kind map[string]schema.NodeKind) bool {
	for i := 0; i < len(trees); i++ {
		for j := i + 1; j < len(trees); j++ {
			for id := range nodesUnder[trees[i]] {
				if !nodesUnder[trees[j]][id] {
					continue
				}
				switch kind[id] {
				case schema.KindBoundary, schema.KindExternal:
					continue
				default:
					return false
				}
			}
		}
	}
	return true
}

// declaredTogether reports whether the repository has already placed every
// tree in one project of its own.
func declaredTogether(trees []string, set *Set) bool {
	first := set.Of(trees[0])
	if first == Root {
		return false
	}
	for _, tree := range trees[1:] {
		if set.Of(tree) != first {
			return false
		}
	}
	return true
}

func declarationsByTree(readings []schema.Node, trees map[string]bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, n := range readings {
		for _, src := range n.Sources {
			if src.Path == "" {
				continue
			}
			for tree := range trees {
				if !under(tree, src.Path) {
					continue
				}
				if out[tree] == nil {
					out[tree] = map[string]bool{}
				}
				out[tree][n.ID] = true
			}
		}
	}
	return out
}

// nameOf returns the name the readings agree on, preferring one that was
// written down over one that was derived from a directory.
func nameOf(readings []schema.Node) string {
	for _, n := range readings {
		if n.Name != "" && n.Attrs["nameFrom"] == nil {
			return n.Name
		}
	}
	for _, n := range readings {
		if n.Name != "" {
			return n.Name
		}
	}
	return ""
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
// directories fall under. A directory that is the ancestor itself contributes
// no tree: a file at the repository root belongs to no top-level directory.
func treesUnder(ancestor string, dirs []string) []string {
	prefix := ancestor + "/"
	if ancestor == Root {
		prefix = ""
	}
	var out []string
	for _, dir := range dirs {
		if !strings.HasPrefix(dir, prefix) {
			continue
		}
		rest := strings.TrimPrefix(dir, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		if rest == "" || rest == Root {
			continue
		}
		out = unique(append(out, path.Join(ancestor, rest)))
	}
	sort.Strings(out)
	return out
}

func under(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func unique(list []string) []string {
	out := list[:0]
	seen := map[string]bool{}
	for _, v := range list {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
