package main

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// What the release ships, checked on what was built rather than claimed by the flags passed.
//
// A static binary, because it goes into an image with nothing else in it, and is copied onto a host
// that may have another libc or none: a binary that needed a dynamic loader would fail there with
// an exec error the shell prints as "not found", the least informative failure there is. And an
// image holding that same binary, run as a user that is not root.

// architectures are the machines the release builds for, with the ELF machine each one reports.
var architectures = []struct {
	arch    string
	machine elf.Machine
}{
	{"amd64", elf.EM_X86_64},
	{"arm64", elf.EM_AARCH64},
}

// TestTheBinaryIsAStaticLinuxELFForEveryArchitectureTheReleaseBuilds builds it and reads the
// result.
func TestTheBinaryIsAStaticLinuxELFForEveryArchitectureTheReleaseBuilds(t *testing.T) {
	for _, c := range architectures {
		t.Run(c.arch, func(t *testing.T) {
			f, err := elf.Open(buildController(t, c.arch))
			if err != nil {
				t.Fatalf("the build for linux/%s is not an ELF object: %s", c.arch, err)
			}
			defer f.Close()

			if f.Class != elf.ELFCLASS64 || f.Machine != c.machine {
				t.Errorf("the build for linux/%s is %s %s, want ELFCLASS64 %s", c.arch, f.Class, f.Machine, c.machine)
			}
			// No PT_INTERP: nothing outside this file is loaded to run it.
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					t.Errorf("the binary carries a PT_INTERP segment, so it asks for a dynamic loader the image does not have")
				}
			}
			if libs, err := f.ImportedLibraries(); err != nil {
				t.Errorf("reading the imported libraries: %s", err)
			} else if len(libs) > 0 {
				t.Errorf("the binary needs %v, and the image holds none of them", libs)
			}
			if syms, err := f.ImportedSymbols(); err == nil && len(syms) > 0 {
				t.Errorf("the binary imports %d dynamic symbols", len(syms))
			}
		})
	}
}

// The image build/controller.Dockerfile describes, built from the binary as the release builds it
// and run on the daemon on this machine. It holds that binary, which runs with nothing else in the
// image and is refused its start for want of configuration, naming each setting; it runs as a user
// that is not root; it holds the certificates every connection it makes is verified against; and
// it carries the annotations a published image carries.
func TestTheImageRunsTheBinaryAsAUserThatIsNotRoot(t *testing.T) {
	socket, ok := dockertest.Socket()
	if !ok {
		dockertest.Unavailable(t, "no Docker daemon on this machine")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		dockertest.Unavailable(t, "no docker command to build the image with")
	}
	arch := daemonArch(t, socket)

	dir := t.TempDir()
	built, err := os.ReadFile(buildController(t, arch))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agentiik-controller-linux-"+arch), built, 0o755); err != nil {
		t.Fatal(err)
	}
	// The one image the build pulls, which a machine running these tests holds already: a test
	// that pulled it would be a test of somebody's registry. Once it is here, a build that fails
	// is the Dockerfile's.
	if err := exec.Command("docker", "image", "inspect", "alpine:3.21").Run(); err != nil {
		dockertest.Unavailable(t, "alpine:3.21, which the image takes its certificates from, is not on this machine")
	}
	const image = "agentiik-controller:test"
	build := exec.Command("docker", "build", "-q", "-t", image,
		"--build-arg", "VERSION=0.0.0-test", "--build-arg", "REVISION=0000000",
		"-f", filepath.Join("..", "..", "build", "controller.Dockerfile"), dir)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the image could not be built: %v\n%s", err, out)
	}

	out, err := exec.Command("docker", "image", "inspect", image).Output()
	if err != nil {
		t.Fatal(err)
	}
	var inspected []struct {
		Config struct {
			User   string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(out, &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("the image inspects as %s: %v", out, err)
	}
	cfg := inspected[0].Config
	if user, _, _ := strings.Cut(cfg.User, ":"); user == "" || user == "0" || user == "root" {
		t.Errorf("the image runs as %q, which is root", cfg.User)
	}
	for _, label := range []string{"title", "description", "version", "source", "revision", "licenses"} {
		if cfg.Labels["org.opencontainers.image."+label] == "" {
			t.Errorf("the image carries no org.opencontainers.image.%s", label)
		}
	}
	if got := cfg.Labels["org.opencontainers.image.licenses"]; got != "AGPL-3.0-or-later" {
		t.Errorf("the image is licensed %q, and the server is AGPL-3.0-or-later", got)
	}

	// Bounded, so that a container that hangs fails the test rather than the run.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	runIt := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", image)
	runIt.Stdout, runIt.Stderr = &stdout, &stderr
	err = runIt.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitFailed {
		t.Fatalf("with nothing configured, the image exited %v, want %d:\n%s%s", err, exitFailed, stdout.String(), stderr.String())
	}
	for _, variable := range []string{config.DatabaseURL, config.BusURL, config.BusCredentialsFile, config.ObjectsDir} {
		if !strings.Contains(stderr.String(), variable) {
			t.Errorf("the image's refusal does not name %s:\n%s", variable, stderr.String())
		}
	}

	// scratch has no shell to look inside with, so the certificates are copied out.
	created, err := exec.Command("docker", "create", image).Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(created))
	defer exec.Command("docker", "rm", "-f", id).Run()
	bundle, err := exec.Command("docker", "cp", id+":/etc/ssl/certs/ca-certificates.crt", "-").Output()
	if err != nil || !bytes.Contains(bundle, []byte("CERTIFICATE")) {
		t.Errorf("the image holds no certificate bundle to verify the database and the bus with: %v", err)
	}

	// The object store's directory is the image user's, so that a named volume mounted there
	// takes that owner and the controller can write the inputs of every task in it.
	objects, err := exec.Command("docker", "cp", id+":/var/lib/agentiik/objects", "-").Output()
	if err != nil {
		t.Fatalf("the image holds no /var/lib/agentiik/objects: %v", err)
	}
	header, err := tar.NewReader(bytes.NewReader(objects)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if owner := fmt.Sprintf("%d:%d", header.Uid, header.Gid); owner != cfg.User || !header.FileInfo().IsDir() {
		t.Errorf("/var/lib/agentiik/objects is owned by %s, and the image runs as %s", owner, cfg.User)
	}
}

// daemonArch is the GOARCH the daemon runs containers as, which is what the binary in the image is
// built for.
func daemonArch(t *testing.T, socket string) string {
	t.Helper()
	cli, err := docker.Dial(socket)
	if err != nil {
		dockertest.Unavailable(t, "opening a client on %s: %v", socket, err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	info, err := cli.Info(ctx)
	if err != nil {
		dockertest.Unavailable(t, "asking the daemon what it is: %v", err)
	}
	switch info.Architecture {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	t.Skipf("the daemon reports the architecture %q, and the release builds amd64 and arm64", info.Architecture)
	return ""
}

// buildController builds this program for linux on one architecture, with the flags the release
// passes, and answers with the path.
//
// CGO_ENABLED=0 is what makes it static. These are the flags the header of
// build/controller.Dockerfile gives for the binaries the image is built from, and the two are
// kept the same by hand until a release workflow builds from one definition of them. A machine
// with no Go toolchain in reach skips.
func buildController(t *testing.T, arch string) string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the controller with")
	}
	path := filepath.Join(t.TempDir(), "agentiik-controller-linux-"+arch)
	cmd := exec.Command(tool, "build", "-trimpath", "-ldflags=-s -w", "-o", path, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building for linux/%s: %s\n%s", arch, err, out)
	}
	return path
}
