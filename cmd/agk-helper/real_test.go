package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What a temporary directory cannot prove, run against the daemon on this machine and
// skipped where there is none, exactly as driver/real_test.go does, so that CI stays green
// and this machine tests for real.
//
// Two things, and they are the two the unit tests take on trust. That the binary runs in an
// image this project does not control, which is the whole of what static was for: the first
// test builds an image with nothing in it at all, no libc, no shell, no interpreter and no
// /etc/passwd, and runs the three verbs in it. And that what it writes is what the runner
// collects, through the driver's own mount, environment and collection rather than through
// this package's reading of them.

// scratchImage is the image with nothing in it but this program.
const scratchImage = "agk-helper-scratch:test"

// daemonArch answers with the GOARCH the daemon runs containers as, which is the
// architecture the binary has to be built for. The host's own is not it: an amd64 binary
// bound into an arm64 container gives a script an exec format error rather than a program.
func daemonArch(t *testing.T, socket string) string {
	t.Helper()
	cli, err := docker.Dial(socket)
	if err != nil {
		t.Skipf("opening a client on %s: %v", socket, err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	info, err := cli.Info(ctx)
	if err != nil {
		t.Skipf("asking the daemon what it is: %v", err)
	}
	if info.OSType != "" && info.OSType != "linux" {
		t.Skipf("the daemon runs %s containers, and this is mounted into linux ones", info.OSType)
	}
	switch info.Architecture {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		t.Skipf("the daemon reports the architecture %q, and the release builds amd64 and arm64", info.Architecture)
		return ""
	}
}

// contract lays out a host directory as /agk: one input port, and the two output
// directories the runner creates before a container starts.
func contractDir(t *testing.T, envelope agk.Envelope, port agk.Port) string {
	t.Helper()
	root := t.TempDir()
	in := filepath.Join(root, "in", string(port))
	for _, dir := range []string{in, filepath.Join(root, "out", "ports"), filepath.Join(root, "out", "files")} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if _, err := envelope.Encode(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, EnvelopeFile), b.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestTheHelperRunsInAnImageWithNothingInIt is what static was for, asked of a daemon.
//
// The image is FROM scratch carrying this one file: no libc, no dynamic loader, no shell, no
// /etc/passwd, nothing. Three containers share one /agk, which is what lets the three verbs
// run without a shell to sequence them, and the result is read back with brick.Collect: the
// runner's own door, refusing nothing.
func TestTheHelperRunsInAnImageWithNothingInIt(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker command to build the image with")
	}
	arch := daemonArch(t, socket)

	// Built for the daemon's architecture and laid down at 0555, which is the mode the
	// extracted helper is written at and the mode a read-only mount is exec'd from.
	binary := buildHelper(t, "linux", arch)
	if err := os.Chmod(binary, 0o555); err != nil {
		t.Fatal(err)
	}
	buildScratch(t, binary)

	root := contractDir(t, batch("ok", 3), "in")
	env := []string{
		EnvRunID + "=" + fixtureRun,
		EnvStep + "=" + fixtureStep,
		EnvAttempt + "=1",
		EnvOutPorts + "=out,error",
	}

	// agk items, with no shell anywhere in the container.
	out := inScratch(t, root, env, "", "items")
	if n := len(lines(out)); n != 3 {
		t.Fatalf("agk items wrote %d lines in an image with nothing in it: %q", n, out)
	}

	// agk emit, reading the payload on standard input, with the identity derived from a
	// field so that the envelope is the same twice.
	inScratch(t, root, env, `[{"vat_number":"FR40000","valid":true},{"vat_number":"FR40001","valid":false}]`,
		"emit", "out", "--filter", ".valid", "--id", "vat_number")
	inScratch(t, root, env, `[{"vat_number":"FR40000","valid":true},{"vat_number":"FR40001","valid":false}]`,
		"emit", "error", "--filter", ".valid | not", "--id", "vat_number")

	// agk attach, naming a file the container has and nothing on the host side does.
	inScratch(t, root, env, "", "attach", InDir+"/in/"+EnvelopeFile, "--port", "out", "--name", "source.json")

	// And what the runner collects is what those three wrote.
	m := agk.Meta{RunID: fixtureRun, Step: fixtureStep, Attempt: 1, ProducedAt: time.Now().UTC()}
	collected, err := brick.Collect(filepath.Join(root, "out"), []agk.Port{"out", "error"}, m, agk.DefaultLimits())
	if err != nil {
		t.Fatalf("the runner refuses what the container wrote: %s", err)
	}
	if n := len(collected["out"].Items); n != 1 {
		t.Errorf("port out carries %d items, and one of the two is valid", n)
	}
	if n := len(collected["error"].Items); n != 1 {
		t.Errorf("port error carries %d items, and one of the two is not valid", n)
	}
	if id := collected["out"].Items[0].ID; id != "FR40000" {
		t.Errorf("the item carries the identifier %q, and the field it was derived from carries FR40000", id)
	}

	files := collected["out"].Items[0].Files
	if len(files) != 1 {
		t.Fatalf("the item carries %d files", len(files))
	}
	// The digest the container stated, against the bytes on this side of the mount.
	// That is the check the driver makes before it uploads, made here on the same bytes.
	laid, err := os.ReadFile(filepath.Join(root, "out", "files", files[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(laid)
	if files[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("the entry says sha256 %s and the bytes under that name hash to %s", files[0].SHA256, hex.EncodeToString(sum[:]))
	}
	if files[0].Size != int64(len(laid)) {
		t.Errorf("the entry says %d bytes and the file holds %d", files[0].Size, len(laid))
	}
	want := "agk://run/" + fixtureRun + "/" + fixtureStep + "/out/source.json"
	if got := files[0].URI.String(); got != want {
		t.Errorf("the entry is addressed %s, want %s", got, want)
	}
}

// TestTheHelperIsWhatAScriptStepPipesInto runs the documented shape of the thing: a script
// step in a base image, the helper bound read-only at /agk/bin/agk by the driver, and agk
// items piped into the shell.
//
// It goes through the driver rather than around it, because the half this group does not own
// is the mount, the environment table and the collection, and a test of the helper that
// prepared those itself would be testing this package's reading of them.
func TestTheHelperIsWhatAScriptStepPipesInto(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine")
	}
	arch := daemonArch(t, socket)
	image := smallImage(t, socket)

	binary := buildHelper(t, "linux", arch)
	if err := os.Chmod(binary, 0o555); err != nil {
		t.Fatal(err)
	}

	store, err := artifact.New(artifact.Dir(t.TempDir()), "finance", agk.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := driver.DefaultPolicy()
	// Docker Desktop offers no user namespace remapping, and this is the machine the
	// floor is lifted for.
	policy.RequireUsernsRemap = driver.RemapLifted
	policy.SecretsDir = ""
	policy.StopGrace = 2 * time.Second
	policy.Helper = binary

	log := &memLog{}
	d, err := driver.New(driver.Config{
		Socket:   socket,
		Store:    func(string) (*artifact.Store, error) { return store, nil },
		Repo:     func(context.Context, string, string, string) (string, error) { return t.TempDir(), nil },
		Logs:     log,
		Policy:   policy,
		WorkRoot: t.TempDir(),
		Announce: func(s string) { t.Log(s) },
	})
	if err != nil {
		t.Skipf("opening a driver on %s: %v", socket, err)
	}
	t.Cleanup(func() { d.Close() })

	task := graph.Task{
		ID:        agk.NewTaskID(fixtureRun, "check-vat", 1, agk.Shard{}),
		Run:       fixtureRun,
		Namespace: "finance",
		Step:      "check-vat",
		Attempt:   1,
		Image:     image,
		Inputs:    map[agk.Port]agk.Envelope{"in": batch("ok", 3)},
		Script: []string{
			// The documented example writes agk items, with no path, and nothing
			// puts /agk/bin on PATH: the contract's environment table does not
			// name the variable and the driver does not set it, so the bare name
			// does not resolve and a step written exactly as the page writes it
			// says "agk: not found". The line below is what makes the documented
			// spelling work today, and the gap is one for the page or for the
			// driver's environment rather than for this program, which is mounted
			// at the path it is given.
			`PATH="$PATH:` + filepath.Dir(BinPath) + `"`,
			// The documented pipe, and the shell is the one reading it.
			`agk items | grep -c vat_number > /tmp/count`,
			`test "$(cat /tmp/count)" = "3"`,
			// One item per vat number, with the identity derived from the payload.
			`agk items | sed 's/.*"vat_number":"\([^"]*\)".*/{"vat_number":"\1","valid":true}/' > /tmp/result.jsonl`,
			`agk emit out --from /tmp/result.jsonl --filter '.valid' --id vat_number`,
			`agk emit error --from /tmp/result.jsonl --filter '.valid | not' --id vat_number`,
			// An artifact, hashed by the container itself, attached to one item.
			`printf 'a,b\n1,2\n' > /tmp/report.csv`,
			`agk attach /tmp/report.csv --port out --all`,
		},
		Outputs: []agk.Port{"out", "error"},
		Network: graph.NetworkNone,
	}

	result, err := d.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running the script step: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s with exit code %d, and the container said:\n%s", result.State, result.ExitCode, log)
	}

	out := result.Outputs["out"]
	if len(out.Items) != 3 {
		t.Fatalf("port out carries %d items, and the batch held 3", len(out.Items))
	}
	if len(result.Outputs["error"].Items) != 0 {
		t.Errorf("port error carries %d items, and the filter kept none", len(result.Outputs["error"].Items))
	}
	for i, item := range out.Items {
		if !strings.HasPrefix(item.ID, "FR") {
			t.Errorf("item %d carries the identifier %q, and the identity was derived from vat_number", i+1, item.ID)
		}
		if len(item.Files) != 1 {
			t.Fatalf("item %d carries %d files, and --all attached one to each", i+1, len(item.Files))
		}
		// The driver rewrote the entry to the artifact it uploaded, which it only
		// does after verifying the digest the container stated against the bytes.
		if item.Files[0].Name != "report.csv" || item.Files[0].Size == 0 {
			t.Errorf("item %d carries %+v", i+1, item.Files[0])
		}
		if item.Files[0].SHA256 != out.Items[0].Files[0].SHA256 {
			t.Errorf("item %d disagrees about the digest of one shared artifact", i+1)
		}
	}
}

// memLog keeps what the container wrote, so that a failure here reads like the failure a
// person holding a workflow file would be shown: the step, the exit code, and the last lines
// of the log as the container wrote them.
type memLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (m *memLog) OpenLog(context.Context, agk.TaskID) (io.WriteCloser, error) {
	return nopCloser{m}, nil
}

func (m *memLog) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.Write(b)
}

func (m *memLog) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.String()
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// buildScratch builds the image with nothing in it but this program.
func buildScratch(t *testing.T, binary string) {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agk"), b, 0o555); err != nil {
		t.Fatal(err)
	}
	// No FROM but scratch, and the entry point is the program: an image with a shell in
	// it would leave the reader wondering which of the two was being tested.
	dockerfile := "FROM scratch\nCOPY agk " + BinPath + "\nENTRYPOINT [\"" + BinPath + "\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "build", "-t", scratchImage, dir).CombinedOutput()
	if err != nil {
		t.Skipf("the image with nothing in it could not be built: %v\n%s", err, out)
	}
}

// inScratch runs one verb in that image and answers with what it wrote on standard output.
func inScratch(t *testing.T, root string, env []string, stdin string, args ...string) string {
	t.Helper()
	// Bound the way the driver binds them, port by port and /agk/out whole, rather than
	// the root in one mount: a bind at /agk would hide the one file the image carries.
	argv := []string{"run", "--rm", "-i", "--network", "none",
		"-v", filepath.Join(root, "in", "in") + ":" + InDir + "/in:ro",
		"-v", filepath.Join(root, "out") + ":" + OutDir,
	}
	for _, v := range env {
		argv = append(argv, "-e", v)
	}
	argv = append(argv, scratchImage)
	argv = append(argv, args...)

	cmd := exec.Command("docker", argv...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("agk %s in an image with nothing in it: %v\n%s", strings.Join(args, " "), err, errs.String())
	}
	return out.String()
}

// smallImage finds a base image already on the machine, so that nothing is pulled.
func smallImage(t *testing.T, socket string) string {
	t.Helper()
	cli, err := docker.Dial(socket)
	if err != nil {
		t.Skipf("opening a client on %s: %v", socket, err)
	}
	defer cli.Close()
	for _, ref := range []string{"alpine:3.21", "alpine:latest", "busybox:latest", "debian:stable-slim"} {
		if _, err := cli.ImageInspect(t.Context(), ref); err == nil {
			return ref
		}
	}
	t.Skip("no small image on this machine to run a script step in: docker pull alpine")
	return ""
}
