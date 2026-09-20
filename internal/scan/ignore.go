package scan

import (
	"bufio"
	"io"
	"path"
	"strings"
)

// Ignore rules follow gitignore(5) semantics, implemented here rather than
// pulled in as a dependency. The available libraries either bring a large
// module graph for one file of logic, or compile a flat pattern list with no
// notion of per-directory stacking — and stacking is not optional, because a
// .gitignore in a subdirectory is anchored to that subdirectory.
//
// The behavior is verified against real git in ignore_test.go: a generated
// tree is walked both by this matcher and by `git ls-files`, and the two
// file sets must agree. That is a genuine oracle, which is what makes
// reimplementing the format defensible.

// pattern is one compiled ignore line.
type pattern struct {
	// segments is the pattern split on "/", where "**" matches any number of
	// path segments and every other segment is matched with path.Match.
	segments []string
	negate   bool
	dirOnly  bool
	// raw is kept for diagnostics.
	raw string
}

// patternList is the compiled contents of one ignore file, anchored at the
// directory that file lives in.
type patternList struct {
	// base is the repo-relative directory the patterns are anchored to, ""
	// for the repository root.
	base     string
	patterns []pattern
	source   string
}

// compilePatterns parses one ignore file's contents.
func compilePatterns(r io.Reader, base, source string) (*patternList, error) {
	pl := &patternList{base: base, source: source}
	sc := bufio.NewScanner(r)
	// Ignore files are small; a generous line cap avoids a scanner error on
	// a pathological one-line file.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		if p, ok := compilePattern(sc.Text()); ok {
			pl.patterns = append(pl.patterns, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return pl, nil
}

func compilePattern(line string) (pattern, bool) {
	// Trailing whitespace is not significant unless escaped.
	line = trimUnescapedTrailingSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}

	p := pattern{raw: line}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	// A leading backslash escapes a literal '#' or '!'.
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}
	if line == "" {
		return pattern{}, false
	}

	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			return pattern{}, false
		}
	}

	// A pattern is anchored to the ignore file's directory if it contains a
	// slash anywhere other than at the end. Otherwise it matches at any
	// depth, which is expressed by prepending a "**" segment.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")

	p.segments = strings.Split(line, "/")
	if !anchored {
		p.segments = append([]string{"**"}, p.segments...)
	}
	return p, true
}

// trimUnescapedTrailingSpace removes trailing spaces that are not
// backslash-escaped, per gitignore(5).
func trimUnescapedTrailingSpace(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		// Count the backslashes immediately before this run of whitespace;
		// an odd number means the space is escaped and significant.
		backslashes := 0
		for i := end - 2; i >= 0 && s[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		end--
	}
	return s[:end]
}

// match reports whether p matches the given repo-relative path.
func (p pattern) match(relPath string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	return matchSegments(p.segments, strings.Split(relPath, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// "**" matches zero or more segments; try each split point.
			// Patterns are short, so the recursion stays shallow.
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		// path.Match never lets * or ? cross a separator, and the segments
		// here contain none, so this is exactly gitignore's per-segment glob.
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// matches reports the verdict of this file's patterns for a path, and whether
// any pattern matched at all. Within one file the last matching pattern wins,
// which is what makes "!" re-inclusion work.
func (pl *patternList) matches(relPath string, isDir bool) (ignored, matched bool) {
	rel, ok := relativeTo(pl.base, relPath)
	if !ok {
		return false, false
	}
	for i := len(pl.patterns) - 1; i >= 0; i-- {
		if pl.patterns[i].match(rel, isDir) {
			return !pl.patterns[i].negate, true
		}
	}
	return false, false
}

// relativeTo re-expresses a repo-relative path relative to base, reporting
// false when the path lies outside base.
func relativeTo(base, relPath string) (string, bool) {
	if base == "" {
		return relPath, true
	}
	if !strings.HasPrefix(relPath, base+"/") {
		return "", false
	}
	return relPath[len(base)+1:], true
}

// ignoreStack is the set of ignore rules in effect, ordered from lowest to
// highest precedence: built-in defaults, then .gitignore files, then
// .structuraignore files, each ordered from the repository root downward.
//
// A deeper file wins over a shallower one, and a later layer wins over an
// earlier one, so a .structuraignore can re-include something .gitignore
// excluded — which is the point, since build output a developer does not want
// in git may still describe the architecture.
type ignoreStack struct {
	lists []*patternList
}

func (s *ignoreStack) push(pl *patternList) *ignoreStack {
	if pl == nil || len(pl.patterns) == 0 {
		return s
	}
	next := &ignoreStack{lists: make([]*patternList, len(s.lists), len(s.lists)+1)}
	copy(next.lists, s.lists)
	next.lists = append(next.lists, pl)
	return next
}

// ignored reports whether a path is excluded, and which ignore file decided.
func (s *ignoreStack) ignored(relPath string, isDir bool) (ignored bool, source string) {
	for i := len(s.lists) - 1; i >= 0; i-- {
		if verdict, matched := s.lists[i].matches(relPath, isDir); matched {
			return verdict, s.lists[i].source
		}
	}
	return false, ""
}

// DefaultIgnores are directories that never describe architecture but do
// contain enormous numbers of files: dependency trees, build output, virtual
// environments, VCS metadata.
//
// They are the lowest-precedence layer, so a repository can re-include any of
// them with a negation in .structuraignore.
var DefaultIgnores = []string{
	".git/",
	".hg/",
	".svn/",
	"node_modules/",
	"vendor/",
	"bower_components/",
	".venv/",
	"venv/",
	"__pycache__/",
	".tox/",
	".mypy_cache/",
	".pytest_cache/",
	".ruff_cache/",
	"target/",
	".gradle/",
	".terraform/",
	".next/",
	".nuxt/",
	".svelte-kit/",
	".cache/",
	"*.min.js",
	"*.min.css",
	".DS_Store",
	// Structura's own output, so that scanning twice does not feed the
	// previous graph back into the next scan.
	".structura/",
}

func defaultPatternList() *patternList {
	pl := &patternList{source: "<built-in defaults>"}
	for _, line := range DefaultIgnores {
		if p, ok := compilePattern(line); ok {
			pl.patterns = append(pl.patterns, p)
		}
	}
	return pl
}
