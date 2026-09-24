package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/runner"
)

// credential is a runner credential written the way token.New writes one.
const credential = "agkrunner_Xy9QkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3oY2"

// output is a buffer the agent's log and a test can share.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// host is one runner host as serve finds it: a joined runner.env, a runner.toml or none, a daemon,
// a service manager listening, and an API that counts what reaches it.
type host struct {
	e        env
	out, err *output
	requests atomic.Int32
	notify   *net.UnixConn
}

// newHost lays a host out around a daemon. policy is the text of runner.toml, and "" is no file.
func newHost(t *testing.T, daemon *dockertest.Daemon, policy string) *host {
	t.Helper()
	h := &host{out: &output{}, err: &output{}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(api.Close)

	dir := t.TempDir()
	envFile := filepath.Join(dir, "runner.env")
	joined := strings.Join([]string{
		"AGK_API=" + api.URL,
		"AGK_RUNNER_ID=runner-dmz-02",
		"AGK_RUNNER_POOL=dmz",
		"AGK_RUNNER_LABELS=zone=dmz,arch=amd64",
		"AGK_RUNNER_CREDENTIAL=" + credential,
		"",
	}, "\n")
	if err := os.WriteFile(envFile, []byte(joined), 0o600); err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(dir, "runner.toml")
	if policy != "" {
		if err := os.WriteFile(policyFile, []byte(policy), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var socket string
	socket, h.notify = notifySocket(t)
	vars := map[string]string{
		"DOCKER_HOST":            daemon.Socket(),
		"AGK_RUNNER_WORKDIR":     filepath.Join(dir, "work"),
		runner.NotifySocket:      socket,
		"AGK_RUNNER_CONCURRENCY": "2",
	}
	h.e = env{
		Out: h.out, Err: h.err,
		Lookup: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
		Geteuid:    func() int { return 1000 },
		EnvFile:    envFile,
		PolicyFile: policyFile,
		Host:       installed{caps: ownership, fs: driver.Filesystem{Tmpfs: true, NoExec: true, NoSUID: true, NoDev: true}},
	}
	return h
}

// installed is the machine an agent installed as the page says finds, whoever runs the test:
// the three capabilities its unit grants, and a secrets directory on a tmpfs mounted
// noexec,nosuid,nodev. A test takes one away to see the start refused.
type installed struct {
	caps uint64
	fs   driver.Filesystem
}

func (m installed) Capabilities() (uint64, error)                { return m.caps, nil }
func (m installed) Filesystem(string) (driver.Filesystem, error) { return m.fs, nil }

// ownership is CAP_CHOWN, CAP_DAC_OVERRIDE and CAP_FOWNER, bits 0, 1 and 3 of the kernel's sets.
const ownership = 1<<0 | 1<<1 | 1<<3

// secretsTmpfs is the line of runner.toml that names the host's secrets tmpfs, which a start
// that is to say ready needs: without it a secret value would have nowhere to go but a disk.
const secretsTmpfs = "secrets_dir = \"/run/agentiik/secrets\"\n"

// set adds a variable to the host's environment.
func (h *host) set(name, value string) {
	lookup := h.e.Lookup
	h.e.Lookup = func(n string) (string, bool) {
		if n == name {
			return value, true
		}
		return lookup(n)
	}
}

// notifySocket listens where systemd would, in a directory short enough for a unix socket.
func notifySocket(t *testing.T) (string, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agkn")
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(dir, "notify")) > 100 {
		os.RemoveAll(dir)
		if dir, err = os.MkdirTemp("/tmp", "agkn"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	name := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return name, conn
}

// heard is what the service manager was told within a wait, or "" for nothing.
func (h *host) heard(wait time.Duration) string {
	h.notify.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 4096)
	n, err := h.notify.Read(b)
	if err != nil {
		return ""
	}
	return string(b[:n])
}

func daemon(t *testing.T, remapped bool) *dockertest.Daemon {
	t.Helper()
	var bs []dockertest.Behaviour
	if remapped {
		bs = append(bs, dockertest.WithUsernsRemap(165536, 165536))
	}
	d, err := dockertest.NewDaemon(bs...)
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// refused runs serve on a host that should refuse it, and holds it to having refused before it
// asked the API anything or told systemd it was ready.
func (h *host) refused(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := run(ctx, h.e, append([]string{"serve"}, args...))
	if code == exitSucceeded {
		t.Fatalf("serve started, and it should have refused:\n%s", h.err)
	}
	if n := h.requests.Load(); n > 0 {
		t.Errorf("%d requests reached the API before the start was refused", n)
	}
	if said := h.heard(50 * time.Millisecond); said != "" {
		t.Errorf("systemd was told %q by a start that was refused", said)
	}
	return h.err.String()
}

// serving runs serve until it tells systemd it is ready, then stops it, and holds it to having
// stopped cleanly.
func (h *host) serving(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, h.e, []string{"serve"}) }()

	said := h.heard(20 * time.Second)
	cancel()
	code := <-done
	if said != runner.Ready {
		t.Fatalf("systemd was told %q, want %s, and serve exited %d:\n%s", said, runner.Ready, code, h.err)
	}
	if code != exitSucceeded {
		t.Errorf("serve stopped by its service manager exited %d:\n%s", code, h.err)
	}
}

func TestServeRefusesADaemonWithoutUsernsRemapBeforeAnyRequestReachesTheAPI(t *testing.T) {
	h := newHost(t, daemon(t, false), "")
	// No variable lifts the floor, whatever it is called.
	h.set("AGK_REQUIRE_USERNS_REMAP", "false")
	h.set("AGK_RUNNER_REQUIRE_USERNS_REMAP", "false")
	said := h.refused(t)
	for _, want := range []string{"does not remap user namespaces", "require_userns_remap = false"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
	// A missing runner.toml keeps the floor, and says so.
	if !strings.Contains(said, "there is no "+h.e.PolicyFile) {
		t.Errorf("the agent did not say it found no %s:\n%s", h.e.PolicyFile, said)
	}
}

func TestNoFlagLiftsTheFloor(t *testing.T) {
	h := newHost(t, daemon(t, false), "")
	for _, flag := range []string{"--require-userns-remap=false", "-require_userns_remap=false", "--insecure"} {
		if code := run(context.Background(), h.e, []string{"serve", flag}); code != exitUsage {
			t.Errorf("serve %s exited %d, want %d", flag, code, exitUsage)
		}
	}
	if n := h.requests.Load(); n > 0 {
		t.Errorf("%d requests reached the API", n)
	}
}

func TestRequireUsernsRemapFalseInRunnerTomlIsTheOneWayPastTheFloor(t *testing.T) {
	h := newHost(t, daemon(t, false), "require_userns_remap = false\n"+secretsTmpfs)
	h.serving(t)
}

func TestARunnerTomlThatCannotBeReadRefusesTheStartRatherThanFallingBack(t *testing.T) {
	for _, policy := range []string{
		"require_userns_remap = \"no\"\n",
		"require_userns_remap = false\nrequire_usernsremap = true\n",
		"this is not toml\n",
	} {
		t.Run(strings.SplitN(policy, "\n", 2)[0], func(t *testing.T) {
			// A remapping daemon, so that the refusal is the file's and not the floor's.
			h := newHost(t, daemon(t, true), policy)
			said := h.refused(t)
			if !strings.Contains(said, h.e.PolicyFile) {
				t.Errorf("the refusal does not name %s:\n%s", h.e.PolicyFile, said)
			}
		})
	}
}

func TestADirectoryAsRunnerTomlRefusesTheStart(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if err := os.Mkdir(h.e.PolicyFile, 0o700); err != nil {
		t.Fatal(err)
	}
	h.refused(t)
}

func TestServeSaysReadyOnceTheFloorHoldsAndTheDaemonIsOpen(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.serving(t)
	for _, want := range []string{"serving as runner-dmz-02 in pool dmz", "2 tasks at once", "the daemon speaking API"} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the agent's log does not say %q:\n%s", want, h.err)
		}
	}
	if strings.Contains(h.err.String(), credential[len("agkrunner_"):]) {
		t.Errorf("the agent's log carries its credential:\n%s", h.err)
	}
}

// An agent that died leaves the network of every internal task it had in flight. The next
// one removes those no container is on before it takes any work, and says so.
func TestServeSweepsTheTaskNetworksAnEarlierAgentLeft(t *testing.T) {
	d := daemon(t, true)
	cli, err := docker.Dial(d.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.NetworkCreate(t.Context(), docker.NetworkSpec{
		Name: "agk-01JMZ8V1P9C4_invoice_1", Internal: true,
		Labels: map[string]string{driver.LabelTask: "01JMZ8V1P9C4/invoice/1"},
	}); err != nil {
		t.Fatal(err)
	}
	d.Backdate("agk-01JMZ8V1P9C4_invoice_1", time.Hour)

	h := newHost(t, d, secretsTmpfs)
	h.serving(t)
	if list, err := cli.NetworkList(t.Context(), nil); err != nil || len(list) != 0 {
		t.Errorf("the networks after the start are %v, %v", list, err)
	}
	if !strings.Contains(h.err.String(), "an earlier process left task networks on this daemon that no container is on, and they were removed: agk-01JMZ8V1P9C4_invoice_1") {
		t.Errorf("the agent's log does not say what it swept:\n%s", h.err)
	}
}

// "Give the agent CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE and nothing else." An agent that
// finds a remapped daemon and lacks one refuses its start before the API hears from it, and
// says which lines of its unit grant them.
func TestServeRefusesARemappedDaemonWithoutTheThreeCapabilities(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.e.Host = installed{caps: 1<<0 | 1<<1, fs: h.e.Host.(installed).fs}
	said := h.refused(t)
	for _, want := range []string{"lacks CAP_FOWNER", "AmbientCapabilities=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE", "CapabilityBoundingSet=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
}

// "A server runner requires its secrets directory to be a tmpfs mounted noexec,nosuid,nodev."
// One that is a tmpfs without noexec, as /dev/shm is on most distributions, refuses the start
// and names the file it is set in.
func TestServeRefusesASecretsDirectoryThatIsNotANoexecTmpfs(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.e.Host = installed{caps: ownership, fs: driver.Filesystem{Tmpfs: true, NoSUID: true, NoDev: true}}
	said := h.refused(t)
	for _, want := range []string{"/run/agentiik/secrets is a tmpfs mounted without noexec.", h.e.PolicyFile} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
}

func TestAStopWhileTheDaemonHangsEndsTheStartWithoutSayingReady(t *testing.T) {
	d := daemon(t, true)
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	d.Handle("GET", "/info", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hung:
		case <-r.Context().Done():
		}
	})
	h := newHost(t, d, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, h.e, []string{"serve"}) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != exitSucceeded {
			t.Errorf("serve stopped while it started exited %d:\n%s", code, h.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve was stopped while the daemon hung and is still starting:\n%s", h.err)
	}
	if said := h.heard(50 * time.Millisecond); said != "" {
		t.Errorf("systemd was told %q by a start that was stopped", said)
	}
}

func TestAStopBeforeTheStartIsNotFollowedByReady(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx, h.e, []string{"serve"}); code != exitSucceeded {
		t.Errorf("serve stopped before it started exited %d:\n%s", code, h.err)
	}
	if said := h.heard(50 * time.Millisecond); said != "" {
		t.Errorf("systemd was told %q by a start that was stopped", said)
	}
}

func TestAStartThatCannotTellSystemdItIsReadyFails(t *testing.T) {
	h := newHost(t, daemon(t, true), secretsTmpfs)
	h.set(runner.NotifySocket, filepath.Join(t.TempDir(), "nobody"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if code := run(ctx, h.e, []string{"serve"}); code != exitRefused {
		t.Errorf("serve that could not say it was ready exited %d:\n%s", code, h.err)
	}
	if !strings.Contains(h.err.String(), runner.Ready) {
		t.Errorf("the refusal does not say what it could not tell:\n%s", h.err)
	}
}

func TestServeRefusesToRunAsRoot(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	h.e.Geteuid = func() int { return 0 }
	if said := h.refused(t); !strings.Contains(said, "root") {
		t.Errorf("the refusal does not say why:\n%s", said)
	}
}

func TestARefusedStartNamesEverythingWrongWithIt(t *testing.T) {
	h := newHost(t, daemon(t, true), "require_userns_remap = maybe\n")
	if err := os.Chmod(h.e.EnvFile, 0o640); err != nil {
		t.Fatal(err)
	}
	said := h.refused(t)
	for _, want := range []string{h.e.EnvFile + " has mode 0640", h.e.PolicyFile} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, credential[len("agkrunner_"):]) {
		t.Errorf("the refusal carries the credential:\n%s", said)
	}
}

func TestAWorkRootTheAgentCannotCreateRefusesTheStart(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.set("AGK_RUNNER_WORKDIR", filepath.Join(blocked, "work"))
	if said := h.refused(t); !strings.Contains(said, runner.WorkDir) {
		t.Errorf("the refusal does not name %s:\n%s", runner.WorkDir, said)
	}
}

func TestTheVerbsAreTheDocumentedThree(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if code := run(context.Background(), h.e, []string{"help"}); code != exitSucceeded {
		t.Errorf("help exited %d", code)
	}
	for _, want := range []string{"agk-runner join --api <url> --token <token>", "agk-runner serve", "agk-runner version"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("help does not carry %q:\n%s", want, h.out)
		}
	}
	for _, args := range [][]string{nil, {"run"}, {"version", "--short"}} {
		if code := run(context.Background(), h.e, args); code != exitUsage {
			t.Errorf("agk-runner %q exited %d, want %d", args, code, exitUsage)
		}
	}
	if code := run(context.Background(), h.e, []string{"join", "--api", "https://agentiik.example.com", "--token", "agkjoin_x"}); code != exitRefused {
		t.Errorf("join exited %d, and it is refused until it is built", code)
	}

	h.out.b.Reset()
	if code := run(context.Background(), h.e, []string{"version"}); code != exitSucceeded || h.out.String() != runner.Version()+"\n" {
		t.Errorf("version exited %d printing %q", code, h.out)
	}
}
