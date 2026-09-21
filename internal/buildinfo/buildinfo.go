// Package buildinfo carries version metadata stamped in at link time.
//
// GoReleaser sets the corresponding variables in package main, which calls
// Set before handing control to the CLI. When built with a plain `go build`
// the values fall back to the module's embedded VCS stamp, and finally to
// "dev".
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/kamronarabi/structura/pkg/schema"
)

// SchemaVersion is the version of the Structura Architecture Graph format
// emitted by this binary. It is versioned independently of the CLI: a CLI
// bugfix does not imply a schema change, and the cloud tier must be able to
// ingest graphs produced by many CLI versions at once.
//
// It aliases the schema package's own constant rather than restating it. Two
// literals would drift the first time one of them was bumped, and the symptom
// would be `structura version` reporting a schema version different from the
// one the binary actually writes into every graph -- which is worse than no
// report at all, because it is believable.
const SchemaVersion = schema.Version

var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Set records the link-time values. Empty arguments are ignored so that a
// plain `go build` keeps the VCS-derived fallbacks.
func Set(v, c, d string) {
	if v != "" {
		version = v
	}
	if c != "" {
		commit = c
	}
	if d != "" {
		date = d
	}
	fillFromVCS()
}

func fillFromVCS() {
	if commit != "" && date != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "" {
				date = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = true
			}
		}
	}
}

var dirty bool

// Version returns the CLI version string.
func Version() string { return version }

// Commit returns the VCS revision the binary was built from, if known.
func Commit() string { return commit }

// Date returns the build timestamp, if known.
func Date() string { return date }

// Dirty reports whether the working tree had uncommitted changes at build time.
func Dirty() bool { return dirty }

// String renders the full multi-line version banner.
func String() string {
	v := version
	if dirty {
		v += " (dirty)"
	}
	return fmt.Sprintf(
		"structura %s\n  commit:  %s\n  built:   %s\n  go:      %s\n  platform: %s/%s\n  schema:  %s",
		v, orNA(commit), orNA(date), runtime.Version(), runtime.GOOS, runtime.GOARCH, SchemaVersion,
	)
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
