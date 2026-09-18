// Package buildinfo reports which revision the running binary was built from.
//
// A test run against a deployed environment is not evidence unless you know
// what it ran against, and a deployment that has silently stopped updating —
// a blocked pipeline, a failed push, an unannounced rollback — looks exactly
// like a healthy one from the outside. A health endpoint returning
// {"status":"ok"} is telling the truth throughout.
//
// The revision comes from a linker flag when the build sets one, and from Go's
// VCS stamp otherwise. When neither is available Get reports Unknown rather
// than something that could be mistaken for an answer.
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
// no VCS stamp: a local `go run`, or an image built from an exported tree. A
// word rather than an empty string, which would be indistinguishable from a
// revision that renders as blank.
const Unknown = "unknown"

// Info is the build identity, as served.
type Info struct {
	Revision string `json:"revision"`
	BuiltAt  string `json:"built_at,omitempty"`

	// Dirty reports that the tree had uncommitted changes at build time. Such a
	// revision names a commit whose contents are not what is running, so a
	// comparison against it can be wrong rather than merely unavailable.
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

// resolve is Get with its inputs passed in rather than read from the process,
// so a test can supply a VCS stamp. Written against the package variables, a
// test of the precedence rule passes even with the precedence branch deleted: a
// `go test` binary carries no vcs.* settings, so there is never a stamp for the
// injected value to outrank.
//
// The linker flag wins. An image is built from a checkout whose stamp may be
// missing or may name a parent commit, while the pipeline knows the revision it
// was told to ship. The VCS stamp is the fallback, so a plain `go build` still
// produces a comparable identity.
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
