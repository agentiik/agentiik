package helper

import (
	"embed"
	"io/fs"
	"slices"
	"strings"
)

// carried is what this build has to offer: the binaries of cmd/agk-helper, built for the
// architectures a container may run on natively, as the release build puts them in bin/.
//
// The directive is unconditional and bin/ carries a committed README, because //go:embed
// refuses a directory it matches nothing in and this module has to compile on a machine that
// has never built the helper. A build that skipped that stage therefore carries no binary,
// which is not a broken build: Extract answers that there is none, Policy.Helper stays empty
// and the driver binds nothing.
//
//go:embed bin
var carried embed.FS

// embeddedDir is where the binaries sit inside the embedded filesystem.
const embeddedDir = "bin"

// prefix is what a carried binary is named by: the program as it is spelled inside the
// container, then the platform it was built for, which is how one is chosen without reading
// any of them.
const prefix = "agk-"

// Embedded is what this build carries, by name, sorted.
//
// An empty answer is a build that skipped the helper stage, which binds nothing. It is here so
// that a command line can say what it has rather than discovering it by failing to find one.
func Embedded() []string {
	entries, err := fs.ReadDir(carried, embeddedDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// carriedName is the binary this build would lay down for one platform, which is the name the
// release build writes and nothing clever: the platform is in the name so that two
// architectures can sit in one directory and in one working directory.
func carriedName(ostype, arch string) string {
	goarch := architecture(arch)
	if ostype == "" || goarch == "" {
		return ""
	}
	return prefix + ostype + "-" + goarch
}

// architecture maps what a daemon calls its machine to what Go calls it.
//
// It is needed because the two do not agree and the disagreement is silent: Docker answers
// aarch64 on the machine Go calls arm64 and x86_64 on the one it calls amd64, and a binary
// chosen by the daemon's spelling would be a file that is not there or, worse, the wrong
// machine's. An architecture neither spelling covers answers with nothing, which is a build
// that carries no binary for this daemon rather than a guess.
func architecture(arch string) string {
	switch arch {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	default:
		return ""
	}
}
