package agk_test

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/fixtures"
)

const aDigest = "sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

// What names an image by digest is what the wire's imageRef accepts, reference for reference,
// read out of the vendored document rather than copied from it: a push that let through what a
// task message cannot carry would record a version no runner may run.
func TestAnImageByDigestIsWhatTheWireAccepts(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Defs struct {
			ImageRef struct {
				Pattern string `json:"pattern"`
			} `json:"imageRef"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Defs.ImageRef.Pattern == "" {
		t.Fatal("the vendored wire carries no imageRef pattern")
	}
	accepted := regexp.MustCompile(wire.Defs.ImageRef.Pattern)

	for _, ref := range []string{
		"ghcr.io/acme/agk-invoice@" + aDigest,
		"alpine@" + aDigest,
		"alpine:3.21@" + aDigest,
		"registry.example:5000/acme/brick@" + aDigest,
		"ghcr.io/acme/agk-invoice:1.4.0",
		"alpine",
		"alpine@sha256:1ab74e",
		"alpine@sha512:" + aDigest[len("sha256:"):] + aDigest[len("sha256:"):],
		"alpine@SHA256:" + aDigest[len("sha256:"):],
		"alpine@" + aDigest + "\n",
		"two words@" + aDigest,
		"@" + aDigest,
	} {
		if got, want := agk.ImageByDigest(ref), accepted.MatchString(ref); got != want {
			t.Errorf("%q names its image by digest: %t, and the wire says %t", ref, got, want)
		}
	}
}

// A repository is a reference less its tag and its digest, and a registry's port is neither.
func TestTheRepositoryOfAReference(t *testing.T) {
	for ref, want := range map[string]string{
		"alpine":                                      "alpine",
		"alpine:3.21":                                 "alpine",
		"alpine@" + aDigest:                           "alpine",
		"alpine:3.21@" + aDigest:                      "alpine",
		"ghcr.io/acme/agk-invoice:1.4.0":              "ghcr.io/acme/agk-invoice",
		"registry.example:5000/acme/brick":            "registry.example:5000/acme/brick",
		"registry.example:5000/acme/brick:1.4.0":      "registry.example:5000/acme/brick",
		"registry.example:5000/acme/brick@" + aDigest: "registry.example:5000/acme/brick",
	} {
		if got := agk.ImageRepository(ref); got != want {
			t.Errorf("the repository of %s is %q, want %q", ref, got, want)
		}
	}
}
