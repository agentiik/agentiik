package main

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// What the release ships, checked on what was built rather than claimed by the flags passed.
//
// A static binary, because it goes into an image with nothing else in it, and is copied onto a host
// that may have another libc or none: a binary that needed a dynamic loader would fail there with
// an exec error the shell prints as "not found", the least informative failure there is. The image
// holding it, ghcr.io/agentiik/agentiik, holds the API's too, and cmd/agentiik-api tests it, running
// this program as its command names it.

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

// buildController builds this program for linux on one architecture, with the flags the release
// passes, and answers with the path.
//
// CGO_ENABLED=0 is what makes it static. These are the flags the header of
// build/agentiik.Dockerfile gives for the binaries the image is built from, and the ones
// .github/workflows/release.yml publishes it with; the three are kept the same by hand. A machine
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
