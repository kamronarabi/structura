package scan

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// A scan that reports nothing is read as a scan that found everything.
//
// Structura's diagnostics say what it could not parse. They said nothing at
// all about what it never opened, so a repository whose architecture lives in
// a format no extractor handles produced a thin graph, an empty gap list, and
// a reader with no way to tell the difference between "there is nothing here"
// and "I cannot read this". On a React application with a Dockerfile, four of
// five files went unexamined and the tool answered "everything this scan
// recognized, it parsed".
//
// Listing every unread file would be worse than silence: most files in any
// repository are source code, and saying so on every scan trains a reader to
// skip the section that matters. What is worth reporting is the narrow set of
// files that plainly describe architecture and that no extractor reads yet --
// a Dockerfile, a Fly or Vercel configuration, a Serverless manifest. Those
// are the cases where something real is missing and the reason is Structura's
// rather than the repository's.
//
// Each entry disappears on its own the day an extractor claims the file,
// because only files that matched nothing are considered.

// unsupportedFormat is a file type that describes architecture and that no
// extractor reads.
type unsupportedFormat struct {
	// singular and plural name the format in a sentence.
	singular string
	plural   string
	// missingOne and missingMany say what is absent from the graph because
	// the file went unread. Both forms are spelled out because one file and
	// several read differently, and a diagnostic that says "1 Dockerfile ...
	// the ports they expose" reads like a template nobody finished.
	missingOne  string
	missingMany string
	// match uses metadata only, like an extractor's Match.
	match func(f FileMeta) bool
}

// unsupportedFormats is deliberately short. Every entry has to be a file
// whose presence says something architectural on its own, from its name
// alone: no entry may depend on reading the contents, and none may be a name
// that is commonly something else. A "template.yaml" might be CloudFormation
// or might be anything, so it is not here.
var unsupportedFormats = []unsupportedFormat{
	{
		singular: "Dockerfile", plural: "Dockerfiles",
		missingOne:  "the base image it builds on, the ports it exposes, and its build stages",
		missingMany: "the base images they build on, the ports they expose, and their build stages",
		match: func(f FileMeta) bool {
			name := strings.ToLower(f.Name)
			return name == "dockerfile" || name == "containerfile" ||
				strings.HasPrefix(name, "dockerfile.") || f.Ext == ".dockerfile"
		},
	},
	{
		singular: "Vercel configuration", plural: "Vercel configurations",
		missingOne:  "the routes, rewrites, and functions it deploys",
		missingMany: "the routes, rewrites, and functions they deploy",
		match:       byName("vercel.json"),
	},
	{
		singular: "Netlify configuration", plural: "Netlify configurations",
		missingOne:  "the redirects, functions, and build settings it declares",
		missingMany: "the redirects, functions, and build settings they declare",
		match:       byName("netlify.toml"),
	},
	{
		singular: "Fly.io configuration", plural: "Fly.io configurations",
		missingOne:  "the services, ports, and processes it declares",
		missingMany: "the services, ports, and processes they declare",
		match:       byName("fly.toml"),
	},
	{
		singular: "Render configuration", plural: "Render configurations",
		missingOne:  "the services and databases it declares",
		missingMany: "the services and databases they declare",
		match:       byName("render.yaml", "render.yml"),
	},
	{
		singular: "Railway configuration", plural: "Railway configurations",
		missingOne:  "the services it declares",
		missingMany: "the services they declare",
		match:       byName("railway.json", "railway.toml"),
	},
	{
		singular: "Procfile", plural: "Procfiles",
		missingOne:  "the processes it runs",
		missingMany: "the processes they run",
		match: func(f FileMeta) bool {
			name := strings.ToLower(f.Name)
			return name == "procfile" || strings.HasPrefix(name, "procfile.")
		},
	},
	{
		singular: "Serverless Framework manifest", plural: "Serverless Framework manifests",
		missingOne:  "the functions, events, and resources it deploys",
		missingMany: "the functions, events, and resources they deploy",
		match:       byName("serverless.yml", "serverless.yaml"),
	},
	{
		singular: "Pulumi program", plural: "Pulumi programs",
		missingOne:  "the cloud resources it declares",
		missingMany: "the cloud resources they declare",
		match:       byName("pulumi.yaml", "pulumi.yml"),
	},
	{
		singular: "Skaffold configuration", plural: "Skaffold configurations",
		missingOne:  "the artifacts it builds and the manifests it deploys",
		missingMany: "the artifacts they build and the manifests they deploy",
		match:       byName("skaffold.yaml", "skaffold.yml"),
	},
	{
		singular: "Supabase configuration", plural: "Supabase configurations",
		missingOne:  "the managed Postgres, auth, and storage it configures",
		missingMany: "the managed Postgres, auth, and storage they configure",
		match: func(f FileMeta) bool {
			return strings.ToLower(f.Name) == "config.toml" &&
				strings.ToLower(path.Base(f.Dir)) == "supabase"
		},
	},
}

func byName(names ...string) func(FileMeta) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = true
	}
	return func(f FileMeta) bool { return set[strings.ToLower(f.Name)] }
}

// matchUnsupported reports whether a file is a known architectural format
// that no extractor reads, and which one. It uses metadata only, like an
// extractor's Match, and the walker calls it on every file it did not claim.
func matchUnsupported(f FileMeta) (int, bool) {
	for i := range unsupportedFormats {
		if unsupportedFormats[i].match(f) {
			return i, true
		}
	}
	return 0, false
}

// unsupportedDiagnostics reports the architectural files that no extractor
// claimed, one entry per format rather than one per file.
func unsupportedDiagnostics(files []FileMeta) []schema.Diagnostic {
	matched := map[int][]string{}
	for _, f := range files {
		if i, ok := matchUnsupported(f); ok {
			matched[i] = append(matched[i], f.Path)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(matched))
	for i := range matched {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)

	out := make([]schema.Diagnostic, 0, len(indexes))
	for _, i := range indexes {
		format := unsupportedFormats[i]
		paths := matched[i]
		sort.Strings(paths)

		d := schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "unsupported_format",
		}
		if len(paths) == 1 {
			// A single file can be pointed at, and Path already carries it,
			// so repeating it in the message would only make it longer.
			d.Path = paths[0]
			// "are", not "is": the clause is always a list of things, and
			// the file being singular does not change that.
			d.Message = fmt.Sprintf("this %s was found but not read: %s are not in the graph",
				format.singular, format.missingOne)
		} else {
			d.Message = fmt.Sprintf("%d %s were found but not read (%s): %s are not in the graph",
				len(paths), format.plural, list(paths), format.missingMany)
		}
		out = append(out, d)
	}
	return out
}
