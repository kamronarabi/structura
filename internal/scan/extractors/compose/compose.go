// Package compose extracts an architecture graph from Docker Compose files.
//
// Compose is the highest-signal format Structura reads: it names every
// service, states the image each runs, and — uniquely among the Phase 1
// formats — declares dependencies outright via depends_on. Most of what other
// extractors have to infer is written down here.
package compose

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/yamlpos"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier, which appears in every source
// and evidence record it produces.
const Name = "compose"

func init() {
	// compose-go logs interpolation warnings through the global logrus
	// logger. Those warnings are expected here — an unset variable is normal
	// when interpolating against an empty environment — and the MCP server
	// shares this process, where stray output on the wrong stream breaks the
	// protocol. The scan reports the same information as diagnostics.
	logrus.SetOutput(io.Discard)
}

// Extractor reads Compose files.
type Extractor struct{}

// New returns a Compose extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// composeNames are the filenames Compose itself recognizes.
var composeNames = map[string]bool{
	"docker-compose.yml": true, "docker-compose.yaml": true,
	"compose.yml": true, "compose.yaml": true,
}

// Match implements scan.Extractor. It reads no file contents: classification
// has to stay cheap enough to run on every file in a large monorepo.
func (*Extractor) Match(f scan.FileMeta) bool {
	name := strings.ToLower(f.Name)
	if composeNames[name] {
		return true
	}
	// Override and environment variants: docker-compose.prod.yml,
	// compose.override.yaml.
	for _, prefix := range []string{"docker-compose.", "compose."} {
		if strings.HasPrefix(name, prefix) && (f.Ext == ".yml" || f.Ext == ".yaml") {
			return true
		}
	}
	return false
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(ctx context.Context, f *scan.File, emit scan.Emitter) error {
	project, err := load(ctx, f)
	if err != nil {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityWarn,
			Code:     "compose_parse_failed",
			Path:     f.Path,
			Message:  fmt.Sprintf("could not load this Compose file: %v", err),
		})
		// A file that will not parse is a reported gap, not a scan failure.
		return nil
	}

	pos := yamlpos.New(f.Content)
	projectName := projectName(project, f)

	// The project itself is a boundary, which is what gives the context-level
	// view something to group services under.
	boundaryID := schema.NewNodeID(schema.KindBoundary, Name, projectName, projectName)
	emit.Node(schema.Node{
		ID:         boundaryID,
		Kind:       schema.KindBoundary,
		Layer:      schema.LayerContext,
		Name:       projectName,
		Attrs:      schema.Attrs{"orchestrator": "docker-compose"},
		Sources:    []schema.Source{{Extractor: Name, Path: f.Path, Line: 1}},
		Confidence: schema.ConfDeclared,
	})

	// Services are visited in sorted order so that the emitted sequence does
	// not depend on map iteration.
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	ids := make(map[string]string, len(names))
	for _, name := range names {
		ids[name] = nodeID(projectName, name, project.Services[name].Image)
	}

	for _, name := range names {
		svc := project.Services[name]
		e.emitService(f, pos, emit, projectName, boundaryID, name, svc, ids)
	}
	return nil
}

func (e *Extractor) emitService(
	f *scan.File, pos *yamlpos.Locator, emit scan.Emitter,
	projectName, boundaryID, name string, svc composetypes.ServiceConfig,
	ids map[string]string,
) {
	line := pos.KeyLine("services", name)
	src := schema.Source{Extractor: Name, Path: f.Path, Line: line}
	id := ids[name]

	kind, tech, _ := classify.ImageKind(svc.Image)

	attrs := schema.Attrs{}
	if svc.Image != "" {
		attrs["image"] = svc.Image
	}
	if svc.Build != nil {
		// A build stanza means the image is first-party, whatever its name
		// happens to look like.
		kind = schema.KindService
		// The context is stored repo-relative rather than as written. A
		// Compose file in deploy/ saying "../services/gateway" and a go.mod
		// in services/gateway are the same component, and normalizing here
		// is what lets the resolver notice that instead of emitting the
		// service twice under two names.
		if ctx := repoRelativeBuildContext(f.Dir, svc.Build.Context); ctx != "" {
			attrs["buildContext"] = ctx
		}
	}
	if ports := servicePorts(svc); len(ports) > 0 {
		attrs["ports"] = ports
	}
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		attrs["replicas"] = *svc.Deploy.Replicas
	}
	if len(svc.Profiles) > 0 {
		profiles := append([]string(nil), svc.Profiles...)
		sort.Strings(profiles)
		attrs["profiles"] = profiles
	}
	if vols := serviceVolumes(svc); len(vols) > 0 {
		attrs["volumes"] = vols
	}
	if svc.Restart != "" {
		attrs["restart"] = svc.Restart
	}

	emit.Node(schema.Node{
		ID:         id,
		Kind:       kind,
		Layer:      schema.LayerContainer,
		Name:       name,
		Namespace:  projectName,
		Tech:       techFor(tech),
		Attrs:      attrs,
		Sources:    []schema.Source{src},
		Confidence: schema.ConfDeclared,
	})

	emit.Edge(schema.Edge{
		From: boundaryID, To: id, Kind: schema.EdgeContains,
		Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{
			Extractor: Name, Path: f.Path, Line: line,
			Rule:   "compose_project_member",
			Detail: fmt.Sprintf("service %q is declared in Compose project %q", name, projectName),
		}},
	})

	e.emitDependsOn(f, pos, emit, name, id, svc, ids)
	e.emitEnvironment(f, pos, emit, name, id, svc)
}

// emitDependsOn turns declared dependencies into edges.
//
// This is the one place in Phase 1 where a relationship is stated rather than
// inferred, so it is the only edge kind that earns full confidence. The
// target is resolved against the services in this same file: an extractor may
// only emit an edge it can justify without looking at anything else. A
// depends_on naming a service defined in another Compose file is left to the
// resolver instead, since emitting it here would produce an edge pointing at
// a node that does not exist.
func (e *Extractor) emitDependsOn(
	f *scan.File, pos *yamlpos.Locator, emit scan.Emitter,
	name, id string, svc composetypes.ServiceConfig, ids map[string]string,
) {
	deps := make([]string, 0, len(svc.DependsOn))
	for dep := range svc.DependsOn {
		deps = append(deps, dep)
	}
	sort.Strings(deps)

	line := pos.KeyLine("services", name, "depends_on")
	if line == 0 {
		line = pos.KeyLine("services", name)
	}

	for _, dep := range deps {
		cfg := svc.DependsOn[dep]
		targetID, known := ids[dep]
		if !known {
			emit.Hint(resolve.Hint{
				FromNode:      id,
				Kind:          resolve.HintDependsOn,
				Raw:           dep,
				Tokens:        []string{strings.ToLower(dep)},
				SuggestedEdge: schema.EdgeDependsOn,
				Source: schema.Evidence{
					Extractor: Name, Path: f.Path, Line: line,
					Rule:   "compose_depends_on",
					Detail: fmt.Sprintf("%q depends_on %q, which is not declared in this file", name, dep),
				},
			})
			continue
		}
		emit.Edge(schema.Edge{
			From: id, To: targetID, Kind: schema.EdgeDependsOn,
			Confidence: schema.ConfDeclared,
			Evidence: []schema.Evidence{{
				Extractor: Name, Path: f.Path, Line: line,
				Rule:   "compose_depends_on",
				Detail: fmt.Sprintf("%q declares depends_on %q (condition: %s)", name, dep, cfg.Condition),
			}},
		})
	}
}

// emitEnvironment turns environment variables that name a location into
// hints. The value may point at a service in this file, in another file, or
// at a third-party API; deciding which is the resolver's job, because only it
// sees the whole node set.
func (e *Extractor) emitEnvironment(
	f *scan.File, pos *yamlpos.Locator, emit scan.Emitter,
	name, id string, svc composetypes.ServiceConfig,
) {
	keys := make([]string, 0, len(svc.Environment))
	for k := range svc.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	envLine := pos.KeyLine("services", name, "environment")
	if envLine == 0 {
		envLine = pos.KeyLine("services", name)
	}

	for _, k := range keys {
		v := svc.Environment[k]
		if v == nil || *v == "" {
			continue
		}
		ref, ok := resolve.ParseValue(k, *v)
		if !ok {
			continue
		}
		// The raw value is masked here as well as at serialization: a
		// connection string routinely carries a password, and this one is
		// about to be copied into a hint that --debug-dump will print.
		redacted, _ := resolve.RedactValue(k, *v)
		emit.Hint(resolve.Hint{
			FromNode:      id,
			Kind:          ref.Kind,
			Raw:           redacted,
			Tokens:        ref.Tokens,
			Port:          ref.Port,
			Protocol:      ref.Protocol,
			SuggestedEdge: ref.Edge,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: envLine,
				Rule:   "compose_env_reference",
				Detail: fmt.Sprintf("env %s=%s", k, redacted),
			},
		})
	}
}

// load parses one Compose file in isolation.
//
// Interpolation runs against an empty environment rather than the process's.
// That is a deliberate trade: "${TAG:-1.0.0}" still resolves to its default,
// while "${DB_PASSWORD}" resolves to empty instead of reaching into the
// developer's shell and copying a real secret into a file that gets
// committed. A scan must never be able to exfiltrate something that was not
// already in the repository.
func load(ctx context.Context, f *scan.File) (*composetypes.Project, error) {
	return composeloader.LoadWithContext(ctx, composetypes.ConfigDetails{
		WorkingDir:  path.Dir(f.Path),
		ConfigFiles: []composetypes.ConfigFile{{Filename: f.Name, Content: f.Content}},
		Environment: composetypes.Mapping{},
	}, func(o *composeloader.Options) {
		o.SkipConsistencyCheck = true
		// Path resolution would turn relative build contexts into absolute
		// paths on this machine, which must not reach the graph.
		o.ResolvePaths = false
		o.SkipValidation = false
		o.SetProjectName(defaultProjectName(f), false)
	})
}

func projectName(p *composetypes.Project, f *scan.File) string {
	if p != nil && p.Name != "" {
		return p.Name
	}
	return defaultProjectName(f)
}

// defaultProjectName mirrors Compose's own rule of naming the project after
// the directory holding the file.
func defaultProjectName(f *scan.File) string {
	if f.Dir == "" {
		return "default"
	}
	return path.Base(f.Dir)
}

func nodeID(projectName, serviceName, image string) string {
	kind, _, _ := classify.ImageKind(image)
	return schema.NewNodeID(kind, Name, projectName, serviceName)
}

// repoRelativeBuildContext resolves a Compose build context against the
// directory holding the Compose file. A context that escapes the repository
// root is dropped rather than recorded: it cannot be joined to anything the
// scan saw, and an absolute or parent-relative path must never reach the
// graph.
func repoRelativeBuildContext(composeDir, buildContext string) string {
	buildContext = strings.TrimSpace(buildContext)
	if buildContext == "" || path.IsAbs(buildContext) {
		return ""
	}
	resolved := path.Clean(path.Join(composeDir, buildContext))
	if resolved == "." {
		return "."
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return ""
	}
	return resolved
}

func techFor(tech string) *schema.Tech {
	t := &schema.Tech{Runtime: "container"}
	if tech != "" {
		t.Framework = tech
	}
	return t
}

// servicePorts returns the container-side ports, sorted and deduplicated.
// The published (host) port is deliberately not used for identity: it is a
// local convenience that changes between environments, while the container
// port is a property of the service.
func servicePorts(svc composetypes.ServiceConfig) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range svc.Ports {
		if p.Target > 0 && !seen[int(p.Target)] {
			seen[int(p.Target)] = true
			out = append(out, int(p.Target))
		}
	}
	for _, e := range svc.Expose {
		if n, err := strconv.Atoi(strings.TrimSpace(e)); err == nil && n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// serviceVolumes returns named volume mounts, which indicate that a service
// holds state. Bind mounts are excluded: they are a local development
// convenience and say nothing about the deployed architecture.
func serviceVolumes(svc composetypes.ServiceConfig) []string {
	var out []string
	for _, v := range svc.Volumes {
		if v.Type == "volume" && v.Source != "" {
			out = append(out, v.Source)
		}
	}
	sort.Strings(out)
	return out
}
