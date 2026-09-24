package driver

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What agk push records a tag as: "a tag is a mutable pointer, and a commit must determine
// what ran", so a version names each image by the digest its registry serves, once.

const registryDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// A tag is pinned to its repository, as the workflow spells it, at the digest the daemon
// holds the image under there, which the registry has said it serves.
func TestATagIsPinnedToTheDigestItsRegistryServes(t *testing.T) {
	r := newRunner(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: imageDigest},
	}, nil)

	pinned, err := r.Pin(t.Context(), "fetch", "ghcr.io/acme/agk-invoice:1.4.0")
	if err != nil {
		t.Fatalf("pinning a pushed image: %v", err)
	}
	if want := "ghcr.io/acme/agk-invoice@" + imageDigest; pinned != want {
		t.Errorf("the tag is pinned to %s, want %s", pinned, want)
	}
}

// The image store Docker had before containerd's holds an image by the digest of its
// configuration and its registry's manifest under another. The first names nothing a
// registry serves, so a version recording it would name an image no runner can pull.
func TestTheClassicStorePinsToTheRegistrysDigestAndNotTheImagesOwn(t *testing.T) {
	r := newRunner(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: imageDigest, RegistryDigest: registryDigest},
	}, nil, dockertest.ClassicImageStore)

	pinned, err := r.Pin(t.Context(), "fetch", "ghcr.io/acme/agk-invoice:1.4.0")
	if err != nil {
		t.Fatalf("pinning a pushed image: %v", err)
	}
	if want := "ghcr.io/acme/agk-invoice@" + registryDigest; pinned != want {
		t.Errorf("the tag is pinned to %s, want %s", pinned, want)
	}
}

// An image the daemon does not hold is pulled first, as it is for its manifest, so that
// what is pinned is what the push reads.
func TestAnImageTheDaemonDoesNotHoldIsPulledBeforeItIsPinned(t *testing.T) {
	r := newRunner(t, map[string]dockertest.Image{
		"alpine:3.21": {Digest: imageDigest, Remote: true},
	}, nil)

	pinned, err := r.Pin(t.Context(), "fetch", "alpine:3.21")
	if err != nil {
		t.Fatalf("pinning an image the daemon did not hold: %v", err)
	}
	if want := "alpine@" + imageDigest; pinned != want {
		t.Errorf("the tag is pinned to %s, want %s", pinned, want)
	}
}

// An image built on the machine and never pushed is refused naming it, on either store and
// whichever way its registry says so. The classic store holds it under no registry digest
// at all. The containerd store holds it under a digest its registry does not serve, and the
// registry says that with 404 where it holds other images of the repository, and otherwise,
// which is the common case, with 403 or 401, since it will not tell somebody with no
// credentials whether a repository exists.
func TestAnImageBuiltHereAndNeverPushedIsRefusedNamingIt(t *testing.T) {
	const local = "ghcr.io/acme/agk-local:0.1.0"
	for _, c := range []struct {
		name   string
		images map[string]dockertest.Image
		bs     []dockertest.Behaviour
		said   string
	}{
		{"a repository the registry holds nothing of", nil, nil, "403"},
		{"a registry that answers 401", nil, []dockertest.Behaviour{dockertest.RegistryAnswers401}, "401"},
		{"a repository the registry holds other images of", map[string]dockertest.Image{
			"ghcr.io/acme/agk-local:0.0.9": {Digest: registryDigest},
		}, nil, "404"},
		{"the classic store", nil, []dockertest.Behaviour{dockertest.ClassicImageStore}, "under no digest of its registry"},
	} {
		t.Run(c.name, func(t *testing.T) {
			images := map[string]dockertest.Image{local: {Digest: imageDigest, Unpushed: true}}
			maps.Copy(images, c.images)
			r := newRunner(t, images, nil, c.bs...)

			pinned, err := r.Pin(t.Context(), "fetch", local)
			if !errors.Is(err, ErrNotPushed) {
				t.Fatalf("an image never pushed was pinned to %q: %v", pinned, err)
			}
			if charge, _ := Charged(err); charge != ChargeBrick {
				t.Errorf("an image never pushed is charged to %s", charge)
			}
			for _, want := range []string{"fetch", local, "never pushed", c.said} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// A registry answering a question about one digest with the descriptor of another has not
// said that it serves the one this machine holds, so a version recorded under that digest
// would name a manifest nobody confirmed. The image is refused as one its registry does not
// serve, naming what the registry answered.
func TestARegistryAnsweringAnotherDigestHasNotConfirmedTheOneAsked(t *testing.T) {
	r := newRunner(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: imageDigest},
	}, nil)
	r.daemon.Handle("GET", "/distribution/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(docker.Distribution{Descriptor: docker.Descriptor{Digest: registryDigest}})
	})

	pinned, err := r.Pin(t.Context(), "fetch", "ghcr.io/acme/agk-invoice:1.4.0")
	if !errors.Is(err, ErrNotPushed) {
		t.Fatalf("a digest the registry did not confirm was pinned to %q: %v", pinned, err)
	}
	if !strings.Contains(err.Error(), registryDigest) {
		t.Errorf("the refusal does not name what the registry answered: %v", err)
	}
}

// A registry that could not be asked has not said the image is missing, so nothing is
// charged to the image: the push is refused as the platform's trouble, which is what a
// laptop off its network is.
func TestARegistryThatCannotBeAskedIsNotAnImageNeverPushed(t *testing.T) {
	r := newRunner(t, map[string]dockertest.Image{
		"ghcr.io/acme/agk-invoice:1.4.0": {Digest: imageDigest},
	}, nil, dockertest.RegistryUnreachable)

	_, err := r.Pin(t.Context(), "fetch", "ghcr.io/acme/agk-invoice:1.4.0")
	if err == nil || errors.Is(err, ErrNotPushed) {
		t.Fatalf("a registry nobody could reach answered %v", err)
	}
	if charge, _ := Charged(err); charge != ChargePlatform {
		t.Errorf("a registry nobody could reach is charged to %s", charge)
	}
}
