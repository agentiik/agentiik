package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// registry starts a registry:2 on 127.0.0.1, the one name this machine's daemon pushes to and
// both runners' daemons pull from, since all three share the host's network. A registry on a
// loopback address is one every daemon speaks plain HTTP to without being told, which spares the
// daemons a certificate and a flag each.
func (in *Installation) registry(ctx context.Context) {
	port := freePort(in.t)
	in.Registry = fmt.Sprintf("127.0.0.1:%d", port)
	name := in.id + "-registry"
	in.container(ctx, name, "run", "-d", "--name", name, "--label", in.label(), "--network", "host",
		"-e", "REGISTRY_HTTP_ADDR="+in.Registry, registryImage)
	eventually(in.t, time.Minute, "the registry answered", func() error {
		answer, err := http.Get("http://" + in.Registry + "/v2/")
		if err != nil {
			return err
		}
		answer.Body.Close()
		if answer.StatusCode != http.StatusOK {
			return fmt.Errorf("it answered %d", answer.StatusCode)
		}
		return nil
	})
}

// Brick builds the brick under testdata/bricks/<dir>, pushes it to the registry, and answers the
// reference it was tagged as and the digest the registry holds it under, name@sha256:<hex>, which
// is what a version records and what a runner is handed.
//
// Built by this machine's daemon and pushed, as a brick author's machine does it: the runners'
// daemons have never seen it, and pull it by its digest from the registry, which is the path a
// server run takes.
func (in *Installation) Brick(dir string) (tag, pinned string) {
	in.t.Helper()
	ctx := in.t.Context()
	repository := in.Registry + "/agk-e2e/" + dir
	tag = repository + ":" + in.id
	// No provenance: an attestation would make what is pushed an index of two manifests, and a
	// brick is one image.
	if _, err := docker(ctx, "build", "--provenance=false", "-t", tag, filepath.Join(in.module, "e2e", "testdata", "bricks", dir)); err != nil {
		in.t.Fatal(err)
	}
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "image", "rm", "-f", tag)
	})
	if _, err := docker(ctx, "push", tag); err != nil {
		in.t.Fatal(err)
	}
	out, err := docker(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", tag)
	if err != nil {
		in.t.Fatal(err)
	}
	var digests []string
	if err := json.Unmarshal([]byte(out), &digests); err != nil {
		in.t.Fatalf("the digests of %s do not decode: %s: %s", tag, err, out)
	}
	for _, d := range digests {
		if strings.HasPrefix(d, repository+"@sha256:") {
			return tag, d
		}
	}
	in.t.Fatalf("%s was pushed, and this machine's daemon holds no digest for it under %s: %s", tag, repository, out)
	return "", ""
}

// runnerImage builds the runner image from build/runner.Dockerfile, around the agent and the
// helper built from this checkout, as a release builds it around the binaries it ships.
func (in *Installation) runnerImage(ctx context.Context) {
	in.runnerIm = "agk-e2e/runner:" + in.id
	if _, err := docker(ctx, "build", "--provenance=false", "-t", in.runnerIm,
		"-f", filepath.Join(in.module, "build", "runner.Dockerfile"), filepath.Join(in.bin, "image")); err != nil {
		in.t.Fatal(err)
	}
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "image", "rm", "-f", in.runnerIm)
	})
}
