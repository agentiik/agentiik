package dockertest_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The fake daemon is tested through the client that will talk to it, over the socket it
// will talk on. Testing it any other way would test the fake against itself: what has to
// hold is that internal/docker, written against the Engine API, finds the Engine API
// here, and a fake that satisfies a hand-written request while failing the real client
// would let every rule test above it pass on a wire bug.

// brickImage is the reference a test pulls, pinned by digest as production pins one.
const brickImage = "ghcr.io/acme/agk-normalize@sha256:9f2c1d000000000000000000000000000000000000000000000000000000b7e0"

// theManifest is what a brick carries at /agk/brick.yaml, read out of the image through
// a container created from it and never started.
const theManifest = "apiVersion: agentiik.dev/v1\nkind: Brick\n"

// start opens a daemon and a client on it, and closes both when the test ends.
func start(t *testing.T, behaviours ...dockertest.Behaviour) (*dockertest.Daemon, *docker.Client) {
	t.Helper()
	d, err := dockertest.NewDaemon(behaviours...)
	if err != nil {
		t.Fatalf("starting the fake daemon: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	c, err := docker.Dial(d.Socket())
	if err != nil {
		t.Fatalf("dialling the fake daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return d, c
}

// anImage is what a pull resolves to: a brick, with a manifest and an account of its own.
func anImage() map[string]dockertest.Image {
	return map[string]dockertest.Image{
		brickImage: {
			Digest:   "sha256:9f2c1d000000000000000000000000000000000000000000000000000000b7e0",
			Manifest: []byte(theManifest),
			Config:   docker.ImageConfig{User: "1000:1000"},
			Layers:   4,
		},
	}
}

// aTask is the create of one task, written the way the settings table writes it, so that
// what comes back out of Created is the table and not a paraphrase of it.
func aTask(work string) (docker.Config, docker.HostConfig) {
	pids := int64(256)
	cfg := docker.Config{
		Image:        brickImage,
		Env:          []string{"AGK_RUN_ID=01JMZ8W4K7A1B2C3D4E5F6G7H8", "AGK_STEP=normalize", "AGK_OUT_PORTS=ok,rejected"},
		User:         "1000:1000",
		Labels:       map[string]string{"dev.agentiik.task": "01JMZ8W4K7A1B2C3D4E5F6G7H8/normalize/1"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
	}
	host := docker.HostConfig{
		Mounts: []docker.Mount{
			{Type: docker.MountBind, Source: work, Target: "/agk/out"},
			{Type: docker.MountBind, Source: work, Target: "/agk/repo", ReadOnly: true},
		},
		NetworkMode:    "none",
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=64m"},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		AutoRemove:     false,
		Resources: docker.Resources{
			Memory:    512 << 20,
			NanoCPUs:  1_000_000_000,
			PidsLimit: &pids,
			Ulimits:   []docker.Ulimit{{Name: "nofile", Soft: 1024, Hard: 1024}},
		},
	}
	return cfg, host
}

// drain reads a demultiplexed stream to its end and answers with the two streams apart.
func drain(t *testing.T, s *docker.Stream) (stdout, stderr string) {
	t.Helper()
	var out, errs strings.Builder
	for {
		frame, err := s.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosed(err) {
				t.Logf("the stream ended on %v", err)
			}
			return out.String(), errs.String()
		}
		switch frame.Stream {
		case docker.Stdout:
			out.Write(frame.Bytes)
		case docker.Stderr:
			errs.Write(frame.Bytes)
		}
	}
}

// isClosed says whether a stream ended because its connection went, which is what a
// cancelled attach looks like from the reading side.
func isClosed(err error) bool {
	return errors.Is(err, os.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func TestTheVersionIsNegotiatedWithTheDaemonThatAnswered(t *testing.T) {
	_, c := start(t)
	if got := c.APIVersion(); got != docker.Ceiling {
		t.Errorf("version: got %q, want the ceiling %q where the daemon offers it", got, docker.Ceiling)
	}

	_, older := start(t, dockertest.APIVersion("1.55"))
	if got := older.APIVersion(); got != "1.55" {
		t.Errorf("version: got %q, want 1.55, which is what this daemon offers and what every path must carry", got)
	}
}

func TestADaemonBelowTheFloorIsRefusedByName(t *testing.T) {
	d, err := dockertest.NewDaemon(dockertest.APIVersion("1.24"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	_, err = docker.Dial(d.Socket())
	if err == nil {
		t.Fatal("a daemon below the floor was accepted")
	}
	if !strings.Contains(err.Error(), "1.24") || !strings.Contains(err.Error(), docker.Floor) {
		t.Errorf("refusal: got %q, want both versions named", err)
	}
}

func TestTheUsernsFloorIsReadOffTheDaemonsOwnInfo(t *testing.T) {
	_, without := start(t)
	info, err := without.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.UsernsRemapped() {
		t.Error("a daemon with no name=userns reads as remapped, which is the floor not refusing")
	}

	_, with := start(t, dockertest.WithUsernsRemap(100000, 100000))
	info, err = with.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !info.UsernsRemapped() {
		t.Fatalf("security options: got %v, want name=userns among them", info.SecurityOptions)
	}
	uid, gid, ok := info.RemappedRange()
	if !ok || uid != "100000" || gid != "100000" {
		t.Errorf("remapped range: got %s.%s (%v) off %q, want 100000.100000", uid, gid, ok, info.DockerRootDir)
	}
}

func TestAPullReadsEveryMessageAndAPullThatDiedIsNotAPullThatWorked(t *testing.T) {
	_, c := start(t, dockertest.With(dockertest.Options{Images: anImage()}))

	var messages int
	if err := c.ImagePull(t.Context(), brickImage, "", func(docker.Progress) { messages++ }); err != nil {
		t.Fatalf("pulling: %v", err)
	}
	if messages < 4 {
		t.Errorf("progress messages: got %d, want one per layer and the digest after them", messages)
	}

	_, failing := start(t, dockertest.With(dockertest.Options{Images: anImage()}), dockertest.PullFailsHalfway)
	var seen int
	err := failing.ImagePull(t.Context(), brickImage, "", func(docker.Progress) { seen++ })
	if err == nil {
		t.Fatal("a pull that died inside a 200 was taken for a pull that worked")
	}
	if seen == 0 {
		t.Error("the pull failed before it streamed anything, which is not the case this is about")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("failure: got %q, want what the stream said went wrong", err)
	}
}

func TestAReferenceThePullDoesNotHoldIsRefused(t *testing.T) {
	_, c := start(t, dockertest.With(dockertest.Options{Images: anImage()}))

	err := c.ImagePull(t.Context(), "ghcr.io/acme/nothing@sha256:dead", "", nil)
	if err == nil {
		t.Fatal("a reference the registry does not have was pulled")
	}
	if !docker.IsNotFound(err) {
		t.Errorf("failure: got %q, want the 404 a registry answers", err)
	}
}

func TestAnImageResolvesToTheDigestAndTheAccountItDeclares(t *testing.T) {
	_, c := start(t, dockertest.With(dockertest.Options{Images: anImage()}))

	img, err := c.ImageInspect(t.Context(), brickImage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.ID, "sha256:9f2c1d") {
		t.Errorf("id: got %q, want the digest the reference resolved to", img.ID)
	}
	if img.Config.User != "1000:1000" {
		t.Errorf("user: got %q, want the account the image declares, which is what a root user is refused on", img.Config.User)
	}
}

func TestTheManifestIsReadOutOfTheImage(t *testing.T) {
	_, c := start(t, dockertest.With(dockertest.Options{Images: anImage()}))
	cfg, host := aTask(t.TempDir())

	created, err := c.ContainerCreate(t.Context(), "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.ContainerArchive(t.Context(), created.ID, "/agk/brick.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("the archive carries no entry: %v", err)
	}
	if header.Name != "brick.yaml" {
		t.Errorf("entry: got %q, want brick.yaml", header.Name)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != theManifest {
		t.Errorf("manifest: got %q, want what the image carries", b)
	}
}

func TestABaseImageCarriesNoManifest(t *testing.T) {
	images := anImage()
	base := images[brickImage]
	base.Manifest = nil
	images[brickImage] = base

	_, c := start(t, dockertest.With(dockertest.Options{Images: images}))
	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(t.Context(), "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ContainerArchive(t.Context(), created.ID, "/agk/brick.yaml")
	if err == nil {
		t.Fatal("a base image answered with a manifest")
	}
	if !docker.IsNotFound(err) {
		t.Errorf("failure: got %q, want the 404 that tells a base image from a brick", err)
	}
}

func TestOneTaskFromCreateToRemove(t *testing.T) {
	work := t.TempDir()
	arrived := make(chan string, 1)
	d, c := start(t, dockertest.With(dockertest.Options{
		Images: anImage(),
		Run: func(container dockertest.Container) (int, error) {
			b, err := io.ReadAll(container.Stdin)
			if err != nil {
				return 1, err
			}
			arrived <- string(b)
			if container.Work == "" {
				return 1, errors.New("the container was given no working directory")
			}
			if err := os.MkdirAll(filepath.Join(container.Work, "ports"), 0o755); err != nil {
				return 1, err
			}
			if err := os.WriteFile(filepath.Join(container.Work, "ports", "ok.json"), []byte("{}"), 0o644); err != nil {
				return 1, err
			}
			io.WriteString(container.Stdout, "on standard output")
			io.WriteString(container.Stderr, "on standard error")
			return 7, nil
		},
	}))

	ctx := t.Context()
	cfg, host := aTask(work)
	created, err := c.ContainerCreate(ctx, "agk-normalize-1", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The wait is opened before the start, which is what makes the exit impossible
	// to miss.
	waited, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	attachCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := c.ContainerAttach(attachCtx, created.ID, docker.AttachOptions{Stdin: true, Stdout: true, Stderr: true, Stream: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer stream.Close()

	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	const envelope = `{"meta":{"port":"in"},"items":[]}`
	go func() {
		io.WriteString(stream.Stdin, envelope)
		stream.CloseWrite()
	}()

	stdout, stderr := drain(t, stream)
	if attachCtx.Err() != nil {
		t.Fatal("the attached stream did not end when the container exited, so a driver reading it to the end would wait for a container that is over")
	}
	if stdout != "on standard output" || stderr != "on standard error" {
		t.Errorf("streams: got %q and %q, want the two apart", stdout, stderr)
	}

	select {
	case got := <-arrived:
		if got != envelope {
			t.Errorf("standard input: got %q, want the envelope the driver wrote", got)
		}
	default:
		t.Error("the container was given no standard input")
	}

	exit := <-waited
	if exit.Err != nil {
		t.Fatalf("wait: %v", exit.Err)
	}
	if exit.StatusCode != 7 {
		t.Errorf("exit: got %d, want 7", exit.StatusCode)
	}

	if _, err := os.Stat(filepath.Join(work, "ports", "ok.json")); err != nil {
		t.Errorf("what the container wrote under /agk/out is not in the working directory: %v", err)
	}

	// The log is taken from the daemon after the exit, which is the reason
	// AutoRemove is false and why nothing is lost on a fast exit.
	logs, err := c.ContainerLogs(ctx, created.ID, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer logs.Close()
	loggedOut, loggedErr := drain(t, logs)
	if loggedOut != "on standard output" || loggedErr != "on standard error" {
		t.Errorf("log: got %q and %q, want what the container wrote, still apart", loggedOut, loggedErr)
	}

	inspected, err := c.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.State.ExitCode != 7 || inspected.State.Running {
		t.Errorf("state: got %+v, want an exited container carrying 7", inspected.State)
	}
	if inspected.State.StartedAt.IsZero() || inspected.State.FinishedAt.IsZero() {
		t.Errorf("moments: got %v and %v, want the daemon's own, which is what lets one task read twice report the same two", inspected.State.StartedAt, inspected.State.FinishedAt)
	}

	// Adoption: the task is found again by the label it was created with.
	list, err := c.ContainerList(ctx, docker.Filters{}.Add("label", "dev.agentiik.task=01JMZ8W4K7A1B2C3D4E5F6G7H8/normalize/1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("list by label: got %v, want the container that carries it", list)
	}

	if err := c.ContainerRemove(ctx, created.ID, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed := d.Removed(); len(removed) != 1 || removed[0] != created.ID {
		t.Errorf("removed: got %v, want the container this task created", removed)
	}
	if list, err = c.ContainerList(ctx, nil); err != nil || len(list) != 0 {
		t.Errorf("after the remove: got %v (%v), want nothing left behind", list, err)
	}
}

func TestTheSettingsTableIsReadBackAsItWentOnTheWire(t *testing.T) {
	work := t.TempDir()
	d, c := start(t, dockertest.With(dockertest.Options{Images: anImage()}))

	cfg, host := aTask(work)
	if _, err := c.ContainerCreate(t.Context(), "agk-normalize-1", cfg, host, docker.NetworkingConfig{}); err != nil {
		t.Fatal(err)
	}

	created := d.Created()
	if len(created) != 1 {
		t.Fatalf("created: got %d containers, want the one that was asked for", len(created))
	}
	got := created[0]

	if !got.HostConfig.ReadonlyRootfs {
		t.Error("ReadonlyRootfs did not survive the wire, and a container that can write its own root filesystem is not the container the table describes")
	}
	if got.HostConfig.AutoRemove {
		t.Error("AutoRemove came back true, which would take the log with the container")
	}
	if len(got.HostConfig.CapDrop) != 1 || got.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop: got %v, want ALL", got.HostConfig.CapDrop)
	}
	if got.HostConfig.PidsLimit == nil || *got.HostConfig.PidsLimit != 256 {
		t.Errorf("PidsLimit: got %v, want 256, which is not the same answer as unset", got.HostConfig.PidsLimit)
	}
	if got.HostConfig.Tmpfs["/tmp"] == "" {
		t.Errorf("Tmpfs: got %v, want /tmp sized and flagged", got.HostConfig.Tmpfs)
	}
	if mount, ok := got.Mount("/agk/repo"); !ok || !mount.ReadOnly {
		t.Errorf("/agk/repo: got %+v (%v), want a read-only bind", mount, ok)
	}
	if got.Work != work {
		t.Errorf("work: got %q, want the host side of /agk/out, %q", got.Work, work)
	}
	if step, ok := got.Env("AGK_STEP"); !ok || step != "normalize" {
		t.Errorf("AGK_STEP: got %q (%v), want normalize", step, ok)
	}
	if _, ok := got.Env("AGK_SHARD"); ok {
		t.Error("AGK_SHARD is set on a task with no fan-out, where it is absent rather than empty")
	}
	if !got.Config.OpenStdin || got.Config.Tty {
		t.Errorf("config: got OpenStdin %v and Tty %v, want standard input open and no terminal merging the two output streams", got.Config.OpenStdin, got.Config.Tty)
	}
}

func TestAContainerThatExitsDuringTheAttachStillReportsItsCodeAndItsLog(t *testing.T) {
	_, c := start(t,
		dockertest.With(dockertest.Options{
			Images: anImage(),
			Run: func(container dockertest.Container) (int, error) {
				io.WriteString(container.Stdout, "written before anybody was listening")
				return 3, nil
			},
		}),
		dockertest.ExitsDuringAttach,
	)

	ctx := t.Context()
	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}

	attachCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := c.ContainerAttach(attachCtx, created.ID, docker.AttachOptions{Stdin: true, Stdout: true, Stderr: true, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	stdout, _ := drain(t, stream)
	if attachCtx.Err() != nil {
		t.Fatal("the attached stream never ended, so a driver reading it waits for a container that is already over")
	}
	if stdout != "" {
		t.Errorf("attached stream: got %q, want nothing: the container was over before the stream carried anything", stdout)
	}

	exit := <-waited
	if exit.Err != nil || exit.StatusCode != 3 {
		t.Errorf("exit: got %d (%v), want 3 off the wait that was opened before the start", exit.StatusCode, exit.Err)
	}

	logs, err := c.ContainerLogs(ctx, created.ID, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	if loggedOut, _ := drain(t, logs); loggedOut != "written before anybody was listening" {
		t.Errorf("log: got %q, want what the container wrote before the attach, which is what AutoRemove being false keeps", loggedOut)
	}
}

func TestADaemonThatVanishesIsNotARefusal(t *testing.T) {
	_, c := start(t,
		dockertest.With(dockertest.Options{Images: anImage()}),
		dockertest.DaemonVanishes,
	)

	ctx := t.Context()
	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := c.ContainerStart(ctx, created.ID); err == nil {
		t.Fatal("a daemon that closed the connection reported a container that started")
	}
	err = c.ContainerStart(ctx, created.ID)
	if err == nil {
		t.Fatal("the daemon came back")
	}
	var refusal *docker.Error
	if errors.As(err, &refusal) {
		t.Errorf("failure: got %q, want a daemon that answered nothing rather than one that refused", err)
	}
	if _, err := c.Info(ctx); err == nil {
		t.Error("a vanished daemon answered its info")
	}
}

func TestAnOutOfMemoryKillIsOnTheEventStreamAndNotOnTheWait(t *testing.T) {
	_, c := start(t,
		dockertest.With(dockertest.Options{Images: anImage()}),
		dockertest.OOMKills,
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, _ := c.Events(ctx, time.Time{}, docker.Filters{}.Add("type", docker.EventTypeContainer))

	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	var oom, die bool
	var code string
	deadline := time.After(10 * time.Second)
	for !die {
		select {
		case e, open := <-events:
			if !open {
				t.Fatal("the event stream closed before the die")
			}
			switch e.Action {
			case docker.ActionOOM:
				oom = true
			case docker.ActionDie:
				die = true
				code = e.Actor.Attributes["exitCode"]
			}
		case <-deadline:
			t.Fatal("no die on the event stream, which is the one place this exit is reported")
		}
	}
	if !oom {
		t.Error("the die arrived with no oom before it, and the die alone does not say why")
	}
	if code != "137" {
		t.Errorf("exit code on the die: got %q, want 137", code)
	}

	select {
	case got := <-waited:
		t.Errorf("the wait answered %v, and this is the exit a wait never sees", got)
	case <-time.After(200 * time.Millisecond):
	}

	inspected, err := c.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !inspected.State.OOMKilled || inspected.State.ExitCode != 137 {
		t.Errorf("inspect: got OOMKilled %v and code %d, want the backstop to say what the wait would not", inspected.State.OOMKilled, inspected.State.ExitCode)
	}
}

func TestAnEventStreamThatDropsIsResumedFromWhereItStopped(t *testing.T) {
	_, c := start(t,
		dockertest.With(dockertest.Options{Images: anImage()}),
		dockertest.EventStreamDrops,
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, _ := c.Events(ctx, time.Time{}, nil)

	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case e, open := <-events:
			if !open {
				t.Fatal("the stream gave up rather than reconnecting")
			}
			if e.Action == docker.ActionDie {
				return
			}
		case <-deadline:
			t.Fatal("the die never arrived, so a stream that drops loses an exit instead of replaying it")
		}
	}
}

func TestAStopIsSigtermAndThenSigkillAfterTheGrace(t *testing.T) {
	_, c := start(t, dockertest.With(dockertest.Options{
		Images: anImage(),
		Run: func(container dockertest.Container) (int, error) {
			if got := <-container.Signalled(); got != "SIGTERM" {
				return 1, errors.New("the first signal was " + got)
			}
			return 143, nil
		},
	}))

	ctx := t.Context()
	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStop(ctx, created.ID, 5*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	exit := <-waited
	if exit.StatusCode != 143 {
		t.Errorf("exit: got %d, want 143, the code a container that honoured the signal chose", exit.StatusCode)
	}
}

func TestAContainerThatIgnoresTheSignalIsKilledWhenTheGraceRunsOut(t *testing.T) {
	held := make(chan struct{})
	_, c := start(t, dockertest.With(dockertest.Options{
		Images: anImage(),
		Run: func(container dockertest.Container) (int, error) {
			<-held
			return 0, nil
		},
	}))
	defer close(held)

	ctx := t.Context()
	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	// A grace of nothing is the escalation with no wait in it, which is what the
	// daemon does when the grace has already run out.
	if err := c.ContainerStop(ctx, created.ID, 0); err != nil {
		t.Fatalf("stop: %v", err)
	}

	select {
	case exit := <-waited:
		if exit.StatusCode != 137 {
			t.Errorf("exit: got %d, want 137, which the exit table charges to the runtime", exit.StatusCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the container that ignored the signal was never killed")
	}
}

// A daemon runs an exited container again on POST /start, and answers 304 only while it is
// running. A fake answering 304 to any container ever started would let a driver that
// starts a container which has already done its work pass, while every real daemon ran the
// brick twice.
func TestAnExitedContainerStartsAgainAndARunningOneDoesNot(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	d, c := start(t, dockertest.With(dockertest.Options{
		Images: anImage(),
		Run: func(container dockertest.Container) (int, error) {
			n := runs.Add(1)
			io.WriteString(container.Stdout, "ran\n")
			if n == 2 {
				// The second run is held, so that there is a running
				// container to start.
				<-release
			}
			return int(n), nil
		},
	}))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, _ := c.Events(ctx, time.Time{}, docker.Filters{}.Add("type", docker.EventTypeContainer))

	cfg, host := aTask(t.TempDir())
	created, err := c.ContainerCreate(ctx, "", cfg, host, docker.NetworkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if exit := <-first; exit.Err != nil || exit.StatusCode != 1 {
		t.Fatalf("the first run exited %d (%v)", exit.StatusCode, exit.Err)
	}

	// not-running on a container that is over answers at once, with the code it
	// already exited with.
	over, err := c.ContainerWait(ctx, created.ID, docker.WaitNotRunning)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-over:
		if exit.StatusCode != 1 {
			t.Errorf("not-running answered %d on a container that exited 1", exit.StatusCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not-running waited on a container that is not running")
	}

	// next-exit on the same container waits for the run a start begins, and the start
	// begins one.
	next, err := c.ContainerWait(ctx, created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatalf("starting the exited container: %v", err)
	}

	// It is running now, so a start is answered 304: nothing runs and nothing is
	// emitted.
	before := d.Events()
	if err := c.ContainerStart(ctx, created.ID); err != nil {
		t.Fatalf("starting the running container: %v", err)
	}
	if d.Events() != before {
		t.Error("starting a running container emitted an event, and a daemon answers it 304 and does nothing")
	}
	close(release)

	select {
	case exit := <-next:
		if exit.StatusCode != 2 {
			t.Errorf("next-exit answered %d, and it waits for the run the start began, which exited 2", exit.StatusCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second run never ended")
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("the container ran %d times for one start while created and one while exited", n)
	}

	var actions []string
	deadline := time.After(10 * time.Second)
	for dies := 0; dies < 2; {
		select {
		case e, open := <-events:
			if !open {
				t.Fatal("the event stream closed before the second die")
			}
			actions = append(actions, e.Action)
			if e.Action == docker.ActionDie {
				dies++
			}
		case <-deadline:
			t.Fatalf("the events were %v, and the second run never died", actions)
		}
	}
	if got := strings.Join(actions, " "); got != "start die start die" {
		t.Errorf("the events were %q, and each run is a start and a die", got)
	}

	logs, err := c.ContainerLogs(ctx, created.ID, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	if out, _ := drain(t, logs); out != "ran\nran\n" {
		t.Errorf("the log reads %q, and a daemon's log of a container is every run it has had", out)
	}
}

func TestANetworkIsCreatedFoundByItsLabelAndRemoved(t *testing.T) {
	d, c := start(t)
	ctx := t.Context()

	created, err := c.NetworkCreate(ctx, docker.NetworkSpec{
		Name:     "agk-01JMZ8W4K7A1B2C3D4E5F6G7H8-normalize-1",
		Driver:   "bridge",
		Internal: true,
		Labels:   map[string]string{"dev.agentiik.task": "01JMZ8W4K7A1B2C3D4E5F6G7H8/normalize/1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := c.NetworkList(ctx, docker.Filters{}.Add("label", "dev.agentiik.task=01JMZ8W4K7A1B2C3D4E5F6G7H8/normalize/1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Internal {
		t.Fatalf("networks: got %v, want the one internal network this task created", list)
	}

	if err := c.NetworkRemove(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if removed := d.Removed(); len(removed) != 1 || removed[0] != created.ID {
		t.Errorf("removed: got %v, want the network, so that its destruction is an assertion", removed)
	}
}

func TestARouteATestReplacedWinsOverTheDaemonsOwn(t *testing.T) {
	d, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Handle("GET", "/info", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"the daemon is starting"}`))
	})

	c, err := docker.Dial(d.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.Info(t.Context())
	if err == nil {
		t.Fatal("the replaced route was not consulted")
	}
	var refusal *docker.Error
	if !errors.As(err, &refusal) || refusal.Status != 500 || refusal.Message != "the daemon is starting" {
		t.Errorf("failure: got %q, want the daemon's own message off the replaced route", err)
	}
}

func TestSocketAnswersWhereARealDaemonIs(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		t.Skip("no Docker daemon on this machine, which is what CI looks like")
	}
	if socket == "" {
		t.Fatal("a daemon was found at no path")
	}

	c, err := docker.Dial(socket)
	if err != nil {
		t.Fatalf("dialling the daemon at %s: %v", socket, err)
	}
	defer c.Close()

	if c.APIVersion() == "" {
		t.Error("the version was not negotiated with a daemon that is there")
	}
	if _, err := c.Info(t.Context()); err != nil {
		t.Errorf("info: %v", err)
	}
}
