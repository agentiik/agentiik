package driver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The manifest of a brick that honours the contract, which is the smallest one the
// contract accepts: an identity and the account the container runs as.
const goodManifest = `apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: http-request
  version: 1.4.0
spec:
  runtime:
    user: "65532:65532"
`

// The manifest of a brick that declares the account the non-root rule exists to refuse.
const rootManifest = `apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: careless
  version: 0.1.0
spec:
  runtime:
    user: root
`

const imageDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// withDaemon opens a client on a fake daemon and hands back both, because these rules
// are read as much off what the daemon was asked to do as off what it answered.
func withDaemon(t *testing.T, bs ...dockertest.Behaviour) (*docker.Client, *dockertest.Daemon) {
	t.Helper()
	daemon, err := dockertest.NewDaemon(bs...)
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	t.Cleanup(func() { daemon.Close() })

	cli, err := docker.Dial(daemon.Socket())
	if err != nil {
		t.Fatalf("dialing the fake daemon: %s", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli, daemon
}

// imageTask is one task naming one image, which is all these rules need of a task.
func imageTask(ref string) graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "fetch", 1, agk.Shard{}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "fetch",
		Attempt:   1,
		Image:     ref,
	}
}

// "The manifest is embedded in the image at /agk/brick.yaml. It is read on first pull and
// cached by digest."
func TestTheManifestIsReadOnFirstPullAndCachedByDigest(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	cli, daemon := withDaemon(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(goodManifest)}},
	}))

	var reads atomic.Int64
	daemon.Handle("GET", "/containers/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		w.Write(tarOf(t, goodManifest))
	})

	cache := newManifests()
	first, err := resolveImage(t.Context(), cli, cache, imageTask(ref), "", nil)
	if err != nil {
		t.Fatalf("resolving %s: %s", ref, err)
	}
	if first.Manifest == nil {
		t.Fatal("the image carries a manifest and none was read")
	}
	if first.Manifest.Metadata.Name != "http-request" {
		t.Errorf("the manifest read is %q", first.Manifest.Metadata.Name)
	}
	if first.User != "65532:65532" {
		t.Errorf("the account is %q, and the manifest declares 65532:65532", first.User)
	}
	if first.Digest != imageDigest {
		t.Errorf("the digest is %q, and the reference resolved to %s", first.Digest, imageDigest)
	}

	second, err := resolveImage(t.Context(), cli, cache, imageTask(ref), "", nil)
	if err != nil {
		t.Fatalf("resolving %s again: %s", ref, err)
	}
	if second.Manifest == nil || second.Manifest.Metadata.Name != "http-request" {
		t.Fatal("the cached manifest is not the one that was read")
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("%s was read %d times, and it is read on first pull and cached by digest", "/agk/brick.yaml", got)
	}
}

// "refusing a manifest that declares a root user": the refusal comes before a container
// for the task exists, and it names the account and the rule.
func TestAManifestDeclaringARootUserIsRefusedBeforeAnyContainerRuns(t *testing.T) {
	const ref = "ghcr.io/acme/careless@" + imageDigest
	cli, daemon := withDaemon(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(rootManifest)}},
	}))

	_, err := resolveImage(t.Context(), cli, newManifests(), imageTask("ghcr.io/acme/careless@"+imageDigest), "", nil)
	if err == nil {
		t.Fatal("a manifest declaring a root user was accepted")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("the refusal is %q, and it does not name the account it refused", err)
	}
	if !strings.Contains(err.Error(), "step fetch") {
		t.Errorf("the refusal is %q, and it does not name the step", err)
	}

	// The only container the daemon was asked for is the one the manifest was read
	// through, and it does not carry the task label, so nothing can adopt it as the
	// task's own.
	for _, c := range daemon.Created() {
		if _, ok := c.Labels[LabelTask]; ok {
			t.Errorf("a container carrying %s was created for a manifest that was refused", LabelTask)
		}
	}
}

// The three spellings the rule refuses, and the one it does not. "The group half is not
// read, so nonroot:0 is accepted."
func TestTheAccountsTheNonRootRuleRefuses(t *testing.T) {
	for _, c := range []struct {
		user    string
		refused bool
	}{
		{user: "root", refused: true},
		{user: "0", refused: true},
		{user: "0:0", refused: true},
		{user: "root:root", refused: true},
		{user: "nonroot:0", refused: false},
		{user: "65532:65532", refused: false},
		{user: "nonroot", refused: false},
		{user: "", refused: false},
	} {
		if got := declaresRootUser(c.user); got != c.refused {
			t.Errorf("user %q: refused %v, want %v", c.user, got, c.refused)
		}
	}
}

// An image carrying no manifest is a base image, which is what "running a script instead
// of a brick" runs in. It is not a failure and it is remembered as such, so that a base
// image used by every script step of a run is opened once.
func TestAnImageCarryingNoManifestIsABaseImage(t *testing.T) {
	const ref = "docker.io/library/alpine@" + imageDigest
	cli, _ := withDaemon(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {
			Digest: imageDigest,
			Config: docker.ImageConfig{User: "nonroot"},
		}},
	}))

	cache := newManifests()
	got, err := resolveImage(t.Context(), cli, cache, imageTask(ref), "", nil)
	if err != nil {
		t.Fatalf("an image with no manifest was refused: %s", err)
	}
	if got.Manifest != nil {
		t.Error("a manifest was read out of an image that carries none")
	}
	if got.User != "nonroot" {
		t.Errorf("the account is %q, and with no manifest it is the image's own", got.User)
	}
	if _, none, known := cache.lookup(imageDigest); !known || !none {
		t.Error("an image known to carry no manifest was not remembered as one")
	}
}

// "a failed pull is an error object inside a 200 that already streamed half its layers",
// and it is charged to the platform: the image was not reached, so nothing in it failed.
func TestAPullThatDiedIsChargedToThePlatform(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	// Remote, so that the image is not already held and the pull is reached at all.
	cli, _ := withDaemon(t, dockertest.PullFailsHalfway, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest, Layers: 6, Manifest: []byte(goodManifest), Remote: true}},
	}))

	_, err := resolveImage(t.Context(), cli, newManifests(), imageTask(ref), "", nil)
	if err == nil {
		t.Fatal("a pull that died was reported as a pull that worked")
	}
	if !errors.Is(err, ErrImagePullFailed) {
		t.Errorf("the failure is %q, and it does not read as a pull that died", err)
	}
	charge, decided := Charged(err)
	if !decided || charge != ChargePlatform {
		t.Errorf("the failure is charged to %s, and a pull that died is the runner's", charge)
	}
}

// A daemon that is not there is not a failed brick, and the refusal says so.
func TestADaemonThatVanishedDuringAPullIsNotAFailedBrick(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	cli, daemon := withDaemon(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest}},
	}))
	daemon.Close()

	_, err := resolveImage(t.Context(), cli, newManifests(), imageTask(ref), "", nil)
	if err == nil {
		t.Fatal("a pull against a daemon that is gone succeeded")
	}
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("the failure is %q, and it does not read as a daemon that could not be reached", err)
	}
	if !unreachable(err) {
		t.Errorf("%q is not recognised as the daemon being unreachable", err)
	}
}

// The container a manifest is read through is removed, so that reading a manifest leaves
// nothing behind on the host.
func TestTheContainerAManifestIsReadThroughIsRemoved(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	cli, daemon := withDaemon(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(goodManifest)}},
	}))

	if _, err := resolveImage(t.Context(), cli, newManifests(), imageTask(ref), "", nil); err != nil {
		t.Fatalf("resolving: %s", err)
	}

	created := daemon.Created()
	if len(created) != 1 {
		t.Fatalf("%d containers were created to read one manifest", len(created))
	}
	if created[0].Labels[labelManifestRead] != imageDigest {
		t.Errorf("the reader container carries %v, and it is marked with the digest it read", created[0].Labels)
	}
	removed := daemon.Removed()
	if len(removed) != 1 || removed[0] != created[0].ID {
		t.Errorf("removed %v, and the reader container is %s", removed, created[0].ID)
	}
}

// A step that names no image names no container either, and the refusal says which step.
func TestAStepWithNoImageIsRefusedByName(t *testing.T) {
	cli, _ := withDaemon(t)

	_, err := resolveImage(t.Context(), cli, newManifests(), imageTask(""), "", nil)
	if err == nil {
		t.Fatal("a step naming no image was accepted")
	}
	if !strings.Contains(err.Error(), "step fetch") {
		t.Errorf("the refusal is %q, and it does not name the step", err)
	}
}

// An image the daemon already holds is not pulled again, which is what docker run itself
// does and what agk run --local depends on: a brick built on the machine and never pushed
// has no registry behind it, and a pull of it is answered "pull access denied" by a
// registry that has never heard of the name.
func TestAnImageTheDaemonAlreadyHoldsIsNotPulled(t *testing.T) {
	// The reference names no registry that has it: the map is what the daemon holds,
	// and PullFailsHalfway is what any pull attempted here would run into.
	const ref = "agk-invoice:built-here"
	cli, _ := withDaemon(t, dockertest.PullFailsHalfway, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {
			Digest:   imageDigest,
			Manifest: []byte(goodManifest),
		}},
	}))

	got, err := resolveImage(t.Context(), cli, newManifests(), imageTask(ref), "", nil)
	if err != nil {
		t.Fatalf("an image the daemon already holds was refused: %s", err)
	}
	if got.Digest != imageDigest {
		t.Errorf("the image resolved to %s, want %s", got.Digest, imageDigest)
	}
	if got.Manifest == nil {
		t.Error("the manifest was not read out of an image the daemon already holds")
	}
	if got.PullMillis != 0 {
		t.Errorf("the pull took %dms, and an image already held is not pulled", got.PullMillis)
	}
}

// taskContainers are the containers created for a task, as opposed to the throwaway one a
// manifest is read through.
func taskContainers(d *dockertest.Daemon) int {
	n := 0
	for _, c := range d.Created() {
		if c.Labels[LabelTask] != "" {
			n++
		}
	}
	return n
}

// "A runner refuses a message whose image is not a digest; agk run --local keeps accepting
// tags." The refusal comes before anything is asked of the host, and it is the platform's:
// a task message naming a tag is the control plane's doing, never the brick's.
func TestATagIsRefusedUnderTheDigestFloorAndRunWhereItIsLifted(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request:1.4.0"
	r := newRunner(t, oneImage(ref, goodManifest), nil)

	_, err := r.Run(t.Context(), oneTask(ref))
	if !errors.Is(err, ErrImageNotByDigest) {
		t.Fatalf("a tag was run under the digest floor, or refused for another reason: %v", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a message naming a tag is the platform's", charge)
	}
	if created := r.daemon.Created(); len(created) != 0 {
		t.Errorf("%d containers were created for a task refused before anything is", len(created))
	}

	r.cfg.Policy.RequireDigest = DigestLifted
	result, err := r.Run(t.Context(), oneTask(ref))
	if err != nil {
		t.Fatalf("a tag was refused where the floor is lifted: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Errorf("the state is %s", result.State)
	}
}

// "On a server, a non-script step whose image has no /agk/brick.yaml is refused rather than
// run as the image's own account, root included." A script step runs in the same image,
// which is what a base image is for, and agk run --local reads every brick's manifest
// before anything runs, so it lifts the floor.
func TestABrickWithNoManifestIsRefusedOnAServer(t *testing.T) {
	const ref = "ghcr.io/agentiik/base@" + imageDigest
	images := map[string]dockertest.Image{ref: {Digest: imageDigest, Config: docker.ImageConfig{User: "root"}}}
	r := newRunner(t, images, nil)

	_, err := r.Run(t.Context(), oneTask(ref))
	if !errors.Is(err, ErrContractBroken) || !strings.Contains(err.Error(), "carries no /agk/brick.yaml") {
		t.Fatalf("a brick with no manifest was not refused as one: %v", err)
	}
	if n := taskContainers(r.daemon); n != 0 {
		t.Errorf("%d containers were created for a brick with no manifest, which would run as root", n)
	}

	script := oneTask(ref)
	script.ID = agk.NewTaskID("01JMZ8V1P9C4", "fetch", 2, agk.Shard{})
	script.Attempt = 2
	script.Script = []string{"true"}
	if _, err := r.Run(t.Context(), script); err != nil {
		t.Errorf("a script step was refused the base image it runs in: %s", err)
	}

	r.cfg.Policy.RequireDigest = DigestLifted
	lifted := oneTask(ref)
	lifted.ID = agk.NewTaskID("01JMZ8V1P9C4", "fetch", 3, agk.Shard{})
	lifted.Attempt = 3
	if _, err := r.Run(t.Context(), lifted); err != nil {
		t.Errorf("an image with no manifest was refused where the floor is lifted: %s", err)
	}
}

// A pull still running at the task's deadline is cut short, and the task ends timed_out
// with no container, as one whose grant could not be redeemed before it does: an ending
// and not an error, since nothing failed but the clock. The log says why, the observer is
// told how long the pull ran, and the key is written down as ended.
func TestAPullPastTheDeadlineEndsTimedOutWithNoContainer(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	images := map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(goodManifest), Remote: true, Layers: 4}}
	r := newRunner(t, images, nil, dockertest.SlowPull(time.Second))
	logs := &memLogs{}
	r.cfg.Logs = logs

	task := oneTask(ref)
	task.Deadline = time.Now().Add(300 * time.Millisecond)
	started := time.Now()
	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("a pull past the deadline answered an error rather than an ending: %s", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the task took %s, and the pull is cut short at its deadline", took)
	}
	if result.State != agk.TaskTimedOut {
		t.Errorf("the state is %s, want timed_out", result.State)
	}
	if n := len(r.daemon.Created()); n != 0 {
		t.Errorf("%d containers were created for a task whose deadline passed during the pull", n)
	}

	var ended *Event
	for _, e := range r.observed.es {
		if e.State == agk.TaskTimedOut {
			ended = &e
		}
	}
	switch {
	case ended == nil:
		t.Fatalf("the observer was told %v, and never of the ending", r.observed.states())
	case ended.Usage.ImagePullMS < 250:
		t.Errorf("the pull ran %dms by the usage, and it ran until the deadline", ended.Usage.ImagePullMS)
	case !ended.StartedAt.IsZero():
		t.Error("the ending says a container started")
	}
	if log := logs.String(); !strings.Contains(log, "deadline passed while its image") {
		t.Errorf("the log says %q, and not why no container ran", log)
	}

	var completed *Completed
	if err := r.Hold(task.ID); !errors.As(err, &completed) || completed.Ending.State != agk.TaskTimedOut {
		t.Errorf("the key is held as %v, and it ended timed_out", err)
	}
}

// A pull inside its deadline is not cut short, and how long it took is the usage's
// image_pull_ms, as it is where no deadline bounds it.
func TestAPullInsideItsDeadlineIsMeasured(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	images := map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(goodManifest), Remote: true, Layers: 2}}
	r := newRunner(t, images, nil, dockertest.SlowPull(100*time.Millisecond))

	task := oneTask(ref)
	task.Deadline = time.Now().Add(time.Minute)
	result, err := r.Run(t.Context(), task)
	if err != nil {
		t.Fatalf("running: %s", err)
	}
	if result.State != agk.TaskSucceeded {
		t.Fatalf("the state is %s", result.State)
	}
	var pulled int64
	for _, e := range r.observed.es {
		if e.State.Terminal() {
			pulled = e.Usage.ImagePullMS
		}
	}
	if pulled < 200 || pulled > 10000 {
		t.Errorf("the pull took %dms by the usage, and two layers at 100ms each take at least 200", pulled)
	}
}

// "A registry's 401 fails the task on the platform's account and names v0.8.0." A runner
// pulls with no credentials, so the refusal says where credentials will come from rather
// than leaving somebody to look for a setting that does not exist.
func TestARegistrys401IsChargedToThePlatformAndNamesV080(t *testing.T) {
	const ref = "registry.example/acme/private@" + imageDigest
	cli, _ := withDaemon(t, dockertest.PullAnswers401, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(goodManifest), Remote: true}},
	}))

	_, err := resolveImage(t.Context(), cli, newManifests(), imageTask(ref), "", nil)
	if !errors.Is(err, ErrImagePullFailed) {
		t.Fatalf("a pull the registry refused reads as %v", err)
	}
	if charge, decided := Charged(err); !decided || charge != ChargePlatform {
		t.Errorf("the refusal is charged to %s, and a pull the registry refused is the platform's", charge)
	}
	for _, want := range []string{"401 Unauthorized", "v0.8.0", "namespace secrets"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}

// A container an earlier delivery left is adopted whatever the deadline says, since only the
// pull is bounded by it: a deadline already past ends the adopted container through the
// watch, which stops it and removes it, and never through the ending of a task whose pull
// ran out of time, which would leave the container where it is.
func TestAnAdoptedContainerPastItsDeadlineIsNotLeftBehind(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t, oneImage(ref, goodManifest), func(c dockertest.Container) (int, error) {
		<-c.Signalled()
		return 137, nil
	})

	task := oneTask(ref)
	task.Deadline = time.Now().Add(-time.Second)
	container, _ := stageFirstDelivery(t, r, task)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := r.Run(ctx, task)
	if err != nil {
		t.Fatalf("the redelivered task: %s", err)
	}
	if result.State != agk.TaskTimedOut {
		t.Errorf("the state is %s, and the container was stopped at its deadline", result.State)
	}
	if !slices.Contains(r.daemon.Removed(), container) {
		t.Errorf("the adopted container %s was left behind", container[:12])
	}
}
