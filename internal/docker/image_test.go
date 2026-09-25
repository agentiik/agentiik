package docker_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// TestAPullThatDiedIsNotAPullThatWorked is the reason ImagePull reads every message
// rather than the status. The daemon commits to a 200 before it knows whether the pull
// will work, so a failure arrives part way through a stream that has already reported
// half its layers as complete.
func TestAPullThatDiedIsNotAPullThatWorked(t *testing.T) {
	client, _ := dial(t, dockertest.PullFailsHalfway, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{
			"ghcr.io/agentiik/brick@" + digest: {Digest: digest, Layers: 6},
		},
	}))

	var seen []docker.Progress
	err := client.ImagePull(ctxOf(t), "ghcr.io/agentiik/brick@"+digest, "", func(p docker.Progress) {
		seen = append(seen, p)
	})
	if err == nil {
		t.Fatal("a pull that died was reported as a pull that worked")
	}
	if !strings.Contains(err.Error(), "failed to register layer") {
		t.Errorf("the failure is %q, and it does not carry what the daemon said", err)
	}
	if len(seen) < 2 {
		t.Errorf("read %d progress messages, and the stream had reported layers before it failed", len(seen))
	}
}

// TestEveryProgressMessageIsRead holds the other half: a pull that worked streams its
// whole progress and ends without an error.
func TestEveryProgressMessageIsRead(t *testing.T) {
	const ref = "ghcr.io/agentiik/brick@" + digest
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {Digest: digest, Layers: 3}},
	}))

	var seen int
	if err := client.ImagePull(ctxOf(t), ref, "", func(docker.Progress) { seen++ }); err != nil {
		t.Fatalf("pulling: %v", err)
	}
	if seen < 3 {
		t.Errorf("read %d progress messages of a pull of three layers", seen)
	}
}

// TestAnImageTheRegistryDoesNotHaveIsRefused holds that a missing image is a refusal
// with a status and not a stream.
func TestAnImageTheRegistryDoesNotHaveIsRefused(t *testing.T) {
	client, _ := dial(t)

	err := client.ImagePull(ctxOf(t), "ghcr.io/agentiik/absent@"+digest, "", nil)
	if err == nil {
		t.Fatal("pulling an image that does not exist succeeded")
	}
	if !docker.IsNotFound(err) {
		t.Errorf("%v does not read as a 404", err)
	}
}

// TestAReferenceIsTakenApartTheWayTheCreateWantsIt holds the one wire detail of a pull
// that is easy to get wrong: a digest travels in the tag position, and a registry
// written with a port carries a colon that is not a tag.
func TestAReferenceIsTakenApartTheWayTheCreateWantsIt(t *testing.T) {
	for _, c := range []struct{ ref, resolved string }{
		{ref: "brick@" + digest, resolved: "brick@" + digest},
		{ref: "ghcr.io/agentiik/brick@" + digest, resolved: "ghcr.io/agentiik/brick@" + digest},
		{ref: "registry.example:5000/brick@" + digest, resolved: "registry.example:5000/brick@" + digest},
		{ref: "registry.example:5000/brick:1.4.0", resolved: "registry.example:5000/brick:1.4.0"},
		{ref: "alpine", resolved: "alpine"},
	} {
		t.Run(c.ref, func(t *testing.T) {
			// The fake daemon puts the reference back together from the two
			// parameters, so a reference that survives the round trip is one
			// that was taken apart correctly.
			client, _ := dial(t, dockertest.With(dockertest.Options{
				Images: map[string]dockertest.Image{c.resolved: {Digest: digest, Layers: 1}},
			}))
			if err := client.ImagePull(ctxOf(t), c.ref, "", nil); err != nil {
				t.Fatalf("pulling %s: %v", c.ref, err)
			}
		})
	}
}

// TestAnInspectIsWhereAReferenceBecomesADigest holds what the driver resolves an image
// with.
func TestAnInspectIsWhereAReferenceBecomesADigest(t *testing.T) {
	const ref = "ghcr.io/agentiik/brick@" + digest
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{ref: {
			Digest: digest,
			Config: docker.ImageConfig{User: "65532:65532"},
		}},
	}))

	img, err := client.ImageInspect(ctxOf(t), ref)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if img.ID != digest {
		t.Errorf("Id: got %q, want the digest the reference resolved to", img.ID)
	}
	if img.Config.User != "65532:65532" {
		t.Errorf("Config.User: got %q, want the account the image declares", img.Config.User)
	}
}

// TestAnImageIsPinnedUnderItsOwnRepository holds what a reference may be pinned to: the
// digests the daemon holds the image under in the repository the reference names, however
// either of them spells it, and never one held under another repository.
func TestAnImageIsPinnedUnderItsOwnRepository(t *testing.T) {
	const other = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	img := docker.Image{RepoDigests: []string{
		"alpine@" + digest,
		"ghcr.io/acme/alpine@" + other,
		"registry.example:5000/acme/brick@" + other,
		"Registry/acme/brick@" + digest,
		"alpine@sha256:abc",
		"no-digest-at-all",
	}}
	for ref, want := range map[string][]string{
		"alpine:3.21":                      {"alpine@" + digest},
		"alpine":                           {"alpine@" + digest},
		"docker.io/library/alpine:3.21":    {"docker.io/library/alpine@" + digest},
		"index.docker.io/library/alpine":   {"index.docker.io/library/alpine@" + digest},
		"library/alpine:3.21":              {"library/alpine@" + digest},
		"ghcr.io/acme/alpine:3.21":         {"ghcr.io/acme/alpine@" + other},
		"registry.example:5000/acme/brick": {"registry.example:5000/acme/brick@" + other},
		"Registry/acme/brick:1":            {"Registry/acme/brick@" + digest},
		"registry/acme/brick:1":            nil,
		"acme/alpine:3.21":                 nil,
		"ghcr.io/acme/other:1":             nil,
	} {
		if got := img.RegistryDigests(ref); !slices.Equal(got, want) {
			t.Errorf("%s may be pinned to %q, want %q", ref, got, want)
		}
	}
	if got := (docker.Image{}).RegistryDigests("alpine:3.21"); got != nil {
		t.Errorf("an image held under no digest may be pinned to %q", got)
	}
}

// TestTheRegistryIsAskedWhatItServes holds the question a push puts to the registry
// behind an image, through the daemon, and the answers it can get: the manifest, a
// manifest the registry does not hold in a repository it does, and a repository it holds
// nothing of, which it will not talk about, with 403 or with 401 depending on the
// registry. The last two are what an image built on a machine and never pushed gets.
func TestTheRegistryIsAskedWhatItServes(t *testing.T) {
	const otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	images := dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{
			"ghcr.io/acme/brick:1.4.0":     {Digest: digest},
			"ghcr.io/acme/brick:1.5.0-dev": {Digest: otherDigest, Unpushed: true},
			"ghcr.io/acme/local:1.0.0":     {Digest: digest, Unpushed: true},
		},
	})
	client, _ := dial(t, images)

	d, err := client.DistributionInspect(ctxOf(t), "ghcr.io/acme/brick@"+digest, "")
	if err != nil {
		t.Fatalf("asking about a pushed image: %v", err)
	}
	if d.Descriptor.Digest != digest {
		t.Errorf("the registry serves %q, want %q", d.Descriptor.Digest, digest)
	}

	_, err = client.DistributionInspect(ctxOf(t), "ghcr.io/acme/brick@"+otherDigest, "")
	if !docker.IsNotFound(err) {
		t.Errorf("an image never pushed to a repository the registry holds answered %v, and the registry holds no such manifest", err)
	}
	_, err = client.DistributionInspect(ctxOf(t), "ghcr.io/acme/local@"+digest, "")
	if !docker.IsDenied(err) || answered(err) != 403 {
		t.Errorf("an image never pushed to a repository the registry never heard of answered %v", err)
	}

	client, _ = dial(t, images, dockertest.RegistryAnswers401)
	_, err = client.DistributionInspect(ctxOf(t), "ghcr.io/acme/local@"+digest, "")
	if !docker.IsDenied(err) || answered(err) != 401 {
		t.Errorf("a registry answering 401 for a repository it never heard of answered %v", err)
	}
}

// answered is the status a refusal carries, so that a test holding a registry to 401 or
// to 403 is not satisfied by IsDenied answering for the other.
func answered(err error) int {
	var refused *docker.Error
	if errors.As(err, &refused) {
		return refused.Status
	}
	return 0
}

// TestOnlyTheClassicStoreSaysAnImageWasNeverPushed holds why the registry is asked at all:
// the containerd store holds an image built on the machine under a digest of its own
// repository exactly as it holds one it pulled, and only the classic store leaves it with
// none.
func TestOnlyTheClassicStoreSaysAnImageWasNeverPushed(t *testing.T) {
	images := dockertest.With(dockertest.Options{Images: map[string]dockertest.Image{
		"ghcr.io/acme/local:1.0.0": {Digest: digest, Unpushed: true},
	}})
	for _, c := range []struct {
		store string
		bs    []dockertest.Behaviour
		held  int
	}{
		{"containerd", []dockertest.Behaviour{images}, 1},
		{"classic", []dockertest.Behaviour{images, dockertest.ClassicImageStore}, 0},
	} {
		client, _ := dial(t, c.bs...)
		img, err := client.ImageInspect(ctxOf(t), "ghcr.io/acme/local:1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if got := len(img.RegistryDigests("ghcr.io/acme/local:1.0.0")); got != c.held {
			t.Errorf("the %s store holds an image never pushed under %d registry digests, want %d", c.store, got, c.held)
		}
	}
}

// TestAPullARegistryRefusedIsToldApartFromOneThatFailed holds IsPullDenied to the refusals
// its comment lists, to one that arrives inside the progress stream, and to failures that
// are not a registry refusing anybody: the daemon's own 403, a proxy's 407, and an image
// whose name happens to hold one of the words.
func TestAPullARegistryRefusedIsToldApartFromOneThatFailed(t *testing.T) {
	const hex = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, c := range []struct {
		name   string
		err    error
		denied bool
	}{
		{"Docker Hub", &docker.Error{Status: 404, Message: "pull access denied for acme/private, repository does not exist or may require 'docker login'"}, true},
		{"ghcr.io", &docker.Error{Status: 500, Message: "error from registry: denied\ndenied"}, true},
		{"GitLab", &docker.Error{Status: 403, Message: "error from registry: access forbidden"}, true},
		{"quay.io", &docker.Error{Status: 500, Message: `unknown: failed to resolve reference "quay.io/acme/private` + hex + `": unexpected status from HEAD request to https://quay.io/v2/acme/private/manifests/sha256:00: 401 Unauthorized`}, true},
		{"a 403 from a registry", &docker.Error{Status: 500, Message: `unknown: failed to resolve reference "registry.example/acme/private` + hex + `": unexpected status from HEAD request to https://registry.example/v2/acme/private/manifests/sha256:00: 403 Forbidden`}, true},
		{"ECR", &docker.Error{Status: 500, Message: "Head \"https://123456789012.dkr.ecr.eu-west-3.amazonaws.com/v2/acme/private/manifests/sha256:00\": no basic auth credentials"}, true},
		{"Artifact Registry", &docker.Error{Status: 500, Message: "Head \"https://europe-docker.pkg.dev/v2/acme/images/private/manifests/sha256:00\": denied: Unauthenticated request. Unauthenticated requests do not have permission \"artifactregistry.repositories.downloadArtifacts\""}, true},
		{"a 401 with no words", &docker.Error{Status: 401}, true},
		{"in the stream", errors.New("pulling ghcr.io/acme/private" + hex + ": unauthorized: authentication required"), true},
		{"a disk", &docker.Error{Status: 500, Message: "failed to register layer: open /var/lib/docker/tmp: permission denied"}, false},
		{"a disk, mid-sentence", &docker.Error{Status: 500, Message: "failed to register layer: permission denied: /var/lib/docker/tmp"}, false},
		{"a manifest not found", &docker.Error{Status: 404, Message: `failed to resolve reference "mcr.microsoft.com/acme/x` + hex + `": not found`}, false},
		{"a layer cut short", errors.New("pulling ghcr.io/acme/brick" + hex + ": failed to register layer: unexpected EOF"), false},
		{"a layer cut short, of an image named for the word", errors.New("pulling ghcr.io/acme/unauthorized-api" + hex + ": failed to register layer: unexpected EOF"), false},
		{"a tag named for the word", &docker.Error{Status: 500, Message: `failed to resolve reference "ghcr.io/acme/brick:denied` + hex + `": connection reset by peer`}, false},
		{"an authorization plugin", &docker.Error{Status: 403, Message: "authorization denied by plugin opa-docker-authz: request rejected by administrative policy"}, false},
		{"a proxy in front of the socket", &docker.Error{Status: 403, Message: "<html><body><h1>403 Forbidden</h1>\nRequest forbidden by administrative rules.\n</body></html>"}, false},
		{"a proxy on the way to the registry", &docker.Error{Status: 500, Message: `Get "https://ghcr.io/v2/": Proxy Authentication Required`}, false},
		{"nothing", nil, false},
	} {
		if got := docker.IsPullDenied(c.err); got != c.denied {
			t.Errorf("%s: IsPullDenied(%v) is %t, want %t", c.name, c.err, got, c.denied)
		}
	}
}
