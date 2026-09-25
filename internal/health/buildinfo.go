package health

import (
	"runtime/debug"
	"time"
)

// BuildStamp is what the binary knows about its own provenance.
type BuildStamp struct {
	// Commit is the full VCS revision, when the binary was built from a
	// checkout; empty for a `git archive` build and for `go run`.
	Commit string
	// Date is the commit time from the VCS stamp, or the ldflags build date a
	// caller supplies when there is no stamp.
	Date time.Time
	// Dirty reports vcs.modified=true: a tree with uncommitted changes, which
	// no published build can be matched against.
	Dirty bool
}

// BuildInfo reads the toolchain's VCS stamp. It is what turns "23713ea" into a
// commit the fleet page can compare with the manifest the release host
// publishes, and it is why the fleet could not attribute the build it was
// running until now: the published binary carried a vcs.revision that names no
// commit in this repository, and nothing reported it.
//
// fallbackDate is used when the stamp carries no vcs.time: a build from an
// archive, or one whose Makefile passed -X main.BuildDate instead.
func BuildInfo(fallbackDate string) BuildStamp {
	var b BuildStamp
	if t, err := time.Parse(time.RFC3339, fallbackDate); err == nil {
		b.Date = t.UTC()
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				b.Date = t.UTC()
			}
		case "vcs.modified":
			b.Dirty = s.Value == "true"
		}
	}
	return b
}
