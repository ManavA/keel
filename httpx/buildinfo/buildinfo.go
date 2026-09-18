// Package buildinfo answers the one question a running service cannot
// otherwise answer about itself: which build is this.
//
// A test suite that runs against a deployed environment and reports green is
// not evidence unless you know what it ran against. A deployment that has
// silently stopped updating — a blocked pipeline, a failed push, a rollback
// nobody announced — produces exactly the same green as a healthy one, and a
// health endpoint returning {"status":"ok"} is telling the truth the whole
// time, because the database really is fine. The build is the missing field.
//
// The revision comes from a linker flag when the pipeline sets one, and from
// Go's own VCS stamp otherwise. It is never defaulted to something that looks
// like an answer: when nothing is known, Get reports Unknown, and callers are
// expected to treat that as "cannot compare" rather than "matches".
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Revision is the commit the binary was built from, injected at link time:
//
//	go build -ldflags "-X github.com/ManavA/keel/httpx/buildinfo.Revision=$SHA"
var Revision string

// BuiltAt is an RFC 3339 build timestamp, injected the same way. Optional: a
// revision on its own answers the staleness question, and a timestamp without a
// revision cannot.
var BuiltAt string

// Unknown is what Get reports when nothing was injected and the build carries
// no VCS stamp — a local `go run`, or an image built from an exported tree. It
// is a word rather than an empty string so that it cannot be mistaken for a
// revision that happens to render as blank in a log line or a JSON field.
const Unknown = "unknown"

// Info is the build identity, as served.
type Info struct {
	Revision string `json:"revision"`
	BuiltAt  string `json:"built_at,omitempty"`

	// Dirty reports that the tree had uncommitted changes at build time. A
	// revision from a dirty tree names a commit whose contents are not what is
	// running, so a comparison against it can be confidently wrong — which is
	// worse than being unable to compare at all.
	Dirty bool `json:"dirty,omitempty"`
}

// Known reports whether this identity can be compared with anything.
func (i Info) Known() bool { return i.Revision != "" && i.Revision != Unknown }

// Short is the revision abbreviated for a log line, or Unknown.
func (i Info) Short() string {
	if !i.Known() {
		return Unknown
	}
	if len(i.Revision) <= 12 {
		return i.Revision
	}
	return i.Revision[:12]
}

// Get resolves the build identity of the running binary.
func Get() Info {
	var settings []debug.BuildSetting
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
	}
	return resolve(Revision, BuiltAt, settings)
}

// resolve is Get with its inputs passed in rather than read from the process.
//
// The split is not stylistic. Written against the package variables, a test for
// the precedence rule below passes while the precedence branch is deleted: a
// `go test` binary carries no vcs.* settings at all, so there is never a stamp
// for the injected value to outrank and both sides of the comparison are the
// string "unknown". Taking the settings as an argument is the only way a test
// can supply one.
//
// The linker flag wins. An image is built from a checkout whose stamp may be
// missing or may name a vendored parent commit, whereas a pipeline knows the
// revision it was told to ship. The VCS stamp is the fallback, so a plain
// `go build` with nobody remembering the flag still produces a comparable
// identity — the point is to remove a failure mode, not to move it into a build
// script.
func resolve(injectedRev, injectedAt string, settings []debug.BuildSetting) Info {
	info := Info{
		Revision: strings.TrimSpace(injectedRev),
		BuiltAt:  strings.TrimSpace(injectedAt),
	}
	if info.Revision != "" {
		return info
	}

	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.time":
			if info.BuiltAt == "" {
				info.BuiltAt = s.Value
			}
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		}
	}
	if info.Revision == "" {
		info.Revision = Unknown
	}
	return info
}
