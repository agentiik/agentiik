package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// registryPort is where registry:2 listens in its container.
const registryPort = "5000"

// registry makes the installation's network and starts a registry:2 on it, whose name is the one
// every runner's daemon pulls from. The daemons resolve it through the network's own DNS and speak
// plain HTTP to it because each is told to with --insecure-registry, which spares them a
// certificate. Its port is published on 127.0.0.1 too, for this machine to see it answer.
func (in *Installation) registry(ctx context.Context) {
	in.network = in.id
	if _, err := docker(ctx, "network", "create", "--label", in.label(), in.network); err != nil {
		in.t.Fatal(err)
	}
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "network", "rm", in.network)
	})

	name := in.id + "-registry"
	in.Registry = name + ":" + registryPort
	in.container(ctx, name, "run", "-d", "--name", name, "--label", in.label(), "--network", in.network,
		"-p", "127.0.0.1::"+registryPort, registryImage)
	published, err := docker(ctx, "port", name, registryPort+"/tcp")
	if err != nil {
		in.t.Fatal(err)
	}
	published, _, _ = strings.Cut(published, "\n")
	eventually(in.ctx, in.t, time.Minute, "the registry answered", func() error {
		answer, err := http.Get("http://" + published + "/v2/")
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
// Built by this machine's daemon, as a brick author's machine builds it, and pushed by runner a's,
// which is a daemon that resolves the registry's name: this machine's does not. The digest is the
// one the pushing daemon holds, as agk push resolves it. The image is then taken off that daemon,
// so that neither runner holds it and the one that takes the task pulls it by that digest, which
// is the path a server run takes.
func (in *Installation) Brick(dir string) (tag, pinned string) {
	in.t.Helper()
	ctx := in.ctx
	pusher := in.Runners[0]
	local := "agk-e2e/" + dir + ":" + in.id
	repository := in.Registry + "/agk-e2e/" + dir
	tag = repository + ":" + in.id
	// No provenance: an attestation would make what is pushed an index of two manifests, and a
	// brick is one image.
	if _, err := docker(ctx, "build", "--provenance=false", "-t", local, filepath.Join(in.module, "e2e", "testdata", "bricks", dir)); err != nil {
		in.t.Fatal(err)
	}
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "image", "rm", "-f", local)
	})

	save := exec.CommandContext(ctx, "docker", "save", local)
	load := exec.CommandContext(ctx, "docker", "exec", "-i", pusher.Daemon, "docker", "-H", "unix://"+daemonSocketDir+"/docker.sock", "load")
	pipe, err := save.StdoutPipe()
	if err != nil {
		in.t.Fatal(err)
	}
	var saveErr, loadOut bytes.Buffer
	save.Stderr = &saveErr
	load.Stdin, load.Stdout, load.Stderr = pipe, &loadOut, &loadOut
	if err := save.Start(); err != nil {
		in.t.Fatal(err)
	}
	loadErr := load.Run()
	if loadErr != nil {
		// A load that ended without reading the archive leaves save blocked on a full pipe,
		// which only ends by being killed.
		save.Process.Kill()
	}
	if err := save.Wait(); err != nil || loadErr != nil {
		in.t.Fatalf("%s could not be carried to runner %s's daemon: %v %v: %s%s", local, pusher.Name, err, loadErr, saveErr.String(), loadOut.String())
	}

	for _, args := range [][]string{{"tag", local, tag}, {"push", tag}} {
		if _, err := pusher.daemon(ctx, args...); err != nil {
			in.t.Fatal(err)
		}
	}
	out, err := pusher.daemon(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", tag)
	if err != nil {
		in.t.Fatal(err)
	}
	var digests []string
	if err := json.Unmarshal([]byte(out), &digests); err != nil {
		in.t.Fatalf("the digests of %s do not decode: %s: %s", tag, err, out)
	}
	for _, d := range digests {
		if !strings.HasPrefix(d, repository+"@sha256:") {
			continue
		}
		// Taken back off the daemon that pushed it, so that whichever runner takes the task
		// pulls it from the registry by its digest.
		if _, err := pusher.daemon(ctx, "image", "rm", tag, local); err != nil {
			in.t.Fatal(err)
		}
		return tag, d
	}
	in.t.Fatalf("%s was pushed, and the daemon that pushed it holds no digest for it under %s: %s", tag, repository, out)
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
