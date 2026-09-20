// Package manifest extracts components and their technology from language
// dependency manifests: go.mod, package.json, requirements.txt, and
// pyproject.toml.
//
// The value here is not the dependency list. It is two things a manifest
// states that no infrastructure file does: that a directory contains a
// service at all, and what that service is written in. A Compose file says
// "the image for ./checkout is built here"; go.mod says "./checkout is a Go
// module called acme/checkout". Putting those together is what lets the
// graph say a service is Go rather than leaving it an anonymous container.
//
// Dependencies contribute only where they imply infrastructure — a Postgres
// driver, a Kafka client, an AWS SDK. See classify.LibraryImplies for why the
// rest are deliberately dropped.
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/modfile"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "manifest"

// Extractor reads language dependency manifests.
type Extractor struct{}

// New returns a manifest extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

var matchedNames = map[string]bool{
	"go.mod":           true,
	"package.json":     true,
	"requirements.txt": true,
	"pyproject.toml":   true,
}

// Match implements scan.Extractor.
func (*Extractor) Match(f scan.FileMeta) bool {
	name := strings.ToLower(f.Name)
	if matchedNames[name] {
		return true
	}
	// requirements-dev.txt, requirements/base.txt, and similar.
	return strings.HasPrefix(name, "requirements") && f.Ext == ".txt"
}

// component is what one manifest declares.
type component struct {
	name         string
	language     string
	framework    string
	dependencies []string
	attrs        schema.Attrs
	line         int
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	var (
		comp *component
		err  error
	)
	switch strings.ToLower(f.Name) {
	case "go.mod":
		comp, err = parseGoMod(f)
	case "package.json":
		comp, err = parsePackageJSON(f)
	case "pyproject.toml":
		comp, err = parsePyproject(f)
	default:
		comp = parseRequirements(f)
	}
	if err != nil {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityWarn,
			Code:     "manifest_parse_failed",
			Path:     f.Path,
			Message:  fmt.Sprintf("could not read this %s: %v", f.Name, err),
		})
		return nil
	}
	if comp == nil || comp.name == "" {
		return nil
	}

	e.emit(f, emit, comp)
	return nil
}

func (e *Extractor) emit(f *scan.File, emit scan.Emitter, comp *component) {
	// The directory is the identity that matters: it is what a Compose build
	// context or a Dockerfile path points at, which is how the resolver
	// connects this to the container that actually runs it.
	dir := f.Dir
	if dir == "" {
		dir = "."
	}
	// A manifest at the repository root has no directory to name it by, and
	// "." normalizes away to nothing, so the node would end up with a
	// meaningless identifier.
	idPath := dir
	if idPath == "." {
		idPath = "root"
	}
	id := schema.NewNodeID(schema.KindService, Name, comp.language, idPath)

	// Dependencies arrive from JSON and TOML maps, whose iteration order is
	// random. Sorting here is not cosmetic: without it, a package.json
	// listing both next and react would pick a different framework between
	// runs, and two scans of an unchanged tree would disagree.
	sort.Strings(comp.dependencies)

	implied := impliedInfrastructure(comp.dependencies)
	framework := comp.framework
	for _, dep := range comp.dependencies {
		if fw, ok := classify.FrameworkOf(dep); ok {
			framework = fw
			break
		}
	}

	attrs := schema.Attrs{"manifest": f.Name, "directory": dir}
	for k, v := range comp.attrs {
		attrs[k] = v
	}
	attrs["dependencyCount"] = len(comp.dependencies)

	src := schema.Source{Extractor: Name, Path: f.Path, Line: comp.line}
	emit.Node(schema.Node{
		ID:        id,
		Kind:      schema.KindService,
		Layer:     schema.LayerContainer,
		Name:      comp.name,
		Namespace: comp.language,
		Tech:      &schema.Tech{Language: comp.language, Framework: framework},
		Attrs:     attrs,
		Sources:   []schema.Source{src},
		// A manifest proves a codebase exists here, not that it is deployed
		// as its own service. An infrastructure file saying so is what
		// raises this, via the merge.
		Confidence: schema.ConfHostMatch,
	})

	// The names this code is likely to be known by elsewhere: its directory
	// (how a build context refers to it) and its declared name.
	aliasNames := []string{path.Base(dir)}
	if base := path.Base(comp.name); base != aliasNames[0] {
		aliasNames = append(aliasNames, base)
	}
	emit.Alias(resolve.Alias{
		Name:       path.Base(dir),
		Namespace:  comp.language,
		DNS:        aliasNames,
		TargetName: comp.name,
		Source: schema.Evidence{
			Extractor: Name, Path: f.Path, Line: comp.line,
			Rule:   "manifest_module_name",
			Detail: fmt.Sprintf("%s declares module %q in %s", f.Name, comp.name, dir),
		},
	})

	for _, imp := range implied {
		emit.Hint(resolve.Hint{
			FromNode: id,
			Kind:     resolve.HintLibrary,
			Raw:      imp.dependency,
			// The technology name, not a hostname: this says what kind of
			// thing is on the other end, never which instance.
			Tokens:        []string{imp.lib.Tech},
			Protocol:      imp.lib.Tech,
			SuggestedEdge: imp.lib.Edge,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: comp.line,
				Rule: "manifest_dependency_implies",
				Detail: fmt.Sprintf("%s depends on %q, which indicates %s",
					f.Name, imp.dependency, imp.lib.Tech),
			},
		})
	}
}

type impliedDep struct {
	dependency string
	lib        classify.Library
}

// impliedInfrastructure keeps one dependency per technology — the first in
// sorted order — so that a project using three Redis clients does not produce
// three identical hints.
func impliedInfrastructure(deps []string) []impliedDep {
	sorted := append([]string(nil), deps...)
	sort.Strings(sorted)

	seen := map[string]bool{}
	var out []impliedDep
	for _, dep := range sorted {
		lib, ok := classify.LibraryImplies(dep)
		if !ok || seen[lib.Tech] {
			continue
		}
		seen[lib.Tech] = true
		out = append(out, impliedDep{dependency: dep, lib: lib})
	}
	return out
}

func parseGoMod(f *scan.File) (*component, error) {
	mf, err := modfile.Parse(f.Name, f.Content, nil)
	if err != nil {
		return nil, err
	}
	if mf.Module == nil {
		return nil, nil
	}

	comp := &component{
		name:     mf.Module.Mod.Path,
		language: "go",
		attrs:    schema.Attrs{"module": mf.Module.Mod.Path},
		line:     1,
	}
	if mf.Go != nil {
		comp.attrs["goVersion"] = mf.Go.Version
	}
	for _, req := range mf.Require {
		// Indirect requirements are the transitive closure, not choices this
		// codebase made, and including them would flood the table lookups
		// with dependencies nobody here imports.
		if req.Indirect {
			continue
		}
		comp.dependencies = append(comp.dependencies, req.Mod.Path)
	}
	return comp, nil
}

type packageJSON struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
}

func parsePackageJSON(f *scan.File) (*component, error) {
	var pkg packageJSON
	if err := json.Unmarshal(f.Content, &pkg); err != nil {
		return nil, err
	}
	if pkg.Name == "" {
		// A workspace root or a bare config file; nothing names a component.
		return nil, nil
	}

	comp := &component{
		name:     pkg.Name,
		language: "javascript",
		attrs:    schema.Attrs{"package": pkg.Name},
		line:     1,
	}
	if pkg.Version != "" {
		comp.attrs["version"] = pkg.Version
	}
	if node, ok := pkg.Engines["node"]; ok {
		comp.attrs["nodeVersion"] = node
	}
	// Dev dependencies are build and test tooling, not runtime architecture.
	for name := range pkg.Dependencies {
		comp.dependencies = append(comp.dependencies, name)
	}
	if _, ok := pkg.DevDependencies["typescript"]; ok {
		comp.language = "typescript"
	}
	if _, ok := pkg.Dependencies["typescript"]; ok {
		comp.language = "typescript"
	}
	return comp, nil
}

type pyproject struct {
	Project struct {
		Name           string   `toml:"name"`
		Version        string   `toml:"version"`
		Dependencies   []string `toml:"dependencies"`
		RequiresPython string   `toml:"requires-python"`
	} `toml:"project"`
	Tool struct {
		Poetry struct {
			Name         string         `toml:"name"`
			Version      string         `toml:"version"`
			Dependencies map[string]any `toml:"dependencies"`
		} `toml:"poetry"`
	} `toml:"tool"`
}

func parsePyproject(f *scan.File) (*component, error) {
	var pp pyproject
	if err := toml.Unmarshal(f.Content, &pp); err != nil {
		return nil, err
	}

	comp := &component{language: "python", line: 1, attrs: schema.Attrs{}}
	switch {
	case pp.Project.Name != "":
		comp.name = pp.Project.Name
		comp.attrs["package"] = pp.Project.Name
		if pp.Project.Version != "" {
			comp.attrs["version"] = pp.Project.Version
		}
		if pp.Project.RequiresPython != "" {
			comp.attrs["pythonVersion"] = pp.Project.RequiresPython
		}
		for _, dep := range pp.Project.Dependencies {
			comp.dependencies = append(comp.dependencies, requirementName(dep))
		}
	case pp.Tool.Poetry.Name != "":
		comp.name = pp.Tool.Poetry.Name
		comp.attrs["package"] = pp.Tool.Poetry.Name
		if pp.Tool.Poetry.Version != "" {
			comp.attrs["version"] = pp.Tool.Poetry.Version
		}
		for name := range pp.Tool.Poetry.Dependencies {
			if name != "python" {
				comp.dependencies = append(comp.dependencies, name)
			}
		}
	default:
		return nil, nil
	}
	return comp, nil
}

// parseRequirements cannot fail: a requirements file is a list of lines, and
// a line that does not parse is simply not a dependency.
func parseRequirements(f *scan.File) *component {
	comp := &component{
		// requirements.txt names no package, so the directory is the only
		// identity available.
		name:     path.Base(orDot(f.Dir)),
		language: "python",
		attrs:    schema.Attrs{},
		line:     1,
	}
	for _, line := range strings.Split(string(f.Content), "\n") {
		if name := requirementName(line); name != "" {
			comp.dependencies = append(comp.dependencies, name)
		}
	}
	if len(comp.dependencies) == 0 {
		return nil
	}
	return comp
}

// requirementName pulls the package name out of a requirements line,
// discarding version specifiers, environment markers, comments, and the
// option lines pip allows.
func requirementName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
		return ""
	}
	if i := strings.Index(line, "#"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	// Environment markers: "celery ; python_version >= '3.9'".
	if i := strings.Index(line, ";"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	// A direct URL reference names the package before the @.
	if i := strings.Index(line, "@"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if i := strings.IndexAny(line, "=<>!~"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if strings.ContainsAny(line, " \t/\\") {
		return ""
	}
	return line
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}
