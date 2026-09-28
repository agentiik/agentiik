package api_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/version"
)

// A name holding U+FFFD is refused by the push, whatever the rules every tree is held to say of it:
// JSON leaves that character where a name was not UTF-8, and a name really holding one cannot be
// told from one that lost its bytes on the way. The rule is the transport's, until the installation
// hosts the repository.
func TestAPushedNameHoldingTheReplacementCharacterIsRefused(t *testing.T) {
	if err := version.TreePath("data/caf�.csv"); err != nil {
		t.Fatalf("the rules every tree is held to refuse a name holding U+FFFD: %v", err)
	}
	_, _, err := api.CheckTree(map[string]api.PushFile{"data/caf�.csv": {Content: []byte("x"), Mode: "0644"}})
	if err == nil || !strings.Contains(err.Error(), "U+FFFD") {
		t.Fatalf("a pushed name holding U+FFFD was answered %v", err)
	}
}

// A push keys each manifest by the reference its workflow writes, and a check asks for it by the
// digest a step runs: two tags pinned to one digest are one image, answered once, and a push
// carrying two different manifests for it is refused rather than one of them chosen.
func TestTheManifestsAPushCarriesAreAskedForByDigest(t *testing.T) {
	const digest = "ghcr.io/acme/agk-invoice@sha256:8214cabcb148ac56e69ee08e1554684f7609d08b58c4527938fdcf400be68595"
	p := api.Push{
		Images:    map[string]string{"ghcr.io/acme/agk-invoice:1.4": digest, "ghcr.io/acme/agk-invoice:1.4.0": digest},
		Manifests: map[string][]byte{"ghcr.io/acme/agk-invoice:1.4": []byte("one"), "ghcr.io/acme/agk-invoice:1.4.0": []byte("one")},
	}
	body, err := api.ManifestsCarried(p)(t.Context(), digest, "invoice")
	if err != nil || string(body) != "one" {
		t.Fatalf("the manifest of one image under two tags was answered %q, %v", body, err)
	}
	if _, err := api.ManifestsCarried(p)(t.Context(), "ghcr.io/acme/agk-other@sha256:"+strings.Repeat("0", 64), "other"); !errors.Is(err, version.ErrNotHeld) {
		t.Errorf("an image the push carries no manifest for was answered %v", err)
	}

	p.Manifests["ghcr.io/acme/agk-invoice:1.4.0"] = []byte("two")
	if _, err := api.ManifestsCarried(p)(t.Context(), digest, "invoice"); err == nil || !strings.Contains(err.Error(), "two manifests") {
		t.Errorf("two manifests for one image were answered %v", err)
	}
}
