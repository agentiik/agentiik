package docker_test

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// TestTheWaitIsOpenBeforeTheContainerIsStarted is the ordering this whole package is
// arranged around. With condition=next-exit the daemon writes the response header as
// soon as the wait is registered, so a caller holding the channel knows the exit cannot
// be missed and may then start the container. Opening the wait after the start is the
// exit-during-attach race.
func TestTheWaitIsOpenBeforeTheContainerIsStarted(t *testing.T) {
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run:    func(dockertest.Container) (int, error) { return 7, nil },
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	wait, err := client.ContainerWait(ctxOf(t), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	select {
	case w := <-wait:
		t.Fatalf("the wait answered %+v before the container was started", w)
	default:
	}

	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	select {
	case w := <-wait:
		if w.Err != nil {
			t.Fatalf("waiting: %v", w.Err)
		}
		if w.StatusCode != 7 {
			t.Errorf("exit code: got %d, want 7", w.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never answered")
	}
}

// TestStandardOutputAndStandardErrorStayApart is what the eight-byte demultiplexer is
// for: the shorthand's standard output is published and standard error goes through the
// masker into the log, and a pseudo-terminal would merge them into one stream.
func TestStandardOutputAndStandardErrorStayApart(t *testing.T) {
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run: func(c dockertest.Container) (int, error) {
			fmt.Fprint(c.Stdout, "on out\n")
			fmt.Fprint(c.Stderr, "on err\n")
			fmt.Fprint(c.Stdout, "on out again\n")
			return 0, nil
		},
	}))

	out, errs := runAndRead(t, client, docker.Config{Image: "brick", AttachStdout: true, AttachStderr: true})
	if out != "on out\non out again\n" {
		t.Errorf("standard output: got %q", out)
	}
	if errs != "on err\n" {
		t.Errorf("standard error: got %q", errs)
	}
}

// TestTheEnvelopeReachesStandardInputAndItsEndIsTheHalfClose holds what closing standard
// input has to be: a half-close, because closing the connection would throw away the
// output the brick is about to write.
func TestTheEnvelopeReachesStandardInputAndItsEndIsTheHalfClose(t *testing.T) {
	const envelope = `{"meta":{"port":"in"},"items":[]}`

	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run: func(c dockertest.Container) (int, error) {
			b, err := io.ReadAll(c.Stdin)
			if err != nil {
				return 1, err
			}
			// Reading to the end is what a brick does, and there is an end
			// only because the driver half-closed.
			fmt.Fprintf(c.Stdout, "read %d bytes: %s", len(b), b)
			return 0, nil
		},
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{
		Image: "brick", AttachStdin: true, AttachStdout: true, AttachStderr: true, OpenStdin: true, StdinOnce: true,
	}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(ctxOf(t), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	stream, err := client.ContainerAttach(ctxOf(t), created.ID, docker.AttachOptions{
		Stdin: true, Stdout: true, Stderr: true, Stream: true,
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer stream.Close()
	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	if _, err := io.WriteString(stream.Stdin, envelope); err != nil {
		t.Fatalf("writing the envelope: %v", err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("half-closing: %v", err)
	}

	out, _ := read(t, stream)
	if want := fmt.Sprintf("read %d bytes: %s", len(envelope), envelope); out != want {
		t.Errorf("what the container read back: got %q, want %q", out, want)
	}
	if w := <-wait; w.StatusCode != 0 {
		t.Errorf("exit code: got %d, want 0", w.StatusCode)
	}
}

// TestNothingIsLostOnAFastExit is why AutoRemove is false. A container that ran to
// completion before the attach carried anything still has everything it wrote in the
// daemon's log.
func TestNothingIsLostOnAFastExit(t *testing.T) {
	client, _ := dial(t, dockertest.ExitsDuringAttach, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run: func(c dockertest.Container) (int, error) {
			fmt.Fprint(c.Stdout, "everything it wrote\n")
			fmt.Fprint(c.Stderr, "and what it complained about\n")
			return 0, nil
		},
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick", AttachStdout: true, AttachStderr: true}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	stream, err := client.ContainerAttach(ctxOf(t), created.ID, docker.AttachOptions{Stdout: true, Stderr: true, Stream: true})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}
	attached, _ := read(t, stream)
	stream.Close()
	if attached != "" {
		t.Logf("the attach carried %q, which this case is about it not carrying", attached)
	}

	logs, err := client.ContainerLogs(ctxOf(t), created.ID, docker.LogOptions{Stdout: true, Stderr: true})
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	defer logs.Close()
	out, errs := read(t, logs)
	if out != "everything it wrote\n" {
		t.Errorf("the log's standard output: got %q", out)
	}
	if errs != "and what it complained about\n" {
		t.Errorf("the log's standard error: got %q", errs)
	}
}

// TestTheSettingsTableSurvivesTheWire is the reason the fake daemon is on a real socket
// rather than in this process. What comes back out of Created() went through JSON, so a
// field with the wrong wire name reads as a zero value here instead of passing.
func TestTheSettingsTableSurvivesTheWire(t *testing.T) {
	client, daemon := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))

	pids := int64(256)
	sent := docker.HostConfig{
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges", "seccomp=builtin"},
		AutoRemove:     false,
		Tmpfs:          map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=64m"},
		Mounts: []docker.Mount{
			{Type: docker.MountBind, Source: "/work/repo", Target: "/agk/repo", ReadOnly: true},
			{Type: docker.MountBind, Source: "/work/out", Target: "/agk/out"},
		},
		Resources: docker.Resources{
			Memory:    256 << 20,
			NanoCPUs:  500_000_000,
			PidsLimit: &pids,
			Ulimits:   []docker.Ulimit{{Name: "nofile", Soft: 1024, Hard: 1024}},
		},
	}

	if _, err := client.ContainerCreate(ctxOf(t), "agk-task", docker.Config{
		Image:  "brick",
		Labels: map[string]string{"dev.agentiik.task": "01H/step/1"},
	}, sent, docker.NetworkingConfig{}); err != nil {
		t.Fatalf("creating: %v", err)
	}

	created := daemon.Created()
	if len(created) != 1 {
		t.Fatalf("created %d containers, want one", len(created))
	}
	got := created[0].HostConfig

	if got.NetworkMode != "none" {
		t.Errorf("NetworkMode: got %q, want none", got.NetworkMode)
	}
	if !got.ReadonlyRootfs {
		t.Error("ReadonlyRootfs: got false, and the table says true")
	}
	if !slices.Equal(got.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop: got %v, want [ALL]", got.CapDrop)
	}
	if got.AutoRemove {
		t.Error("AutoRemove: got true, and the table says false")
	}
	if got.Memory != 256<<20 {
		t.Errorf("Memory: got %d", got.Memory)
	}
	if got.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCpus: got %d, which is the field name the daemon reads", got.NanoCPUs)
	}
	if got.PidsLimit == nil || *got.PidsLimit != 256 {
		t.Errorf("PidsLimit: got %v, want 256", got.PidsLimit)
	}
	if len(got.Ulimits) != 1 || got.Ulimits[0].Name != "nofile" {
		t.Errorf("Ulimits: got %v", got.Ulimits)
	}
	if got.Tmpfs["/tmp"] == "" {
		t.Error("Tmpfs: /tmp did not survive")
	}
	repo, ok := created[0].Mount("/agk/repo")
	if !ok || !repo.ReadOnly {
		t.Errorf("/agk/repo: got %+v, and the contract mounts it read-only", repo)
	}
	if created[0].Labels["dev.agentiik.task"] != "01H/step/1" {
		t.Errorf("labels: got %v", created[0].Labels)
	}
}

// TestAContainerIsAdoptedByItsLabel holds what makes at-least-once delivery survivable:
// a redelivered task finds the container it already started instead of starting a second
// one.
func TestAContainerIsAdoptedByItsLabel(t *testing.T) {
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))

	const task = "01HQ/fetch/1"
	if _, err := client.ContainerCreate(ctxOf(t), "", docker.Config{
		Image: "brick", Labels: map[string]string{"dev.agentiik.task": task},
	}, docker.HostConfig{}, docker.NetworkingConfig{}); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if _, err := client.ContainerCreate(ctxOf(t), "", docker.Config{
		Image: "brick", Labels: map[string]string{"dev.agentiik.task": "01HQ/other/1"},
	}, docker.HostConfig{}, docker.NetworkingConfig{}); err != nil {
		t.Fatalf("creating the other one: %v", err)
	}

	found, err := client.ContainerList(ctxOf(t), docker.Filters{}.Add("label", "dev.agentiik.task="+task))
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("the label matched %d containers, want the one task it names", len(found))
	}
	if found[0].Labels["dev.agentiik.task"] != task {
		t.Errorf("adopted %v", found[0].Labels)
	}
}

// TestTheManifestIsReadOutOfTheImage holds how /agk/brick.yaml is reached: through a
// container created from the image and never started, as a tar stream.
func TestTheManifestIsReadOutOfTheImage(t *testing.T) {
	const manifest = "apiVersion: agentiik.dev/v1\nkind: Brick\n"
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest, Manifest: []byte(manifest)}},
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	archive, err := client.ContainerArchive(ctxOf(t), created.ID, "/agk/brick.yaml")
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	defer archive.Close()

	tr := tar.NewReader(archive)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("reading the tar: %v", err)
	}
	if header.Name != "brick.yaml" {
		t.Errorf("entry: got %q, want the file itself", header.Name)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("reading the entry: %v", err)
	}
	if string(b) != manifest {
		t.Errorf("the manifest: got %q", b)
	}
}

// TestStopIsSigtermThenSigkillAfterTheGrace holds where the escalation lives. It belongs
// to the daemon, so that it survives this process dying between the two signals.
func TestStopIsSigtermThenSigkillAfterTheGrace(t *testing.T) {
	client, daemon := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run: func(c dockertest.Container) (int, error) {
			// A container that ignores its term is exactly the one the
			// escalation exists for.
			<-c.Signalled()
			time.Sleep(10 * time.Second)
			return 0, nil
		},
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(ctxOf(t), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	if err := client.ContainerStop(ctxOf(t), created.ID, 100*time.Millisecond); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	select {
	case w := <-wait:
		if w.StatusCode != 137 {
			t.Errorf("exit code: got %d, want 137, which is what a kill leaves", w.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the container was never killed")
	}

	signals := daemon.Created()[0].Signals
	if !slices.Equal(signals, []string{"SIGTERM", "SIGKILL"}) {
		t.Errorf("signals: got %v, want SIGTERM then SIGKILL", signals)
	}
}

// TestAGraceUnderASecondIsNotAKill holds the one rounding decision the stop makes. The
// Engine API counts the grace in whole seconds, and rounding a fraction down to zero
// would turn a stop into a kill.
func TestAGraceUnderASecondIsNotAKill(t *testing.T) {
	asked := make(chan string, 4)
	client, daemon := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))
	daemon.Handle("POST", "/containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.Query().Get("t")
		w.WriteHeader(http.StatusNoContent)
	})

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	for _, c := range []struct {
		grace time.Duration
		want  string
	}{
		{grace: 30 * time.Second, want: "30"},
		{grace: 1500 * time.Millisecond, want: "2"},
		{grace: 100 * time.Millisecond, want: "1"},
		{grace: 0, want: "0"},
		{grace: -1, want: ""},
	} {
		if err := client.ContainerStop(ctxOf(t), created.ID, c.grace); err != nil {
			t.Fatalf("stopping with a grace of %s: %v", c.grace, err)
		}
		if got := <-asked; got != c.want {
			t.Errorf("a grace of %s asked the daemon for t=%q, want %q", c.grace, got, c.want)
		}
	}
}

// TestTheContainerAndItsNetworkAreDestroyed makes the destruction an assertion rather
// than an assumption.
func TestTheContainerAndItsNetworkAreDestroyed(t *testing.T) {
	client, daemon := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))

	network, err := client.NetworkCreate(ctxOf(t), docker.NetworkSpec{Name: "agk-task", Driver: "bridge", Internal: true})
	if err != nil {
		t.Fatalf("creating the network: %v", err)
	}
	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{
		EndpointsConfig: map[string]*docker.EndpointSettings{"agk-task": {NetworkID: network.ID}},
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := client.ContainerRemove(ctxOf(t), created.ID, false); err != nil {
		t.Fatalf("removing the container: %v", err)
	}
	if err := client.NetworkRemove(ctxOf(t), network.ID); err != nil {
		t.Fatalf("removing the network: %v", err)
	}

	removed := daemon.Removed()
	if !slices.Contains(removed, created.ID) {
		t.Errorf("the container was not destroyed: %v", removed)
	}
	if !slices.Contains(removed, network.ID) {
		t.Errorf("the network was not destroyed: %v", removed)
	}
	if _, err := client.ContainerInspect(ctxOf(t), created.ID); !docker.IsNotFound(err) {
		t.Errorf("the container is still there: %v", err)
	}
}

// TestANetworkIsCreatedWithIPv6SaidAndItsOptions reads the create body as the daemon reads
// it. EnableIPv6 is sent when it is false, because a field left out is the daemon's own
// default and a daemon.json may turn IPv6 on for every network; and the driver's options
// arrive where a bridge reads them.
func TestANetworkIsCreatedWithIPv6SaidAndItsOptions(t *testing.T) {
	client, daemon := dial(t)
	var body map[string]any
	daemon.Handle("POST", "/networks/create", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the create body did not read: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"n1"}`))
	})

	if _, err := client.NetworkCreate(ctxOf(t), docker.NetworkSpec{
		Name: "agk-task", Driver: "bridge", Internal: true,
		Options: map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "isolated"},
	}); err != nil {
		t.Fatalf("creating the network: %v", err)
	}
	if v, ok := body["EnableIPv6"]; !ok || v != false {
		t.Errorf("EnableIPv6 went as %v, sent %v, and it is sent false rather than left to the daemon", v, ok)
	}
	options, _ := body["Options"].(map[string]any)
	if options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" {
		t.Errorf("the options went as %v", body["Options"])
	}
}

// TestTheVersionSpokenIsAskedByNumber holds Speaks to the version settled on at the dial,
// compared as two numbers rather than as text, where 1.9 comes after 1.48.
func TestTheVersionSpokenIsAskedByNumber(t *testing.T) {
	client, _ := dial(t, dockertest.APIVersion("1.48"))
	for v, want := range map[string]bool{"1.41": true, "1.9": true, "1.48": true, "1.49": false, "2.0": false, "one": false} {
		if got := client.Speaks(v); got != want {
			t.Errorf("a client speaking 1.48 answers Speaks(%q) %v", v, got)
		}
	}
}

// TestTheTwoMomentsComeFromTheDaemon holds why StartedAt and FinishedAt are read off an
// inspect rather than taken from a clock on this side: the same task read twice reports
// the same two moments.
func TestTheTwoMomentsComeFromTheDaemon(t *testing.T) {
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
		Run:    func(dockertest.Container) (int, error) { return 0, nil },
	}))

	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(ctxOf(t), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}
	<-wait

	first, err := client.ContainerInspect(ctxOf(t), created.ID)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	second, err := client.ContainerInspect(ctxOf(t), created.ID)
	if err != nil {
		t.Fatalf("inspecting again: %v", err)
	}
	if first.State.StartedAt.IsZero() || first.State.FinishedAt.IsZero() {
		t.Fatalf("the two moments: %+v", first.State)
	}
	if !first.State.StartedAt.Equal(second.State.StartedAt) || !first.State.FinishedAt.Equal(second.State.FinishedAt) {
		t.Error("the same task read twice reported two different pairs of moments")
	}
	if first.State.Running {
		t.Error("a container that exited is reported as running")
	}
}

// runAndRead runs one container to completion and answers with what it wrote on each
// stream, read through the attach.
func runAndRead(t *testing.T, client *docker.Client, cfg docker.Config) (stdout, stderr string) {
	t.Helper()

	created, err := client.ContainerCreate(ctxOf(t), "", cfg, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	wait, err := client.ContainerWait(ctxOf(t), created.ID, docker.WaitNextExit)
	if err != nil {
		t.Fatalf("opening the wait: %v", err)
	}
	stream, err := client.ContainerAttach(ctxOf(t), created.ID, docker.AttachOptions{
		Stdin: cfg.AttachStdin, Stdout: true, Stderr: true, Stream: true,
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer stream.Close()
	if err := client.ContainerStart(ctxOf(t), created.ID); err != nil {
		t.Fatalf("starting: %v", err)
	}

	stdout, stderr = read(t, stream)
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never answered")
	}
	return stdout, stderr
}

// read drains a demultiplexed stream and answers with each of its two halves.
func read(t *testing.T, stream *docker.Stream) (stdout, stderr string) {
	t.Helper()

	var out, errs strings.Builder
	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "use of closed") {
				break
			}
			t.Fatalf("reading a frame: %v", err)
		}
		switch frame.Stream {
		case docker.Stdout:
			out.Write(frame.Bytes)
		case docker.Stderr:
			errs.Write(frame.Bytes)
		}
	}
	return out.String(), errs.String()
}
