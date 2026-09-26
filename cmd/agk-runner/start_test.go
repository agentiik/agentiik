package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
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

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/runner"
	"github.com/nats-io/nkeys"
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
// a service manager listening, and an API that counts what reaches it. The API answers a heartbeat
// with beat, 200 where it is zero, a bus credential with busToken where it is set, and nothing else
// it is asked.
type host struct {
	e        env
	out, err *output
	requests atomic.Int32
	beat     atomic.Int32
	// revoked has the heartbeat's answer say the runner is revoked, its grace ending in an hour.
	revoked atomic.Bool
	// busToken answers POST /api/v1/bus/token. It is set before serve runs.
	busToken http.HandlerFunc
	// bearers is every credential a heartbeat carried.
	bearers sync.Map
	notify  *net.UnixConn
	// api is the API's address.
	api string
	// joins counts the joins that reached the API. The first unanswered of them are answered
	// 503, as an API not up yet is answered by what is in front of it, and the rest joinStatus
	// where it is set, or as a good token is.
	joins, unanswered, joinStatus atomic.Int32
	// claimed is the body of the last join the API took.
	claimed atomic.Value
}

// newHost lays a host out around a daemon. policy is the text of runner.toml, and "" is no file.
func newHost(t *testing.T, daemon *dockertest.Daemon, policy string) *host {
	t.Helper()
	h := &host{out: &output{}, err: &output{}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if r.URL.Path == "/api/v1/runners" && r.Method == http.MethodPost {
			h.joins.Add(1)
			body, _ := io.ReadAll(r.Body)
			switch {
			case h.unanswered.Add(-1) >= 0:
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case h.joinStatus.Load() != 0:
				w.WriteHeader(int(h.joinStatus.Load()))
				return
			}
			h.claimed.Store(string(body))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"runner": "runner-dmz-03", "pool": "dmz", "credential": %q, "rotate_by": %q}`, credential, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.URL.Path == "/api/v1/bus/token" && h.busToken != nil {
			h.busToken(w, r)
			return
		}
		if r.URL.Path != "/api/v1/runners/heartbeat" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		h.bearers.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), true)
		if status := int(h.beat.Load()); status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		now := time.Now().UTC()
		if h.revoked.Load() {
			fmt.Fprintf(w, `{"received_at":%q,"drain":true,"reason":"host decommissioned","results_accepted_until":%q,"cancel":[]}`,
				now.Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano))
			return
		}
		fmt.Fprintf(w, `{"received_at":%q,"drain":false,"cancel":[]}`, now.Format(time.RFC3339Nano))
	}))
	t.Cleanup(api.Close)
	h.api = api.URL

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
	// The key join wrote beside it, which serve reads before the daemon.
	keyFile := filepath.Join(dir, "runner.key")
	if err := os.WriteFile(keyFile, aHostKey(t), 0o600); err != nil {
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
		KeyFile:    keyFile,
		MemInfo:    filepath.Join("..", "..", "runner", "testdata", "meminfo"),
		// Where serve keeps a renewed credential, and nothing there.
		CredentialFile: filepath.Join(dir, "credential"),
		// Where the image installs the helper, and nothing there until a test puts one.
		HelperFile: filepath.Join(dir, "agk-helper"),
		Host:       installed{caps: ownership},
	}
	return h
}

// aHostKey is a private key as join writes one.
func aHostKey(t *testing.T) []byte {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// installed is the machine an agent installed as the page says finds, whoever runs the test:
// the three capabilities its unit grants, and every directory, the work root among them, on a
// plain disk. A test takes one away to see the start refused.
type installed struct {
	caps uint64
	disk driver.Filesystem
}

func (m installed) Capabilities() (uint64, error) { return m.caps, nil }
func (m installed) Filesystem(string) (driver.Filesystem, error) {
	return m.disk, nil
}

// ownership is CAP_CHOWN, CAP_DAC_OVERRIDE and CAP_FOWNER, bits 0, 1 and 3 of the kernel's sets.
const ownership = 1<<0 | 1<<1 | 1<<3

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
	h := newHost(t, daemon(t, false), "require_userns_remap = false\n")
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

// "A 401 stops the agent": a credential the API refuses at the first heartbeat ends the start
// before systemd is told anything, saying to join again, with the status the unit's
// RestartPreventExitStatus= names, so that systemd does not start it again.
func TestACredentialRefusedAtTheHeartbeatEndsTheStartSayingToJoinAgain(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	h.beat.Store(http.StatusUnauthorized)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if code := run(ctx, h.e, []string{"serve"}); code != exitJoinAgain {
		t.Errorf("serve whose credential was refused exited %d, want %d:\n%s", code, exitJoinAgain, h.err)
	}
	if n := h.requests.Load(); n != 1 {
		t.Errorf("%d requests reached the API, want the one heartbeat and nothing asked again", n)
	}
	if said := h.heard(50 * time.Millisecond); said != "" {
		t.Errorf("systemd was told %q by a start whose credential was refused", said)
	}
	if !strings.Contains(h.err.String(), "this runner's credential was refused: join it again with agk-runner join --replace") {
		t.Errorf("the refusal does not say to join again:\n%s", h.err)
	}
}

// A revoked runner holding nothing has nothing left to do in its grace: it ends with the status
// the unit's RestartPreventExitStatus= names, so that Restart=always does not bring back a runner
// that would only take nothing until its grace ends and every call is refused.
func TestARevokedRunnerHoldingNothingExitsWithTheStatusSystemdDoesNotRestart(t *testing.T) {
	url := os.Getenv("AGENTIIK_TEST_BUS_URL")
	if url == "" {
		t.Skip("no NATS on this machine: set AGENTIIK_TEST_BUS_URL")
	}
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := account.Seed()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := bus.NewIssuer(string(seed), url)
	if err != nil {
		t.Fatal(err)
	}

	h := newHost(t, daemon(t, true), "")
	h.revoked.Store(true)
	h.busToken = func(w http.ResponseWriter, r *http.Request) {
		// What the API mints a revoked runner: results and stops, and no pull.
		c, err := issuer.ForRevokedRunner("runner-dmz-02", time.Now().Add(time.Hour))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"kind": c.Kind, "url": c.URL, "jwt": c.JWT, "seed": c.Seed,
			"stream": bus.Stream, "consumer": bus.Durable("dmz"), "expires_at": c.ExpiresAt.Format(time.RFC3339Nano),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if code := run(ctx, h.e, []string{"serve"}); code != exitJoinAgain {
		t.Errorf("a revoked runner holding nothing exited %d, want %d:\n%s", code, exitJoinAgain, h.err)
	}
	if ctx.Err() != nil {
		t.Error("serve ended only as the test gave up on it")
	}
	if !strings.Contains(h.err.String(), "this runner is revoked, and has answered for every task it held") {
		t.Errorf("the agent does not say why it ended:\n%s", h.err)
	}
}

// "A host whose key is gone is a new runner": it can never renew its credential, so it is told to
// join again before it asks the API anything, with the status that keeps systemd from starting it
// again.
func TestAHostWhoseKeyIsGoneIsToldToJoinAgainBeforeAnyRequest(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if err := os.Remove(h.e.KeyFile); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if code := run(ctx, h.e, []string{"serve"}); code != exitJoinAgain {
		t.Errorf("serve whose key is gone exited %d, want %d:\n%s", code, exitJoinAgain, h.err)
	}
	if n := h.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the API", n)
	}
	for _, want := range []string{h.e.KeyFile, "a host whose key is gone is a new runner", "agk-runner join --replace"} {
		if !strings.Contains(h.err.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, h.err)
		}
	}
}

// The credential the agent renewed to is the one it carries, since runner.env, read-only to it,
// still holds the one join wrote, which the API stopped taking once the renewed one was used. One
// left by another runner refuses the start.
func TestServeCarriesTheCredentialItRenewedTo(t *testing.T) {
	const renewed = "agkrunner_Rn3wEdQkZ3v0bq8LrT2mN5pW7sD1fG4hJ6kA9cE0uI3o"
	at := time.Now().UTC()
	write := func(h *host, runnerID string) {
		text := fmt.Sprintf(`{"runner":%q,"credential":%q,"rotated_at":%q,"rotate_by":%q}`+"\n",
			runnerID, renewed, at.Format(time.RFC3339Nano), at.Add(720*time.Hour).Format(time.RFC3339Nano))
		if err := os.WriteFile(h.e.CredentialFile, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := newHost(t, daemon(t, true), "")
	write(h, "runner-dmz-02")
	h.serving(t)
	var carried []string
	h.bearers.Range(func(k, _ any) bool { carried = append(carried, k.(string)); return true })
	if len(carried) != 1 || carried[0] != renewed {
		t.Errorf("the heartbeats carried %d credentials, want the renewed one alone", len(carried))
	}

	h = newHost(t, daemon(t, true), "")
	write(h, "runner-lan-01")
	if said := h.refused(t); !strings.Contains(said, h.e.CredentialFile) || strings.Contains(said, renewed) {
		t.Errorf("a renewed credential of another runner was refused saying:\n%s", said)
	}
}

func TestServeSaysReadyOnceItsFirstHeartbeatIsAnswered(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	h.serving(t)
	for _, want := range []string{"serving as runner-dmz-02 in pool dmz", "2 tasks at once", "declaring memory 16318196Ki and cpu ", "the daemon speaking API"} {
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

	h := newHost(t, d, "")
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
	h := newHost(t, daemon(t, true), "")
	h.e.Host = installed{caps: 1<<0 | 1<<1}
	said := h.refused(t)
	for _, want := range []string{"lacks CAP_FOWNER", "AmbientCapabilities=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE", "CapabilityBoundingSet=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE"} {
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
	h := newHost(t, daemon(t, true), "")
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

// A runner that cannot say how much it has cannot put back what it has no room for, and join refuses
// the same host for the same reason.
func TestAHostWhoseMemoryCannotBeMeasuredRefusesTheStart(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	h.e.MemInfo = filepath.Join(t.TempDir(), "meminfo")
	if said := h.refused(t); !strings.Contains(said, h.e.MemInfo) {
		t.Errorf("the refusal does not name %s:\n%s", h.e.MemInfo, said)
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
	// Reset, since the usage printed above names --token too.
	h.err.b.Reset()
	if code := run(context.Background(), h.e, []string{"join", "--api", "https://agentiik.example.com", "--token", "agkjoin_x", "--labels", "zone=dmz"}); code != exitRefused || !strings.Contains(h.err.String(), "--token") {
		t.Errorf("join exited %d given a token that is not one, and it refuses it naming --token:\n%s", code, h.err)
	}

	h.out.b.Reset()
	if code := run(context.Background(), h.e, []string{"version"}); code != exitSucceeded || h.out.String() != runner.Version()+"\n" {
		t.Errorf("version exited %d printing %q", code, h.out)
	}
}

// opened is the configuration serve opens the driver with, once it has.
func opened(t *testing.T) func() driver.Config {
	t.Helper()
	var (
		mu  sync.Mutex
		got driver.Config
	)
	t.Cleanup(func() { newDriver = driver.New })
	newDriver = func(cfg driver.Config) (*driver.Docker, error) {
		mu.Lock()
		got = cfg
		mu.Unlock()
		return driver.New(cfg)
	}
	return func() driver.Config {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// "/agk/bin/agk is mounted read-only" in every script step, so a runner binds the helper installed
// beside it without a line of runner.toml, from a copy under the work root: in the container form
// the installed path is inside the agent's image, where the daemon, which resolves a bind's source
// on the host, would not find it.
func TestTheHelperInstalledBesideTheAgentIsBoundFromUnderTheWorkRoot(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if err := os.WriteFile(h.e.HelperFile, []byte("\x7fELF the helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := opened(t)
	h.serving(t)

	work, _ := h.e.Lookup("AGK_RUNNER_WORKDIR")
	want := filepath.Join(work, runner.HelperDir, "agk")
	if got := cfg().Policy.Helper; got != want {
		t.Errorf("the driver was opened with helper %q, want the copy %s", got, want)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "\x7fELF the helper" {
		t.Errorf("the copy holds %q, %v", b, err)
	}
	if !strings.Contains(h.err.String(), "the static helper "+h.e.HelperFile+" is bound read-only at /agk/bin/agk") {
		t.Errorf("the agent's log does not say which helper it binds:\n%s", h.err)
	}
}

// A helper runner.toml names is the operator's choice, and taken as written.
func TestAHelperRunnerTomlNamesIsTakenAsWritten(t *testing.T) {
	h := newHost(t, daemon(t, true), "helper = \"/opt/agk/agk-linux-amd64\"\n")
	if err := os.WriteFile(h.e.HelperFile, []byte("\x7fELF the helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := opened(t)
	h.serving(t)

	if got := cfg().Policy.Helper; got != "/opt/agk/agk-linux-amd64" {
		t.Errorf("the driver was opened with helper %q, want the one runner.toml names", got)
	}
	work, _ := h.e.Lookup("AGK_RUNNER_WORKDIR")
	if _, err := os.Stat(filepath.Join(work, runner.HelperDir)); !os.IsNotExist(err) {
		t.Errorf("the installed helper was laid down although runner.toml names another: %v", err)
	}
}

// The driver writes every task's log into the shipment its carrier opens for it, which is what
// ships the log to the API while the container runs.
func TestTheDriverWritesEveryTasksLogWhereItIsShipped(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	cfg := opened(t)
	h.serving(t)
	if got, ok := cfg().Logs.(runner.TaskLogs); !ok {
		t.Errorf("the driver was opened with the logs %#v, and not the runner's TaskLogs", got)
	}
}

// No helper installed and none named is a runner that binds none, which the page allows: the
// helper "is a convenience, never a requirement".
func TestNoHelperInstalledStartsAndSaysScriptsHaveNone(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	cfg := opened(t)
	h.serving(t)
	if got := cfg().Policy.Helper; got != "" {
		t.Errorf("the driver was opened with helper %q, and none is installed", got)
	}
	if !strings.Contains(h.err.String(), "there is no "+h.e.HelperFile+" and runner.toml names no helper") {
		t.Errorf("the agent's log does not say script steps have no helper:\n%s", h.err)
	}
}

func TestADirectoryWhereTheHelperIsInstalledRefusesTheStart(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if err := os.Mkdir(h.e.HelperFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if said := h.refused(t); !strings.Contains(said, h.e.HelperFile+" is where the static helper is installed") {
		t.Errorf("the refusal does not name %s:\n%s", h.e.HelperFile, said)
	}
}

// A copy bound from a work root mounted noexec is bound noexec, which a script can read and not run.
func TestAWorkRootMountedNoexecBindsNoHelper(t *testing.T) {
	h := newHost(t, daemon(t, true), "")
	if err := os.WriteFile(h.e.HelperFile, []byte("\x7fELF the helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := h.e.Host.(installed)
	m.disk = driver.Filesystem{NoExec: true, NoDev: true}
	h.e.Host = m
	cfg := opened(t)
	h.serving(t)
	if got := cfg().Policy.Helper; got != "" {
		t.Errorf("the driver was opened with helper %q from a work root mounted noexec", got)
	}
	if !strings.Contains(h.err.String(), "is on a filesystem mounted noexec") {
		t.Errorf("the agent's log does not say why script steps have no helper:\n%s", h.err)
	}
}

// A daemon anywhere but on a local socket refuses the start, naming DOCKER_HOST and not
// repeating the address, before anything reaches the API.
func TestADaemonAcrossTheNetworkRefusesTheStartNamingDockerHost(t *testing.T) {
	for _, address := range []string{"tcp://docker.example.com:2375", "tcp://admin:s3cr3t@10.0.0.7:2376", "ssh://admin@docker.example.com"} {
		h := newHost(t, daemon(t, true), "")
		h.set("DOCKER_HOST", address)
		said := h.refused(t)
		if !strings.Contains(said, "DOCKER_HOST names a daemon at") || !strings.Contains(said, "local unix socket alone") {
			t.Errorf("a daemon at %s refused the start saying:\n%s", address, said)
		}
		if strings.Contains(said, "s3cr3t") || strings.Contains(said, "example.com") {
			t.Errorf("the refusal repeats the address:\n%s", said)
		}
	}
}
