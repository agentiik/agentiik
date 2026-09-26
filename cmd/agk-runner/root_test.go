//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/runner"
)

// asRootHost is a host whose serve is started as root, as the image starts it, with the account
// running the test standing for agentiik, since it is the one a test can give a directory to
// without being root. What it would become is recorded rather than done.
type asRootHost struct {
	*host
	agent  runner.Owner
	became []int
	calls  int
}

func newRootHost(t *testing.T) *asRootHost {
	t.Helper()
	h := &asRootHost{host: newHost(t, daemon(t, true), ""), agent: runner.Owner{UID: os.Getuid(), GID: os.Getgid()}}
	h.e.Geteuid = func() int { return 0 }
	h.e.Account = func(name string) (runner.Owner, error) {
		if name != agentAccount {
			return runner.Owner{}, errors.New("no account " + name)
		}
		return h.agent, nil
	}
	h.e.Become = func(agent runner.Owner, groups []int) error {
		h.calls++
		if agent != h.agent {
			t.Errorf("serve became account %+v, want %+v", agent, h.agent)
		}
		h.became = groups
		return nil
	}
	// Directories nobody made yet, as a bind source is before Docker creates it, one level
	// below where anything is, and each its own.
	fresh := t.TempDir()
	h.e.KeyFile = filepath.Join(fresh, "lib", "runner.key")
	h.e.CredentialFile = filepath.Join(fresh, "state", "credential")
	h.set("AGK_RUNNER_WORKDIR", filepath.Join(fresh, "work", "root"))
	env := filepath.Join(fresh, "etc", "runner.env")
	if err := os.MkdirAll(filepath.Dir(env), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(h.e.EnvFile, env); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(env), 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.EnvFile = env
	return h
}

// socketOwnedBy has the daemon socket stand for one owned by group, which a test cannot give a file
// to where it is not in it.
func socketOwnedBy(t *testing.T, group int) {
	was := socketGroupOf
	socketGroupOf = func(path string) (int, error) {
		if _, err := socketGroup(path); err != nil {
			return 0, err
		}
		return group, nil
	}
	t.Cleanup(func() { socketGroupOf = was })
}

// Started as root, serve prepares what the agent needs, takes the group of the socket it was given
// and becomes the agent's account, before asking the API anything or saying it is ready.
func TestServeStartedAsRootTakesTheSocketsGroupAndBecomesTheAgent(t *testing.T) {
	h := newRootHost(t)
	socketOwnedBy(t, 4242)
	// Where runner.env goes on a host that never joined, in a directory nobody made yet, as a
	// volume's is before anything is written to it.
	h.e.EnvFile = filepath.Join(t.TempDir(), "etc", "agentiik", "runner.env")
	if code := run(context.Background(), h.e, []string{"serve"}); code != exitSucceeded {
		t.Fatalf("serve started as root exited %d:\n%s", code, h.err)
	}
	if h.calls != 1 {
		t.Fatalf("serve became the agent %d times, want once:\n%s", h.calls, h.err)
	}
	if want := []int{h.agent.GID, 4242}; !slices.Equal(h.became, want) {
		t.Errorf("serve took the groups %v, want the agent's and the socket's, %v", h.became, want)
	}
	work, _ := h.e.Lookup("AGK_RUNNER_WORKDIR")
	for _, dir := range []string{filepath.Dir(h.e.KeyFile), filepath.Dir(h.e.CredentialFile), work, filepath.Dir(h.e.EnvFile)} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not prepared for the agent: %v", dir, err)
			continue
		}
		if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != h.agent.UID {
			t.Errorf("%s is account %d's, want the agent's, %d", dir, st.Uid, h.agent.UID)
		}
	}
	if info, err := os.Stat(work); err == nil && info.Mode().Perm() != 0o700 {
		t.Errorf("the work root has mode %#o, want 0700", info.Mode().Perm())
	}
	if n := h.requests.Load(); n > 0 {
		t.Errorf("%d requests reached the API before serve became the agent", n)
	}
	if said := h.heard(50 * time.Millisecond); said != "" {
		t.Errorf("systemd was told %q by serve as root", said)
	}
}

// The work root may be written in runner.env alone, as the page advises before joining, and it is
// that one serve prepares, although runner.env is the agent's and not root's.
func TestServeStartedAsRootPreparesTheWorkRootRunnerEnvNames(t *testing.T) {
	h := newRootHost(t)
	work := filepath.Join(t.TempDir(), "elsewhere", "work")
	h.set("AGK_RUNNER_WORKDIR", "")
	text, err := os.ReadFile(h.e.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.e.EnvFile, append(text, []byte("AGK_RUNNER_WORKDIR="+work+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), h.e, []string{"serve"}); code != exitSucceeded {
		t.Fatalf("serve started as root exited %d:\n%s", code, h.err)
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Errorf("the work root runner.env names was not prepared: %v", err)
	}
}

// A work root that is a link is refused rather than followed, since root would give away whatever
// it points at.
func TestServeStartedAsRootRefusesAWorkRootThatIsALink(t *testing.T) {
	h := newRootHost(t)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	h.set("AGK_RUNNER_WORKDIR", link)
	said := h.refused(t)
	if !strings.Contains(said, link) || h.calls != 0 {
		t.Errorf("serve took a work root that is a link, becoming the agent %d times:\n%s", h.calls, said)
	}
}

// serve never serves as root: where it cannot become the agent's account, or that account is root,
// it refuses the start, and says why.
func TestServeStartedAsRootRefusesWhereItCannotBecomeTheAgent(t *testing.T) {
	for _, c := range []struct {
		name  string
		spoil func(h *asRootHost)
		want  string
	}{
		{"the drop fails", func(h *asRootHost) {
			h.e.Become = func(runner.Owner, []int) error {
				return errors.New("account 65532 cannot be taken, which takes CAP_SETUID")
			}
		}, "CAP_SETUID"},
		{"there is no such account", func(h *asRootHost) {
			h.e.Account = func(string) (runner.Owner, error) {
				return runner.Owner{}, errors.New("the account agentiik cannot be found")
			}
		}, "cannot be found"},
		{"the account is root", func(h *asRootHost) {
			h.agent = runner.Owner{}
		}, "is root"},
		{"there is no daemon socket", func(h *asRootHost) {
			h.set("DOCKER_HOST", filepath.Join(t.TempDir(), "docker.sock"))
		}, "docker.sock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRootHost(t)
			c.spoil(h)
			said := h.refused(t)
			for _, want := range []string{"refuses to serve as root", c.want} {
				if !strings.Contains(said, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, said)
				}
			}
		})
	}
}

// The kernel's own drop, which only root can take: the helper process becomes nobody in groups of
// its choosing and starts a program, which finds itself that account in those groups.
func TestBecomeTakesTheAccountAndItsGroupsAndStartsTheProgramAgain(t *testing.T) {
	if os.Getenv("AGK_TEST_BECOME") == "1" {
		err := become(runner.Owner{UID: 65534, GID: 65534}, []int{65534, 4242}, "/bin/sh", []string{"sh", "-c", "id -u; id -g; id -G"}, os.Environ())
		// Reached only where the exec failed.
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("taking another account takes root, on Linux, and this test runs as account " + strconv.Itoa(os.Geteuid()) + " on " + runtime.GOOS)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBecomeTakesTheAccountAndItsGroupsAndStartsTheProgramAgain$")
	cmd.Env = append(os.Environ(), "AGK_TEST_BECOME=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, out)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 3 || lines[0] != "65534" || lines[1] != "65534" || !slices.Contains(lines[2:], "4242") || slices.Contains(lines[2:], "0") {
		t.Errorf("the program started as %q, want account 65534 in group 65534 and 4242 and not in 0", out)
	}
}
