package driver

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// The manifest Manifest is asked for: one that declares its ports, which is what agk
// validate holds a step's outputs to.
const portedManifest = `apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: http-request
  version: 1.4.0
spec:
  inputs:
    in:
      schema: { type: object }
  outputs:
    out:
      schema: { type: object }
    rejected:
      schema: { type: object }
  runtime:
    user: "65532:65532"
`

// TestTheManifestOfAnImageIsReadWithoutRunningOne is what agk validate rests on: the ports
// of a brick, read off the image, with no container started.
func TestTheManifestOfAnImageIsReadWithoutRunningOne(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t,
		map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(portedManifest)}},
		func(dockertest.Container) (int, error) {
			t.Error("a container ran, and reading a manifest starts nothing")
			return 0, nil
		})

	m, err := r.Manifest(t.Context(), "fetch", ref)
	if err != nil {
		t.Fatalf("reading the manifest of %s: %v", ref, err)
	}
	if m.Metadata.Name != "http-request" || m.Metadata.Version != "1.4.0" {
		t.Errorf("the manifest is of %s %s", m.Metadata.Name, m.Metadata.Version)
	}
	if got, want := len(m.OutputPorts()), 2; got != want {
		t.Errorf("the manifest carries %d output ports and it declares %d", got, want)
	}
	if _, ok := m.Port("in"); !ok {
		t.Errorf("the input port the manifest declares was not read")
	}
}

// TestTheManifestIsReadOncePerImageHoweverManyStepsAskForIt is why this sits on the
// driver's own cache: a workflow naming one brick in four steps pulls and reads once, and
// the read that warmed the cache is the one the run that follows uses.
func TestTheManifestIsReadOncePerImageHoweverManyStepsAskForIt(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	r := newRunner(t,
		map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(portedManifest)}},
		func(dockertest.Container) (int, error) { return 0, nil })

	var reads atomic.Int64
	r.daemon.Handle("GET", "/containers/{id}/archive", func(w http.ResponseWriter, req *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		w.Write(tarOf(t, portedManifest))
	})

	for _, step := range []agk.Step{"fetch", "retry", "verify", "archive"} {
		if _, err := r.Manifest(t.Context(), step, ref); err != nil {
			t.Fatalf("step %s: %v", step, err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("%s was read %d times, and a manifest is a property of the image: it is read on first pull and cached by digest", brick.ManifestPath, got)
	}
}

// TestAnImageCarryingNoManifestIsRefusedAndNotReportedAbsent: graph.Images names the image
// of a non-script step, so there is no base image to be tolerant of here.
func TestAnImageCarryingNoManifestIsRefusedAndNotReportedAbsent(t *testing.T) {
	const ref = "alpine:3.21"
	r := newRunner(t,
		map[string]dockertest.Image{ref: {Digest: imageDigest}},
		func(dockertest.Container) (int, error) { return 0, nil })

	_, err := r.Manifest(t.Context(), "fetch", ref)
	if err == nil {
		t.Fatal("an image with no manifest answered with one")
	}
	if !errors.Is(err, ErrContractBroken) {
		t.Errorf("the refusal is %v, and an image that is not a brick breaks the contract", err)
	}
	var f *Fault
	if !errors.As(err, &f) || f.Step != "fetch" {
		t.Errorf("the refusal does not name the step: %v", err)
	}
	for _, want := range []string{brick.ManifestPath, ref} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestAManifestDeclaringRootIsRefusedWhereItIsRead, which is the same rule resolveImage
// applies before a container exists, reached through the read agk validate makes.
func TestAManifestDeclaringRootIsRefusedWhereItIsRead(t *testing.T) {
	const ref = "ghcr.io/agentiik/careless@" + imageDigest
	r := newRunner(t,
		map[string]dockertest.Image{ref: {Digest: imageDigest, Manifest: []byte(rootManifest)}},
		func(dockertest.Container) (int, error) { return 0, nil })

	_, err := r.Manifest(t.Context(), "fetch", ref)
	if err == nil {
		t.Fatal("a manifest declaring root was handed back")
	}
	t.Logf("refused: %v", err)
}
