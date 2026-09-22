// Package dockerfile extracts a component and its runtime from a Dockerfile.
//
// A Dockerfile is the most common architectural file there is, and until now
// Structura only reported that it had not read one. What it states that
// nothing else in a repository necessarily does:
//
// A directory holds something deployable. A Compose file or a Deployment says
// that too, but a great many repositories have neither -- a service built by
// CI and pushed to a platform declares its existence in a Dockerfile and
// nowhere else. That directory identity is also what a .env file beside it
// binds to, so reading the Dockerfile is what gives an otherwise anonymous
// tree a component for its configuration to hang on.
//
// What it is written in. A multi-stage build says it twice: the builder stage
// names the toolchain and the final stage names the runtime, which is how a
// Go binary in a scratch image or a React bundle served by nginx can be
// recognized as Go or JavaScript with no manifest present at all.
//
// What port it listens on, which narrows a host:port reference elsewhere to
// one component instead of several.
//
// Two things it deliberately does not do.
//
// Build stages are not components. A three-stage build is one thing that
// ships, and drawing the stages would put boxes on the diagram that nothing
// deploys and nobody operates. The stage graph is walked only to find what
// the final image is really based on.
//
// Several Dockerfiles in one directory are one component. Dockerfile beside
// Dockerfile.debug or Dockerfile.dev is a variant of the same service, which
// is the overwhelmingly common reason for a second one, so they collapse onto
// one node and the file names are recorded on it. A directory that really
// holds two different services in two Dockerfiles is drawn as one box, and
// says so in a diagnostic.
package dockerfile

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "dockerfile"

// Extractor reads Dockerfiles.
type Extractor struct{}

// New returns a Dockerfile extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// Match implements scan.Extractor.
//
// The four spellings in the wild: Dockerfile, Containerfile (Podman's name
// for the same file), Dockerfile.<variant>, and <variant>.dockerfile, which
// is what editors key syntax highlighting off.
func (*Extractor) Match(f scan.FileMeta) bool {
	name := strings.ToLower(f.Name)
	switch {
	case name == "dockerfile" || name == "containerfile":
		return true
	case strings.HasPrefix(name, "dockerfile."), strings.HasPrefix(name, "containerfile."):
		// Dockerfile.md is documentation about one, and .dockerignore is not
		// a build file at all.
		return f.Ext != ".md" && f.Ext != ".txt" && f.Ext != ".yaml" && f.Ext != ".yml"
	case f.Ext == ".dockerfile":
		return true
	}
	return false
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	instructions := parse(f.Content)
	stages := stagesOf(instructions)
	if len(stages) == 0 {
		// A file matching the name with no FROM in it is not a build we can
		// say anything about: a fragment, a template, or a file named after a
		// Dockerfile that is something else.
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "dockerfile_no_base_image",
			Path:     f.Path,
			Message: "this file has no FROM instruction, so nothing could be said about " +
				"what it builds or what the result runs on",
		})
		return nil
	}

	dir := f.Dir
	if dir == "" {
		dir = "."
	}
	base := finalBase(stages)
	language, _ := languageOf(stages)
	kind, tech := runtimeOf(base)

	idPath := dir
	if idPath == "." {
		idPath = "root"
	}
	id := schema.NewNodeID(kind, "", idPath)

	attrs := schema.Attrs{
		"dockerfile": f.Name,
		"directory":  dir,
		// A Dockerfile names no component, so its name is the directory's.
		// The resolver needs to know that, because a derived name has to lose
		// to a declared one when a manifest in the same directory turns out
		// to describe the same component.
		"nameFrom": "directory",
	}
	if base != "" {
		attrs["baseImage"] = base
	}
	if n := len(stages); n > 1 {
		attrs["buildStages"] = n
	}
	if ports := exposedPorts(instructions, buildArgs(instructions)); len(ports) > 0 {
		attrs["ports"] = ports
	}

	emit.Node(schema.Node{
		ID:      id,
		Kind:    kind,
		Layer:   schema.LayerContainer,
		Name:    componentName(dir),
		Tech:    techOf(language, tech),
		Attrs:   attrs,
		Sources: []schema.Source{{Extractor: Name, Path: f.Path, Line: stages[0].line}},
		// A Dockerfile proves something builds here, not that it is deployed
		// as its own service. An infrastructure file saying so raises this,
		// through the merge.
		Confidence: schema.ConfHostMatch,
	})

	// How the rest of the repository refers to this: the image CI pushes is
	// conventionally named after the directory the Dockerfile sits in. A
	// directory whose name describes its role names no image, and claiming
	// that "image" or "src" routes here would make the name ambiguous in
	// every repository that has two of them -- so those are left alone. A
	// build context still finds this component, because it points at the
	// directory rather than at a name.
	dirBase := path.Base(dir)
	if dirBase != "" && dirBase != "." && dirBase != "/" && !genericDirs[strings.ToLower(dirBase)] {
		emit.Alias(resolve.Alias{
			Name:       dirBase,
			DNS:        []string{dirBase},
			TargetName: componentName(dir),
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: stages[0].line,
				Rule:   "dockerfile_directory_name",
				Detail: fmt.Sprintf("%s builds the code in %s", f.Name, dir),
			},
		})
	}

	e.emitReferences(f, emit, id, instructions)
	return nil
}

// emitReferences turns ENV values that name a location into hints.
//
// An environment variable baked into the image pointing at a database or an
// API is the same reference a .env file would carry, and reaching it needs no
// more than the file in hand.
//
// ARG is read for ports and never for references, because a build argument
// does not exist in the running container: it is a parameter to the build, and
// its default is the value for whoever builds without supplying one. Drawing a
// component from one would put a build-time placeholder on the diagram.
//
// The known cost is the promotion idiom, ARG API_URL followed by ENV
// API_URL=$API_URL, where the endpoint really is baked in and this reports
// nothing. Resolving it would mean substituting build defaults into hostnames,
// and those defaults are localhost and example.com far more often than they
// are a real dependency. A gap is visible; a wrong edge is not.
func (e *Extractor) emitReferences(f *scan.File, emit scan.Emitter, id string, instructions []instruction) {
	for _, ins := range instructions {
		if ins.keyword != "ENV" {
			continue
		}
		for _, a := range assignments(ins.keyword, ins.args) {
			// A value still holding a build-time variable names nothing yet.
			// The interpolated form is decided by whoever runs the build, and
			// matching "$DB_HOST" against the node set is how a resolver
			// starts inventing edges.
			if strings.Contains(a.value, "$") {
				continue
			}
			ref, ok := resolve.ParseValue(a.key, a.value)
			if !ok {
				continue
			}
			redacted, _ := resolve.RedactValue(a.key, a.value)
			emit.Hint(resolve.Hint{
				FromNode:      id,
				Kind:          ref.Kind,
				Raw:           redacted,
				Tokens:        ref.Tokens,
				Port:          ref.Port,
				Protocol:      ref.Protocol,
				SuggestedEdge: ref.Edge,
				Source: schema.Evidence{
					Extractor: Name, Path: f.Path, Line: ins.line,
					Rule:   "dockerfile_reference",
					Detail: fmt.Sprintf("%s %s=%s", ins.keyword, a.key, redacted),
				},
			})
		}
	}
}

// genericDirs are directory names that describe a directory's role rather than
// naming anything. A Dockerfile in src/cartservice/src belongs to cartservice,
// and calling the component "src" puts a box on the diagram that a reader
// cannot place -- worse still in a repository with three of them, where the
// same meaningless label appears three times.
var genericDirs = map[string]bool{
	"src": true, "source": true, "sources": true,
	"image": true, "images": true, "docker": true, "dockerfiles": true,
	"build": true, "builds": true, "deploy": true, "deployment": true,
	"app": true, "apps": true, "code": true,
	"container": true, "containers": true,
	"dist": true, "bin": true, "pkg": true,
}

// componentName names the thing the Dockerfile builds, from the nearest
// directory above it that names something.
//
// At the repository root there is nothing to name it after: the directory is
// the repository, whose name the extractor cannot see because it is given
// repo-relative paths. "app" is a placeholder, and in practice it is
// short-lived -- a manifest, a Compose service or a workload almost always
// names the same component, and the merge keeps the declared name.
func componentName(dir string) string {
	for d := dir; d != "" && d != "." && d != "/"; d = path.Dir(d) {
		base := path.Base(d)
		if !genericDirs[strings.ToLower(base)] {
			return base
		}
	}
	return "app"
}

func techOf(language, framework string) *schema.Tech {
	if language == "" && framework == "" {
		return nil
	}
	return &schema.Tech{Language: language, Framework: framework}
}

// runtimeOf says what the finished image is, from the base it was built on.
//
// A Dockerfile starting FROM postgres builds a customized Postgres, and
// calling that a service would put a datastore on the diagram as a box with
// no kind. Anything the table does not recognize is a service, which is the
// conservative answer for a first-party base.
func runtimeOf(base string) (kind schema.NodeKind, framework string) {
	if base == "" {
		return schema.KindService, ""
	}
	kind, tech, recognized := classify.ImageKind(base)
	if !recognized || classify.BaseImageIsOS(base) {
		return schema.KindService, ""
	}
	// A language base image is a runtime for this repository's code, not a
	// product this repository ships. python:3.12 is not "a Python".
	if _, isLanguage := classify.BaseImageLanguage(base); isLanguage {
		return schema.KindService, tech
	}
	// Nor is a web server. FROM nginx over a built bundle is the ordinary way
	// to ship a single-page application, and the component that comes out is
	// the application, served by nginx -- not a piece of infrastructure. A
	// datastore base is the other way round: a Dockerfile starting FROM
	// postgres is shipping a Postgres.
	if kind == schema.KindCloudResource {
		return schema.KindService, tech
	}
	return kind, tech
}

// languageOf finds the language the build uses, preferring the stage that
// ships over the stages that only produce input for it.
//
// The final stage is asked first because that is what runs. When it names no
// language -- a scratch or distroless image, an nginx serving a bundle -- the
// earlier stages are asked in order, because the first stage of a multi-stage
// build is where the application is compiled.
func languageOf(stages []stage) (string, bool) {
	last := len(stages) - 1
	if lang, ok := classify.BaseImageLanguage(resolveBase(stages, last)); ok {
		return lang, true
	}
	for i := 0; i < last; i++ {
		if lang, ok := classify.BaseImageLanguage(resolveBase(stages, i)); ok {
			return lang, true
		}
	}
	return "", false
}

// finalBase is the image the shipped stage is really built on, following stage
// references back to a real image.
func finalBase(stages []stage) string {
	return resolveBase(stages, len(stages)-1)
}

// resolveBase follows a stage that is based on another stage back to the image
// underneath it. "FROM builder" says nothing about a runtime on its own.
func resolveBase(stages []stage, idx int) string {
	if idx < 0 || idx >= len(stages) {
		return ""
	}
	seen := map[int]bool{}
	for !seen[idx] {
		seen[idx] = true
		image := stages[idx].image
		next := -1
		// A stage may only be based on one declared before it, so searching
		// backwards cannot loop through a forward reference -- but a file may
		// still name a stage after itself, which the seen set catches.
		for j := 0; j < idx; j++ {
			if stages[j].name != "" && strings.EqualFold(stages[j].name, image) {
				next = j
			}
		}
		if next < 0 {
			return image
		}
		idx = next
	}
	return ""
}

// exposedPorts collects every EXPOSE across the file, sorted and deduplicated.
//
// Every stage's EXPOSE is taken, not just the shipped stage's: a port declared
// in a builder stage is still the port this component listens on, and
// distinguishing them buys nothing a reader would use.
//
// "EXPOSE $PORT" is read through the file's own ARG and ENV defaults, because
// otherwise the answer is worse than nothing: the file that prompted this
// writes ARG PORT=80 and then EXPOSE $PORT 9229 9230, and reporting the two
// debug ports while dropping the one the service actually listens on is a
// confident wrong answer rather than a gap.
func exposedPorts(instructions []instruction, args map[string]string) []int {
	seen := map[int]bool{}
	for _, ins := range instructions {
		if ins.keyword != "EXPOSE" {
			continue
		}
		for _, field := range strings.Fields(substitute(ins.args, args)) {
			// "8080/tcp" and "8080/udp" are the same port to a reader.
			spec, _, _ := strings.Cut(field, "/")
			port, err := strconv.Atoi(spec)
			// A variable nothing in the file defines is decided at run time
			// and names no port here.
			if err != nil || port <= 0 || port > 65535 {
				continue
			}
			seen[port] = true
		}
	}
	out := make([]int, 0, len(seen))
	for port := range seen {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// buildArgs collects the values the file sets for itself, in order, so that
// later instructions written in terms of them can be read.
//
// These are used for ports and nothing else. A port is a fact about the image:
// whoever wrote ARG PORT=80 is saying the process listens on 80, and an
// override changes a number. A hostname default is a different thing -- it is
// the value for whoever builds without supplying one, which in practice is
// nobody -- so ENV values holding a variable stay unresolved and emit no
// reference. Substituting there would put a build-time placeholder on the
// diagram as a component, which is the mistake the .env extractor exists to
// avoid.
func buildArgs(instructions []instruction) map[string]string {
	out := map[string]string{}
	for _, ins := range instructions {
		if ins.keyword != "ENV" && ins.keyword != "ARG" {
			continue
		}
		for _, a := range assignments(ins.keyword, ins.args) {
			// Resolved as it is stored, so that the ARG PORT=80 / ENV PORT
			// $PORT pair -- which is how a Dockerfile promotes a build
			// argument into the image -- lands on 80 rather than on "$PORT".
			out[a.key] = substitute(a.value, out)
		}
	}
	return out
}

// variablePattern matches $NAME and ${NAME}. Shell's default and substring
// forms, ${NAME:-x} and the rest, are deliberately not matched: a value that
// needs them is not one a simple reading understands, and it stays as written
// so that nothing downstream mistakes it for a resolved one.
var variablePattern = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)

// substitute replaces variables the file defined. One pass, so a value
// defined in terms of itself cannot loop, and anything undefined is left as
// written rather than becoming an empty string -- an empty string looks like
// an answer.
func substitute(text string, args map[string]string) string {
	if !strings.Contains(text, "$") || len(args) == 0 {
		return text
	}
	return variablePattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := variablePattern.FindStringSubmatch(match)
		name := groups[1]
		if name == "" {
			name = groups[2]
		}
		if value, ok := args[name]; ok && !strings.Contains(value, "$") {
			return value
		}
		return match
	})
}

// stage is one FROM in a Dockerfile.
type stage struct {
	// image is the reference as written, which may name an earlier stage.
	image string
	// name is the "AS <name>" label, empty when the stage has none.
	name string
	line int
}

func stagesOf(instructions []instruction) []stage {
	var out []stage
	for _, ins := range instructions {
		if ins.keyword != "FROM" {
			continue
		}
		if s, ok := parseFrom(ins.args); ok {
			s.line = ins.line
			out = append(out, s)
		}
	}
	return out
}

// parseFrom reads "FROM [--flag=value ...] <image> [AS <name>]".
func parseFrom(args string) (stage, bool) {
	fields := strings.Fields(args)
	// --platform=$BUILDPLATFORM is on a third of the Dockerfiles in the wild.
	for len(fields) > 0 && strings.HasPrefix(fields[0], "--") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return stage{}, false
	}
	s := stage{image: fields[0]}
	if len(fields) >= 3 && strings.EqualFold(fields[1], "as") {
		s.name = fields[2]
	}
	return s, true
}

// assignment is one KEY=VALUE from an ENV or ARG instruction.
type assignment struct {
	key   string
	value string
}

// assignments reads both forms an ENV instruction may take: the modern
// "ENV KEY=value KEY2=value2", and the legacy "ENV KEY the rest of the line",
// where the value is unquoted text that may contain spaces. ARG has only the
// first form, and only one variable per instruction.
func assignments(keyword, args string) []assignment {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}

	first := strings.Fields(args)[0]
	if keyword == "ENV" && !strings.Contains(first, "=") {
		value, ok := unquote(strings.TrimSpace(strings.TrimPrefix(args, first)))
		if !ok || value == "" {
			return nil
		}
		return []assignment{{key: first, value: value}}
	}

	var out []assignment
	for _, token := range splitPairs(args) {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		unquoted, ok := unquote(strings.TrimSpace(value))
		if key == "" || !ok || unquoted == "" {
			continue
		}
		out = append(out, assignment{key: key, value: unquoted})
	}
	return out
}

// splitPairs splits on whitespace that is not inside quotes, so that
// ENV A="one two" B=three yields two tokens rather than three.
func splitPairs(args string) []string {
	var (
		out   []string
		token strings.Builder
		quote rune
	)
	flush := func() {
		if token.Len() > 0 {
			out = append(out, token.String())
			token.Reset()
		}
	}
	for _, r := range args {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			token.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			token.WriteRune(r)
		case r == ' ' || r == '\t':
			flush()
		default:
			token.WriteRune(r)
		}
	}
	flush()
	return out
}

// unquote removes a matching pair of surrounding quotes. An unterminated quote
// returns false: the value is not something a simple reading understands, and
// guessing at where it ends is how a hostname gets invented.
func unquote(v string) (string, bool) {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1], true
		}
	}
	if strings.ContainsAny(v, `"'`) {
		return "", false
	}
	return v, true
}

// instruction is one Dockerfile instruction, with line continuations joined.
type instruction struct {
	// keyword is uppercased, because Dockerfile instructions are
	// case-insensitive and half the world writes "from".
	keyword string
	args    string
	// line is where the keyword appeared, 1-based.
	line int
}

// parse reads Dockerfile syntax far enough to find the instructions that say
// something architectural.
//
// This is deliberately not a Dockerfile implementation. The escape directive
// and shell semantics inside RUN are real and neither can produce a base
// image, a port, or a hostname that this reading would miss. Anything not
// understood is skipped rather than guessed at.
//
// Heredocs are the exception, and are handled rather than ignored. Two in five
// of the Dockerfiles in the test corpus use one, and their contents are
// arbitrary text: a script that writes a configuration file can easily hold a
// line beginning with ENV or FROM, which a line-by-line reading would take for
// an instruction of this build. Reading a value out of someone else's file and
// attributing it to this component is the kind of mistake that is invisible
// afterwards.
func parse(content []byte) []instruction {
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	lines := strings.Split(string(content), "\n")

	var (
		out       []instruction
		joined    strings.Builder
		startLine int
		// heredocs holds the terminators still open, because one instruction
		// may open several: COPY <<A <<B dest is legal.
		heredocs []string
	)
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if len(heredocs) > 0 {
			// A terminator is matched against the line as written, except
			// that <<- allows it to be indented.
			if trimmed == heredocs[0] || line == heredocs[0] {
				heredocs = heredocs[1:]
			}
			continue
		}

		// A comment inside a continuation is dropped by the builder rather
		// than ending it, so the same rule applies whether or not one is
		// open.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if joined.Len() == 0 {
			if trimmed == "" {
				continue
			}
			startLine = i + 1
		}

		continues := strings.HasSuffix(trimmed, `\`)
		if continues {
			trimmed = strings.TrimSuffix(trimmed, `\`)
		}
		if joined.Len() > 0 {
			joined.WriteString(" ")
		}
		joined.WriteString(trimmed)
		if continues {
			continue
		}

		text := joined.String()
		joined.Reset()
		if ins, ok := instructionOf(text, startLine); ok {
			out = append(out, ins)
		}
		// The body starts on the line after the instruction that opened it,
		// which is why this is checked once the continuations are joined.
		heredocs = heredocTerminators(text)
	}
	// A file ending mid-continuation still stated what it stated.
	if joined.Len() > 0 {
		if ins, ok := instructionOf(joined.String(), startLine); ok {
			out = append(out, ins)
		}
	}
	return out
}

// heredocPattern matches a heredoc redirection and captures its terminator.
// The word may be quoted, which in shell means no expansion and here means
// nothing: either way it ends the body.
var heredocPattern = regexp.MustCompile(`<<-?\s*(?:"([^"]+)"|'([^']+)'|([A-Za-z_][A-Za-z0-9_]*))`)

// heredocTerminators lists the words that close the bodies an instruction
// opened, in the order they must appear.
func heredocTerminators(text string) []string {
	var out []string
	for _, m := range heredocPattern.FindAllStringSubmatch(text, -1) {
		for _, group := range m[1:] {
			if group != "" {
				out = append(out, group)
				break
			}
		}
	}
	return out
}

func instructionOf(text string, line int) (instruction, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return instruction{}, false
	}
	keyword, args, _ := strings.Cut(text, " ")
	return instruction{
		keyword: strings.ToUpper(strings.TrimSpace(keyword)),
		args:    strings.TrimSpace(args),
		line:    line,
	}, true
}
