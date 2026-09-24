package runner

import (
	"runtime/debug"
	"strings"
)

// Version is the version of this agent, as join sends it and agk-runner version prints it.
//
// It is the module's version as the build recorded it, without its leading v, since the wire
// writes agent_version as the release is tagged in semantic versioning, 0.2.0 and not v0.2.0. A
// build from a checkout between releases records a pseudo-version, which is a semantic version
// too, and one that records none, as a build outside version control does, is 0.0.0-devel: a
// version that orders before every release, which is what a build nobody released is.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return devel
	}
	return versionOf(info.Main.Version)
}

// devel is the version of a build that recorded none.
const devel = "0.0.0-devel"

func versionOf(recorded string) string {
	v, tagged := strings.CutPrefix(recorded, "v")
	if !tagged || v == "" {
		return devel
	}
	return v
}
