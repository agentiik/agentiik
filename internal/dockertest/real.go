package dockertest

import (
	"os"
	"path/filepath"
	"strings"
)

// Socket answers where a real Docker daemon is, and whether there is one.
//
// It is one function rather than a skip condition copied into every file that needs one,
// which is what keeps the suite green in CI where no daemon exists and honest on a
// machine where one does. A test that needs a daemon asks, and skips when the answer is
// false.
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
