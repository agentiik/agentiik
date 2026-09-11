package driver

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

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
