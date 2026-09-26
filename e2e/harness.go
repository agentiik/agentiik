package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/version"
	"github.com/jackc/pgx/v5"
)

// Variable is what stands an installation up rather than skipping: set to 1, Stand builds one,
// and anywhere else every test that asks for one skips. A skip rather than a failure, because
// the installation starts privileged containers, which a laptop's daemon is not always given
// and a developer running go test ./... did not ask for.
const Variable = "AGENTIIK_E2E"

// The images the installation is made of besides what it builds. PostgreSQL and NATS are the
// images the Compose file names, and alpine:3.21 the one the other tests pull.
// docker:29-dind is the major the rest of the project runs, the first whose bridge keeps the
// host out of an internal network being 28.
const (
	postgresImage = "postgres:17-alpine"
	natsImage     = "nats:2-alpine"
	registryImage = "registry:2"
	dindImage     = "docker:29-dind"
	alpineImage   = "alpine:3.21"
)

// The installation's names: the one namespace, the one pool both runners join, and the label
// they claim, which a step selects them by with runs_on. A pool of the test's own rather than
// default, so that a step naming no runs_on reaches neither of them, and a test that joins a
// runner to default, claiming no label, knows which runner such a step ran on.
const (
	Namespace = "e2e"
	Pool      = "e2e"
	Label     = "zone=e2e"
)

// Installation is one test installation: PostgreSQL, the bus, the API and the controller, a
// registry, and two runners each on a Docker daemon of its own. doc.go draws it.
type Installation struct {
	// PublicURL is where the runners and the operator reach the API: the TLS terminator in
	// front of it, which records every request it passes on.
	PublicURL string

	// Registry is the one address every runner's daemon pulls from: the registry's name on
	// the installation's network, and its port.
	Registry string

	// Runners are the two runners, joined and heartbeating.
	Runners []*Runner

	// Requests records every request the terminator passed to the API.
	Requests *Requests

	t       testing.TB
	id      string
	root    string
	module  string
	bin     string
	token   string
	client  *http.Client
	network string

	// apiIm, controllerIm and runnerIm are the images built from this checkout.
	apiIm, controllerIm, runnerIm string

	// volumes are init's, by the service each is for, as the Compose file names them.
	volumes map[string]string

	// private are the volumes and the directories of this machine that are the installation's,
	// which no runner may have mounted.
	private []string

	// superuser is the database as its superuser reaches it, which Database connects to.
	superuser string

	// helper is the static helper built for the architecture the daemons run containers as,
	// which the runner image carries and agk run --local is given.
	helper string

	// ctx bounds everything the installation asks of a program or a daemon: done a margin
	// before go test's own timeout, so that a call that hangs fails the test while there is
	// still time to print the logs and take the installation down, which a test killed by the
	// timeout never does.
	ctx    context.Context
	cancel context.CancelFunc

	// held are the values no runner may hold, by what each is: the installation's own
	// credentials and keys, which only the API and the controller are given.
	held []heldValue

	// teardown is undone last first, after the logs of a failed test have been read.
	teardown []func()

	// logs are what a failed test prints, in the order they were started.
	logs []logSource
}

// heldValue is one value that must be nowhere in a runner's reach, and what it is.
type heldValue struct {
	what  string
	value string
}

// logSource is one component's log, read when a test fails.
type logSource struct {
	name string
	read func() string
}

// Stand builds an installation for t, or skips t where Variable is not 1. Everything it starts
// is taken down when t ends, and a failed t first prints the log of every component.
func Stand(t testing.TB) *Installation {
	t.Helper()
	if os.Getenv(Variable) != "1" {
		t.Skipf("the test installation is stood up only where %s=1, since it starts privileged containers: see the package documentation", Variable)
	}
	// A failure rather than a skip once it was asked for: the daemons resolve every bind
	// against the host, and a daemon in a virtual machine, which is what Docker is on macOS
	// and Windows, resolves them against the machine's disk and not this one's.
	if runtime.GOOS != "linux" {
		t.Fatalf("%s=1 asks for the test installation, which runs on Linux alone: each runner's work root is a named volume its Docker daemon and its agent share, and the agents, the API and the bus join the host's network", Variable)
	}

	in := &Installation{t: t, id: "agk-e2e-" + randomHex(4)}
	in.ctx, in.cancel = boundedBy(t)
	t.Cleanup(in.takeDown)

	var err error
	if in.root, err = os.MkdirTemp("", in.id+"-"); err != nil {
		t.Fatal(err)
	}
	in.undo(func() { in.removeRoot() })
	if in.module, err = moduleRoot(); err != nil {
		t.Fatal(err)
	}

	ctx := in.ctx
	in.token = "agk_op_" + randomHex(24)
	in.held = append(in.held, heldValue{"the operator token", in.token})
	ca := in.certificates()
	in.client = &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: ca.clientConfig()}}
	in.build(ctx)
	in.images(ctx)
	socket := in.database(ctx)
	in.initialize(ctx, socket)
	busURL := in.bus(ctx)
	in.serve(ctx, ca, socket, busURL)
	in.registry(ctx)

	in.pool()
	for _, name := range []string{"a", "b"} {
		in.Runners = append(in.Runners, in.runner(ctx, name, joinWith(in.issue(Pool, Label)), Label))
	}
	in.ready()
	return in
}

// undo registers what takes one component down again.
func (in *Installation) undo(f func()) { in.teardown = append(in.teardown, f) }

// logged registers one component's log for a failed test to print.
func (in *Installation) logged(name string, read func() string) {
	in.logs = append(in.logs, logSource{name, read})
}

// takeDown prints every log where the test failed, then takes every component down, last first.
// It is one cleanup rather than one per component, so that the logs are read while the
// containers that hold them are still there.
func (in *Installation) takeDown() {
	if in.t.Failed() {
		for _, l := range in.logs {
			in.t.Logf("---- %s ----\n%s", l.name, tail(l.read(), 64<<10))
		}
	}
	for i := len(in.teardown) - 1; i >= 0; i-- {
		in.teardown[i]()
	}
	in.cancel()
}

// teardownMargin is how long before go test's timeout the installation stops waiting on anything,
// which is the time taking it down has.
const teardownMargin = 3 * time.Minute

// boundedBy is t's context, done teardownMargin before t's deadline where it has one.
func boundedBy(t testing.TB) (context.Context, context.CancelFunc) {
	if d, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if deadline, set := d.Deadline(); set {
			return context.WithDeadline(t.Context(), deadline.Add(-teardownMargin))
		}
	}
	return context.WithCancel(t.Context())
}

// tail is the last max bytes of s, since a log that ran to megabytes is read from its end.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "[...]" + s[len(s)-max:]
}

// removeRoot removes the directory everything was written under. Some of it belongs to other
// accounts by then, the socket PostgreSQL left and what a runner gave to the agent's account, so
// it is emptied from a container, as root, before it is removed.
func (in *Installation) removeRoot() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := docker(ctx, "run", "--rm", "--network", "none", "--userns", "host", "-v", in.root+":/root-of-the-test", alpineImage,
		"sh", "-c", "rm -rf /root-of-the-test/* /root-of-the-test/.[!.]*"); err != nil {
		in.t.Logf("the test's directory %s could not be emptied: %s", in.root, err)
	}
	if err := os.RemoveAll(in.root); err != nil {
		in.t.Logf("the test's directory %s could not be removed: %s", in.root, err)
	}
}

// path is a path under the test's directory.
func (in *Installation) path(parts ...string) string {
	return filepath.Join(append([]string{in.root}, parts...)...)
}

// mkdir makes a directory under the test's directory.
func (in *Installation) mkdir(mode os.FileMode, parts ...string) string {
	dir := in.path(parts...)
	if err := os.MkdirAll(dir, mode); err != nil {
		in.t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		in.t.Fatal(err)
	}
	return dir
}

// build compiles the programs from this checkout. agk runs on this machine; the API, the
// controller, the agent and the helper run in the images, for the architecture the daemon runs
// containers as, which is also the helper agk run --local mounts on this machine's daemon.
func (in *Installation) build(ctx context.Context) {
	in.bin = in.mkdir(0o755, "bin")
	arch, err := docker(ctx, "version", "--format", "{{.Server.Arch}}")
	if err != nil {
		in.t.Fatal(err)
	}
	// The images' binaries in a directory of their own, which is the images' build context,
	// named as build/*.Dockerfile name them.
	image := in.mkdir(0o755, "bin", "image")
	for _, b := range []struct{ cmd, out, goos, goarch string }{
		{"agk", filepath.Join(in.bin, "agk"), runtime.GOOS, runtime.GOARCH},
		{"agentiik-api", filepath.Join(image, "agentiik-api-linux-"+arch), "linux", arch},
		{"agentiik-controller", filepath.Join(image, "agentiik-controller-linux-"+arch), "linux", arch},
		{"agk-runner", filepath.Join(image, "agk-runner-linux-"+arch), "linux", arch},
		{"agk-helper", filepath.Join(image, "agk-helper-linux-"+arch), "linux", arch},
	} {
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", b.out, "./cmd/"+b.cmd)
		cmd.Dir = in.module
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+b.goos, "GOARCH="+b.goarch)
		if out, err := cmd.CombinedOutput(); err != nil {
			in.t.Fatalf("building %s: %s\n%s", b.cmd, err, out)
		}
	}
	in.helper = filepath.Join(image, "agk-helper-linux-"+arch)
}

// database starts PostgreSQL, as the Compose file does, and answers the directory its socket is
// in, which init, the API and the controller mount at /run/postgresql.
//
// It is reached over its socket alone, in a directory of the test's own, so that this machine reads
// it too, as its superuser, for what no route answers. A local socket is the one path
// internal/config lets a database be reached on without TLS, since it crosses no network.
func (in *Installation) database(ctx context.Context) string {
	socket := in.mkdir(0o755, "postgres")
	name := in.id + "-postgres"
	superuser := randomHex(16)
	in.container(ctx, name, "run", "-d", "--name", name, "--label", in.label(), "--network", "none",
		"-e", "POSTGRES_PASSWORD="+superuser, "-e", "POSTGRES_DB=agentiik",
		"-v", socket+":/var/run/postgresql", postgresImage)

	// The image starts a server of its own to create the database and stops it again, both on
	// the same socket, so the socket answering says nothing until the image says it is done.
	eventually(in.ctx, in.t, 2*time.Minute, "PostgreSQL finished creating its database", func() error {
		logs, err := dockerCombined(ctx, "logs", name)
		if err != nil {
			return fmt.Errorf("%w: %s", err, logs)
		}
		if !strings.Contains(logs, "PostgreSQL init process complete") {
			return errors.New("it is still creating it")
		}
		return nil
	})
	in.superuser = "postgres://postgres@/agentiik?host=" + socket
	in.held = append(in.held,
		heldValue{"the database superuser's password", superuser},
		heldValue{"the database URL", applicationURL},
	)
	eventually(in.ctx, in.t, time.Minute, "PostgreSQL answered on its socket", func() error {
		conn, err := pgx.Connect(ctx, in.superuser)
		if err != nil {
			return err
		}
		defer conn.Close(context.WithoutCancel(ctx))
		return conn.Ping(ctx)
	})
	return socket
}

// The databases as the programs in the images reach them, on the socket mounted at /run/postgresql:
// the superuser init migrates as, and the role the API and the controller connect as, which it
// creates, with a password it generates.
const (
	migrateURL     = "postgres://postgres@/agentiik?host=/run/postgresql"
	applicationURL = "postgres://agentiik@/agentiik?host=/run/postgresql"
)

// initHost is AGK_INIT_HOST: the address the runners reach the bus at, on the host's network,
// which the certificate init makes is issued to.
const initHost = "127.0.0.1"

// The volumes init prepares, one per service, each named as the Compose file names it and mounted
// where the Compose file mounts it: in init under /init, and in its service where each says.
var initVolumes = []string{"api", "controller", "bus", "nats", "runner", "objects"}

// initialize runs agentiik-api init from the API's image, as root, as the Compose file's init
// service runs it, on a volume per service: it makes the certificate, the keys, the database
// password, the bus identity and the bus's configuration, migrates, creates the namespace, writes
// the operator token's hash and a join token of the pool default. Everything it writes that no
// runner may hold is read back, as root, to be looked for in what the runners hold.
func (in *Installation) initialize(ctx context.Context, socket string) {
	in.volumes = map[string]string{}
	args := []string{"run", "--name", in.id + "-init", "--label", in.label(),
		"--user", "0:0", "--network", "none", "--userns", "host",
		"-e", config.InitDir + "=/init",
		"-e", config.InitHost + "=" + initHost,
		"-e", config.InitNamespace + "=" + Namespace,
		"-e", config.OperatorToken + "=" + in.token,
		"-e", config.MigrateDatabaseURL + "=" + migrateURL,
		"-e", config.DatabaseURL + "=" + applicationURL,
		"-v", socket + ":/run/postgresql",
	}
	for _, v := range initVolumes {
		in.volumes[v] = in.volume(ctx, v)
		args = append(args, "-v", in.volumes[v]+":/init/"+v)
	}
	// The API, the controller, the control plane's credential, the bus and the objects are the
	// installation's, which no runner sees; the runner's volume is the runner's.
	in.private = []string{in.volumes["api"], in.volumes["controller"], in.volumes["bus"], in.volumes["nats"], in.volumes["objects"], socket}

	name := in.id + "-init"
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "rm", "-f", "-v", name)
	})
	out, err := dockerCombined(ctx, append(args, in.apiIm, "init")...)
	in.logged(name, func() string { return out })
	if err != nil {
		in.t.Fatalf("agentiik-api init: %s\n%s", err, out)
	}
	if strings.Contains(out, in.token) {
		in.t.Fatalf("agentiik-api init printed the operator token it was given:\n%s", out)
	}

	for _, f := range []struct{ what, path string }{
		{"the presign key", "api/presign-key"},
		{"the master key", "api/master-key"},
		{"the application role's password", "api/database-password"},
		{"the control plane's bus credential", "bus/control-plane.creds"},
		{"the bus account seed", "api/bus/account.seed"},
		{"the private key of the bus's certificate", "api/tls/server.key"},
		{"the operator token's hash", "api/operator-token.sha256"},
	} {
		in.held = append(in.held, heldValue{f.what, in.read(ctx, f.path)})
	}
	// The master key file's key line alone, which is what would be copied out of it.
	for _, line := range strings.Split(in.read(ctx, "api/master-key"), "\n") {
		if key, ok := strings.CutPrefix(line, "key: "); ok {
			in.held = append(in.held, heldValue{"the master key", key})
		}
	}
}

// read is what the file at path under init's volumes holds, trimmed, read as root from a container
// that mounts them, since each is its service's account's alone.
func (in *Installation) read(ctx context.Context, path string) string {
	volume, rest, _ := strings.Cut(path, "/")
	out, err := docker(ctx, "run", "--rm", "--network", "none", "--userns", "host",
		"-v", in.volumes[volume]+":/v:ro", alpineImage, "cat", "/v/"+rest)
	if err != nil {
		in.t.Fatalf("%s could not be read from init's volume: %s", path, err)
	}
	return out
}

// busPort is where the bus listens on every interface, as the nats.conf init writes has it, and
// busHealth where its health check answers, on the loopback.
const (
	busPort   = "4222"
	busHealth = "http://127.0.0.1:8222/healthz"
)

// bus starts the NATS server on the configuration init wrote, as the Compose file does, on the
// host's network, where the API, the controller and every agent reach it at initHost. It answers
// the bus's URL.
func (in *Installation) bus(ctx context.Context) string {
	name := in.id + "-nats"
	in.container(ctx, name, "run", "-d", "--name", name, "--label", in.label(), "--network", "host", "--userns", "host",
		"-v", in.volumes["nats"]+":/nats", natsImage, "-c", "/nats/nats.conf")
	eventually(in.ctx, in.t, time.Minute, "the bus's health check answered", func() error {
		answer, err := http.Get(busHealth)
		if err != nil {
			return err
		}
		answer.Body.Close()
		if answer.StatusCode != http.StatusOK {
			return fmt.Errorf("it answered %d", answer.StatusCode)
		}
		return nil
	})
	return "tls://" + net.JoinHostPort(initHost, busPort)
}

// serve starts the API and the controller from their images, on what init prepared, as the Compose
// file does, and the terminator in front of the API. The API is behind that proxy, AGK_PROXY_URL,
// so it serves plain HTTP on the loopback, and both share the objects volume, which no runner sees,
// and the bus volume, holding the control plane's credential, which the API renews and the
// controller only reads.
func (in *Installation) serve(ctx context.Context, ca authority, socket, busURL string) {
	port := strconv.Itoa(freePort(in.t))
	in.Requests = &Requests{operator: in.token}
	in.PublicURL = in.terminate(ca, "http://127.0.0.1:"+port)

	api := in.id + "-api"
	in.container(ctx, api, "run", "-d", "--name", api, "--label", in.label(), "--network", "host", "--userns", "host",
		"-e", config.DatabaseURL+"="+applicationURL,
		"-e", config.DatabasePasswordFile+"=/agentiik/database-password",
		"-e", config.BusURL+"="+busURL,
		"-e", config.BusCredentialsFile+"=/bus/control-plane.creds",
		"-e", config.BusAccountSeedFile+"=/agentiik/bus/account.seed",
		"-e", config.ObjectsDir+"=/objects",
		"-e", config.PresignKeyFile+"=/agentiik/presign-key",
		"-e", config.MasterKeyFile+"=/agentiik/master-key",
		"-e", config.OperatorTokenFile+"=/agentiik/operator-token.sha256",
		"-e", config.Listen+"=:"+port,
		"-e", config.ProxyURL+"="+in.PublicURL,
		"-e", "SSL_CERT_DIR=/agentiik/trust",
		"-v", in.volumes["api"]+":/agentiik", "-v", in.volumes["bus"]+":/bus", "-v", in.volumes["objects"]+":/objects", "-v", socket+":/run/postgresql",
		in.apiIm)
	// The Compose file's health check, run as it runs it, in the API's own container.
	eventually(in.ctx, in.t, time.Minute, "agentiik-api health said the API is ready", func() error {
		out, err := dockerCombined(ctx, "exec", api, "/agentiik-api", "health")
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
	eventually(in.ctx, in.t, time.Minute, "the API answered the operator", func() error {
		code, body, err := in.call(ctx, "GET", "/api/v1/runner-pools", nil)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("it answered %d: %s", code, body)
		}
		return nil
	})

	controller := in.id + "-controller"
	in.container(ctx, controller, "run", "-d", "--name", controller, "--label", in.label(), "--network", "host", "--userns", "host",
		"-e", config.DatabaseURL+"="+applicationURL,
		"-e", config.DatabasePasswordFile+"=/agentiik/database-password",
		"-e", config.BusURL+"="+busURL,
		"-e", config.BusCredentialsFile+"=/bus/control-plane.creds",
		"-e", config.ObjectsDir+"=/objects",
		"-e", "SSL_CERT_DIR=/agentiik/trust",
		"-v", in.volumes["controller"]+":/agentiik", "-v", in.volumes["bus"]+":/bus:ro", "-v", in.volumes["objects"]+":/objects", "-v", socket+":/run/postgresql",
		in.controllerIm)
}

// call is one request of the operator's to the API, through the terminator, and what it answered.
func (in *Installation) call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	r, err := http.NewRequestWithContext(ctx, method, in.PublicURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	r.Header.Set("Authorization", "Bearer "+in.token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	answer, err := in.client.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(io.LimitReader(answer.Body, 16<<20))
	return answer.StatusCode, read, err
}

// Operator makes one request as the operator, holds its answer to want, and decodes it into into
// where into is not nil.
func (in *Installation) Operator(method, path string, body any, want int, into any) {
	in.t.Helper()
	code, answer, err := in.call(in.ctx, method, path, body)
	if err != nil {
		in.t.Fatalf("%s %s: %s", method, path, err)
	}
	if code != want {
		in.t.Fatalf("%s %s answered %d, want %d: %s", method, path, code, want, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			in.t.Fatalf("%s %s answered what does not decode: %s: %s", method, path, err, answer)
		}
	}
}

// pool creates the pool both runners join.
func (in *Installation) pool() {
	in.Operator("POST", "/api/v1/runner-pools", api.RunnerPool{Pool: api.Pool{
		Name: Pool, Labels: []string{Label}, Namespaces: []string{Namespace}, Ceilings: &api.Ceilings{},
	}}, http.StatusCreated, nil)
}

// issue issues one join token for a pool, permitting the labels given.
func (in *Installation) issue(pool string, labels ...string) string {
	var issued struct {
		JoinToken api.JoinToken `json:"join_token"`
	}
	in.Operator("POST", "/api/v1/runner-pools/"+pool+"/join-tokens", api.Issue{Labels: labels}, http.StatusCreated, &issued)
	if issued.JoinToken.Token == "" {
		in.t.Fatal("the join token was answered without its secret")
	}
	return issued.JoinToken.Token
}

// JoinDefault stands one more runner up, name, in the pool default, which every installation is
// migrated with and which carries no label, as the Compose file's runner is: with the join token
// init wrote for it, which permits no label, and claiming none. It waits until every runner
// reports ready. It is called once per installation: the runner has init's one runner volume as
// its /etc/agentiik, and init's join token is spent by the one join that uses it.
func (in *Installation) JoinDefault(name string) *Runner {
	in.t.Helper()
	r := in.runner(in.ctx, name, joinFromInit)
	in.Runners = append(in.Runners, r)
	in.ready()
	return r
}

// ready waits until the API has heard each runner's heartbeat say it is ready.
func (in *Installation) ready() {
	eventually(in.ctx, in.t, 2*time.Minute, "both runners reported ready", func() error {
		for _, r := range in.Runners {
			if !r.running(in.ctx) {
				return errStop(fmt.Errorf("runner %s's agent has exited", r.Name))
			}
		}
		var inventory struct {
			Runners []struct {
				Runner        string `json:"runner"`
				ReportedState string `json:"reported_state"`
			} `json:"runners"`
		}
		code, body, err := in.call(in.ctx, "GET", "/api/v1/runners", nil)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("the inventory answered %d: %s", code, body)
		}
		if err := json.Unmarshal(body, &inventory); err != nil {
			return err
		}
		state := map[string]string{}
		for _, r := range inventory.Runners {
			state[r.Runner] = r.ReportedState
		}
		for _, r := range in.Runners {
			if state[r.ID] != "ready" {
				return fmt.Errorf("runner %s (%s) reports %q", r.Name, r.ID, state[r.ID])
			}
		}
		return nil
	})
}

// Database connects to the installation's database as its superuser, which reads every
// namespace's rows: for a test to read what no route answers, such as every dispatch of one key
// and when each was redeemed. The connection is closed when the test ends.
func (in *Installation) Database() *pgx.Conn {
	in.t.Helper()
	conn, err := pgx.Connect(in.ctx, in.superuser)
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// label marks every container and volume this installation made, so that they are found by it.
func (in *Installation) label() string { return "dev.agentiik.e2e=" + in.id }

// container runs docker with args, which start the container name, and takes the container
// down with the installation: its anonymous volumes with it, and its log first where the test
// failed.
func (in *Installation) container(ctx context.Context, name string, args ...string) {
	in.t.Helper()
	in.undo(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(stop, "rm", "-f", "-v", name)
	})
	if _, err := docker(ctx, args...); err != nil {
		in.t.Fatal(err)
	}
	in.logged(name, func() string {
		read, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := dockerCombined(read, "logs", "--tail", "400", name)
		if err != nil {
			return err.Error()
		}
		return out
	})
}

// docker runs the docker command line on this machine's daemon, and answers what it wrote to its
// standard output, trimmed.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// dockerCombined is docker with both of its streams in the answer, which is how a container's
// log is read: it writes to either.
func dockerCombined(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// moduleRoot is the directory of this module's go.mod, which the programs are built from.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("the test runs outside the module, and the programs are built from it")
	}
	return filepath.Dir(mod), nil
}

// freePort is a port nothing listens on at the moment it is asked. Another process may take it
// before it is used, which on a machine running one installation does not happen.
func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// randomHex is n random bytes in hexadecimal.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// stopped is an error eventually gives up on at once, since waiting longer changes nothing.
type stopped struct{ error }

func errStop(err error) error { return stopped{err} }

// eventually calls check until it answers nil, and fails t naming what it waited for and the
// last answer where it has not by within, or once ctx is done.
func eventually(ctx context.Context, t testing.TB, within time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		err := check()
		if err == nil {
			return
		}
		var stop stopped
		if errors.As(err, &stop) || time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("waited %s until %s, and it never was: %s", within, what, err)
		}
		time.Sleep(time.Second)
	}
}

// Push pushes a workflow of Namespace as the version commit, the way agk push does: the entry
// point agentiik.yaml as the whole tree, the manifest of every image a step names, and the digest
// each tag was resolved to. workflow is the name the document gives itself.
func (in *Installation) Push(workflow, commit, document string, manifests map[string][]byte, images map[string]string) {
	in.t.Helper()
	read := map[string]brick.Manifest{}
	for ref, text := range manifests {
		m, err := brick.ParseManifest(text)
		if err != nil {
			in.t.Fatalf("the manifest of %s: %s", ref, err)
		}
		read[ref] = m
	}
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(document)}}
	v, err := version.Capture(tree, "agentiik.yaml", read)
	if err != nil {
		in.t.Fatalf("the workflow could not be captured as a version: %s", err)
	}
	in.Operator("PUT", "/api/v1/"+Namespace+"/workflows/"+workflow+"/versions/"+commit, api.Push{
		Entry: v.Entry, Document: v.Document, Includes: v.Includes, Manifests: v.Manifests,
		Images: images, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}},
	}, http.StatusOK, nil)
}

// Start starts a run of the version commit of workflow as the operator, and answers its
// identifier.
func (in *Installation) Start(workflow, commit string, inputs map[string]any) string {
	in.t.Helper()
	var started struct {
		Run string `json:"run"`
	}
	in.Operator("POST", "/api/v1/"+Namespace+"/workflows/"+workflow+"/runs", api.Start{Commit: commit, Inputs: inputs}, http.StatusAccepted, &started)
	if started.Run == "" {
		in.t.Fatal("the run was started and its identifier was not answered")
	}
	return started.Run
}

// Run is a run as the API answers it, as far as a test reads it.
type Run struct {
	Run   string `json:"run"`
	State string `json:"state"`
	Tasks []struct {
		Task     string `json:"task"`
		Step     string `json:"step"`
		State    string `json:"state"`
		Runner   string `json:"runner"`
		ExitCode *int   `json:"exit_code"`
	} `json:"tasks"`

	// Answer is the whole of what the API answered, which a failure prints.
	Answer json.RawMessage `json:"-"`

	// Reasons are what the controller's evaluation says of each step it gave a reason for, by
	// step, which a failure prints too: the API answers a verdict and not why, and a task that
	// could not be built never reached a runner whose log would say.
	Reasons map[string]string `json:"-"`
}

// Wait reads the run until it has ended, and answers it as it ended. A run that has not ended
// within the time given fails the test.
func (in *Installation) Wait(run string, within time.Duration) Run {
	in.t.Helper()
	var ended Run
	eventually(in.ctx, in.t, within, "run "+run+" ended", func() error {
		code, body, err := in.call(in.ctx, "GET", "/api/v1/"+Namespace+"/runs/"+run, nil)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("reading it answered %d: %s", code, body)
		}
		var r Run
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		r.Answer = body
		switch r.State {
		case "succeeded", "failed", "cancelled", "timed_out":
			ended = r
			return nil
		}
		return fmt.Errorf("it is %s: %s", r.State, body)
	})
	ended.Reasons = in.reasons(run)
	return ended
}

// reasons reads the reason the controller's evaluation of run records for each step, as the
// superuser, out of the document it keeps the evaluator's state in. A reading that fails is
// itself the reason given, since it only ever explains a failure and never makes one.
func (in *Installation) reasons(run string) map[string]string {
	out := map[string]string{}
	conn, err := pgx.Connect(in.ctx, in.superuser)
	if err != nil {
		return map[string]string{"": err.Error()}
	}
	defer conn.Close(context.WithoutCancel(in.ctx))
	rows, err := conn.Query(in.ctx, `
		select s.key, s.value->>'reason'
		from runs r, jsonb_each(coalesce(r.evaluation->'state'->'steps', '{}'::jsonb)) s
		where r.namespace = $1 and r.id = $2 and s.value->>'reason' is not null`, Namespace, run)
	if err != nil {
		return map[string]string{"": err.Error()}
	}
	defer rows.Close()
	for rows.Next() {
		var step, reason string
		if err := rows.Scan(&step, &reason); err != nil {
			return map[string]string{"": err.Error()}
		}
		out[step] = reason
	}
	return out
}
