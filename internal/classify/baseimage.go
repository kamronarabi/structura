package classify

import "strings"

// A Dockerfile's FROM line is the only place many repositories say what a
// service is written in. A multi-stage build says it twice over: the builder
// stage names the toolchain, the final stage names the runtime, and between
// them they describe a Go binary in a scratch image or a React bundle served
// by nginx without either fact appearing in any other file.
//
// This is recognition over well-known base images, the same bargain
// knownImages makes: the table is honest about being a table, and an image
// nobody has heard of yields no language rather than a guessed one. Guessing
// here would be cheap to do and expensive to be wrong about, because a
// language is what the graph prints next to a component's name.

// baseImageLanguages maps a base image's final path element to the language
// it runs. Keys are matched against the same name variants as knownImages, so
// golang:1.22-alpine and node:20-slim resolve from "golang" and "node".
var baseImageLanguages = map[string]string{
	"node":   "javascript",
	"nodejs": "javascript",
	"bun":    "javascript",
	"deno":   "typescript",

	"golang": "go",

	"python": "python",
	"pypy":   "python",

	"ruby":           "ruby",
	"jruby":          "ruby",
	"rails":          "ruby",
	"passenger-full": "ruby",

	// The JVM is distributed under a vendor name far more often than under
	// "java": Temurin, Corretto and Semeru are the same runtime rebadged, and
	// a build tool image is a JVM image that also has the build tool.
	"openjdk":             "java",
	"eclipse-temurin":     "java",
	"temurin":             "java",
	"amazoncorretto":      "java",
	"corretto":            "java",
	"sapmachine":          "java",
	"ibm-semeru-runtimes": "java",
	"liberica-openjdk":    "java",
	"graalvm":             "java",
	"gradle":              "java",
	"maven":               "java",
	"tomcat":              "java",
	"jetty":               "java",

	"rust": "rust",

	"php": "php",

	"elixir": "elixir",
	"erlang": "erlang",

	"haskell": "haskell",
	"swift":   "swift",
	"perl":    "perl",
	"dart":    "dart",
	"clojure": "clojure",
	"julia":   "julia",
	"r-base":  "r",
}

// dotnetImages are the .NET base images, which are published under a
// repository path rather than a distinctive name: the final element of
// mcr.microsoft.com/dotnet/aspnet is "aspnet", and "sdk" on its own means
// nothing at all. The repository is what identifies them.
var dotnetImages = map[string]bool{
	"aspnet":       true,
	"sdk":          true,
	"runtime":      true,
	"runtime-deps": true,
	"monitor":      true,
}

// BaseImageLanguage reports the language a container base image runs, and
// false for a base that names no language: an operating system, a web server,
// a distroless runtime, or an image the table does not recognize.
func BaseImageLanguage(ref string) (language string, ok bool) {
	img := ParseImage(ref)
	if img.Name == "" {
		return "", false
	}
	for _, name := range imageNameVariants(img.Name) {
		if lang, found := baseImageLanguages[name]; found {
			return lang, true
		}
		if dotnetImages[name] && strings.Contains(strings.ToLower(img.Repository), "dotnet") {
			return "csharp", true
		}
	}
	return "", false
}

// BaseImageIsOS reports whether a base image is an operating system or an
// empty runtime -- a base that says a component was built somewhere else and
// only copied in here. It is not the absence of a language: an unrecognized
// image is also languageless, but it might be anything, whereas these are
// known to carry no application of their own.
func BaseImageIsOS(ref string) bool {
	if strings.EqualFold(strings.TrimSpace(ref), "scratch") {
		return true
	}
	img := ParseImage(ref)
	if strings.Contains(strings.ToLower(img.Repository), "distroless") {
		return true
	}
	for _, name := range imageNameVariants(img.Name) {
		if osImages[name] {
			return true
		}
	}
	return false
}

var osImages = map[string]bool{
	"scratch": true, "alpine": true, "debian": true, "ubuntu": true,
	"busybox": true, "centos": true, "fedora": true, "rockylinux": true,
	"almalinux": true, "opensuse": true, "amazonlinux": true,
	"debian-base": true, "debian-base-amd64": true, "distroless": true,
	"static": true, "base": true, "oraclelinux": true, "photon": true,
	"wolfi-base": true, "chainguard": true,
}
