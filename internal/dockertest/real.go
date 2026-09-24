package dockertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Socket answers where a real Docker daemon is, and whether there is one.
//
// It is one function rather than a condition copied into every file that needs one, which
// is what keeps the suite green on a machine where no daemon exists and honest on one
// where one does. A test that needs one asks, and ends through Unavailable when the answer
// is false.
//
// The three places it looks are the three places a daemon is: DOCKER_HOST, which an
// operator sets and which a rootless daemon always sets; the per-user path Docker
// Desktop puts its socket at, which is where the daemon on the machine this was written
// on lives; and the system path.
func Socket() (string, bool) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		path := strings.TrimPrefix(h, "unix://")
		if !strings.Contains(path, "://") && exists(path) {
			return path, true
		}
		return "", false
	}
	if home, err := os.UserHomeDir(); err == nil {
		desktop := filepath.Join(home, ".docker", "run", "docker.sock")
		if exists(desktop) {
			return desktop, true
		}
	}
	if exists("/var/run/docker.sock") {
		return "/var/run/docker.sock", true
	}
	return "", false
}

// exists says whether a socket is there. It is a stat and not a dial: a daemon that is
// there but busy is still a daemon, and a test that skipped on a slow answer would be a
// test nobody could rely on.
func exists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

// RequireDocker is the variable that makes a real daemon, and what the tests need on it,
// a requirement rather than a convenience. Set to 1, a test that would have skipped for
// want of either fails instead.
//
// It exists because a skip is silent. go test prints ok for a package whose every real
// test skipped, so a job with no image to run a container from was as green as one that
// ran them all, and the tests that hold the settings table, secret masking and the network
// postures against a kernel had never run in CI. CI sets it; a laptop does not have to.
const RequireDocker = "AGENTIIK_TEST_REQUIRE_DOCKER"

// Required says whether RequireDocker is set to 1.
func Required() bool {
	return os.Getenv(RequireDocker) == "1"
}

// T is the part of a *testing.T that Unavailable uses. It is an interface so that this
// package still takes no *testing.T, and so that its own test can hand it something that
// records rather than stops.
type T interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Unavailable ends a test that needs something of a real daemon this machine cannot give
// it: the daemon itself, an answer from it, an image on it, or the docker command. It
// skips, unless Required, and then it fails and says why it did not skip.
//
// Every test that reaches a real daemon ends through here rather than through a t.Skip of
// its own, for the reason Socket is one function: a rule copied into every file that needs
// it is as many rules, and the copy that forgets is the test that quietly never runs.
func Unavailable(t T, format string, args ...any) {
	t.Helper()
	if Required() {
		t.Fatalf("%s is 1, so this fails where it would have skipped: %s", RequireDocker, fmt.Sprintf(format, args...))
		return
	}
	t.Skipf(format, args...)
}
