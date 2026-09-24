package driver

import (
	"errors"
	"strings"
	"testing"

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

// An image built on the machine and never pushed is refused naming it, on either store:
// the classic one holds it under no registry digest at all, and the containerd one under a
// digest its registry does not serve.
func TestAnImageBuiltHereAndNeverPushedIsRefusedNamingIt(t *testing.T) {
	for _, c := range []struct {
		store string
		bs    []dockertest.Behaviour
	}{
		{"containerd", nil},
		{"classic", []dockertest.Behaviour{dockertest.ClassicImageStore}},
	} {
		t.Run(c.store, func(t *testing.T) {
			r := newRunner(t, map[string]dockertest.Image{
				"ghcr.io/acme/agk-local:0.1.0": {Digest: imageDigest, Unpushed: true},
			}, nil, c.bs...)

			pinned, err := r.Pin(t.Context(), "fetch", "ghcr.io/acme/agk-local:0.1.0")
			if !errors.Is(err, ErrNotPushed) {
				t.Fatalf("an image never pushed was pinned to %q: %v", pinned, err)
			}
			if charge, _ := Charged(err); charge != ChargeBrick {
				t.Errorf("an image never pushed is charged to %s", charge)
			}
			for _, want := range []string{"fetch", "ghcr.io/acme/agk-local:0.1.0", "never pushed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
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
