// Package buildinfo exposes version metadata stamped in at link time.
package buildinfo

import "runtime/debug"

// Overridden via -ldflags at release time; the fallbacks come from the Go
// build info so that `go install`-ed binaries still report something useful.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// License and SourceURL are what the binary says about its own terms, in the
// version output and in the web interface's footer.
//
// SourceURL is a var rather than a const because a fork has to be able to
// change it. AGPL section 13 puts the obligation on whoever modifies Veritix
// and serves it over a network: their users must be offered *their* source,
// not this repository. A fork that relinks is one -ldflags away from
// complying; an operator who does not rebuild sets server.source_url instead,
// which is why config carries the same field and this is only its default.
var (
	License   = "AGPL-3.0-or-later, or a commercial license"
	SourceURL = "https://github.com/russellwallace/veritix"
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "" {
		Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = s.Value
			}
		}
	}
}

// Short returns a one-line human-readable version string.
func Short() string {
	v := Version
	if v == "" {
		v = "dev"
	}
	if Commit != "" {
		if len(Commit) > 12 {
			return v + " (" + Commit[:12] + ")"
		}
		return v + " (" + Commit + ")"
	}
	return v
}
