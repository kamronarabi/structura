// Package project decides where one project in a repository ends and the next
// one begins.
//
// Structura matches names across files to recover relationships, and a name
// only means something inside a boundary. Without one, every matching rule
// eventually reaches across a boundary it cannot see: a Compose file resolving
// its database to an unrelated stack's Postgres, a service absorbing another
// project's package.json, two teams' "prod" namespaces becoming one node
// because the node ID has nowhere to record which repository-within-the-
// repository they came from.
//
// Each of those was fixed once, separately, by guessing at the boundary from
// a different proxy -- the namespace, the directory depth, the extractor that
// produced a node. This package exists so there is one answer instead of
// three, computed once, from the file tree rather than from the graph.
//
// # What counts as a project
//
// Projects are declared, not detected. That is a deliberate retreat from an
// earlier attempt at inferring them, which is worth recording so nobody
// repeats it.
//
// The obvious signal is a stack declaration: a directory holding a Compose
// file, a Helm chart, a Terraform root. It does not work. A repository that
// keeps its chart in charts/api and its Compose file in deploy/ is one
// project, and splitting on those markers cut two of this project's own
// fixtures in half -- turning a correct graph into two disconnected ones.
// Every other structural signal tried had the same shape of failure:
// directory depth, path divergence, connectivity of declared edges. They each
// separate some real layouts and destroy others, because "these directories
// are sibling projects" and "these directories are parts of one project" look
// identical from the file tree.
//
// Guessing wrong in the splitting direction is the worse error. It breaks
// joins that were correct, and it does so silently, in a graph nobody
// re-reads. So a repository is one project unless a person says otherwise,
// and Set.Len reporting 1 is the normal answer.
//
// What configuration buys, where it is used, is exactness: the boundary stops
// being re-derived from a different proxy in each matching rule and becomes
// one fact, recorded once.
package project

import (
	"path"
	"sort"
	"strings"
)

// Root is the repository root, and the project every path falls back to.
const Root = "."

// Set is the projects a repository contains, as repo-relative directories.
type Set struct {
	// roots is sorted deepest-first so that Of can return the first match.
	roots []string
	// configured records which roots were named in configuration.
	configured map[string]bool
}

// Discover returns the projects a repository contains.
//
// configured are the roots named in configuration. Anything not inside one of
// them belongs to the repository root, which is always a project, so the
// result is never empty and the no-configuration answer is a single project.
func Discover(configured []string) *Set {
	s := &Set{configured: map[string]bool{}}
	seen := map[string]bool{Root: true}

	for _, dir := range normalizeRoots(configured) {
		if !seen[dir] {
			seen[dir] = true
			s.roots = append(s.roots, dir)
		}
		s.configured[dir] = true
	}

	s.roots = append(s.roots, Root)
	// Deepest first, so Of returns the most specific project containing a
	// path. Ties break on name to keep the order fixed between runs.
	sort.Slice(s.roots, func(i, j int) bool {
		di, dj := depth(s.roots[i]), depth(s.roots[j])
		if di != dj {
			return di > dj
		}
		return s.roots[i] < s.roots[j]
	})
	return s
}

// normalizeRoots cleans configured roots and drops anything that would escape
// the repository.
func normalizeRoots(in []string) []string {
	var out []string
	for _, raw := range in {
		dir := strings.TrimSpace(raw)
		if dir == "" {
			continue
		}
		dir = path.Clean(strings.TrimPrefix(strings.ReplaceAll(dir, "\\", "/"), "./"))
		if dir == "." || dir == "/" || dir == ".." || strings.HasPrefix(dir, "../") {
			continue
		}
		out = append(out, strings.TrimPrefix(dir, "/"))
	}
	sort.Strings(out)
	return out
}

// Of returns the project that owns a repo-relative path: the deepest root
// that contains it, and the repository root when nothing deeper does.
func (s *Set) Of(p string) string {
	if s == nil || p == "" {
		return Root
	}
	p = path.Clean(strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./"))
	for _, root := range s.roots {
		if root == Root {
			continue
		}
		if p == root || strings.HasPrefix(p, root+"/") {
			return root
		}
	}
	return Root
}

// Roots lists every project, deepest first.
func (s *Set) Roots() []string {
	if s == nil {
		return []string{Root}
	}
	return append([]string(nil), s.roots...)
}

// Len reports how many projects the repository contains. One means the
// repository is a single system, which is the common case and the default.
func (s *Set) Len() int {
	if s == nil {
		return 1
	}
	return len(s.roots)
}

// Configured reports whether a root was named in configuration.
func (s *Set) Configured(root string) bool {
	if s == nil {
		return false
	}
	return s.configured[root]
}

// Label returns a short, stable identifier for a project, suitable for
// qualifying a name that would otherwise collide between projects.
//
// The repository root has no label: a single-project repository must produce
// exactly the identifiers it produced before this package existed.
func Label(root string) string {
	if root == Root || root == "" {
		return ""
	}
	return strings.ReplaceAll(root, "/", "-")
}

func depth(p string) int {
	if p == Root {
		return 0
	}
	return strings.Count(p, "/") + 1
}
