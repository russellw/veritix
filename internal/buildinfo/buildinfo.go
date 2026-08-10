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
