package docker_test

import (
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
