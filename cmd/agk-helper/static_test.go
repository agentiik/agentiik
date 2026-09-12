package main

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// What static means, spelled as something a machine can check.
//
// This binary is bound into an image the project does not control. There may be no libc in
// it, no dynamic loader, no shell and no /etc/passwd, and an executable that needed any of
// those would fail with a message nobody can read: a missing interpreter is an exec error
// the kernel reports and the shell prints as "not found", which is the least informative
// failure available. So the property is checked rather than claimed, on the two
// architectures a brick might be.

// architectures are the machines the release builds for, with the ELF machine each one has
// to report. They are the two the driver can be asked for by driver.Probe, and the two the
// embedded helper is laid down for.
var architectures = []struct {
	arch    string
	machine elf.Machine
}{
	{"amd64", elf.EM_X86_64},
	{"arm64", elf.EM_AARCH64},
}

// TestTheBinaryIsAStaticLinuxELFForEveryArchitectureABrickMightBe builds it and reads the
// result rather than trusting the flags that were passed.
func TestTheBinaryIsAStaticLinuxELFForEveryArchitectureABrickMightBe(t *testing.T) {
	for _, c := range architectures {
		t.Run(c.arch, func(t *testing.T) {
			path := buildHelper(t, "linux", c.arch)
			f, err := elf.Open(path)
			if err != nil {
				t.Fatalf("the build for linux/%s is not an ELF object: %s", c.arch, err)
			}
			defer f.Close()

			if f.Class != elf.ELFCLASS64 {
				t.Errorf("the class is %s, and both architectures are 64 bit", f.Class)
			}
			if f.Machine != c.machine {
				t.Errorf("the machine is %s, and linux/%s is %s: a binary for the wrong machine gives a script an exec format error rather than a program", f.Machine, c.arch, c.machine)
			}
			if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
				t.Errorf("the OS ABI is %s", f.OSABI)
			}

			// No PT_INTERP: nothing outside this file is loaded to run it, which
			// is the whole of what the mount can rely on.
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					t.Errorf("the binary carries a PT_INTERP segment, so it asks for a dynamic loader the image may not have")
				}
			}
			// And nothing it would have needed one for.
			if libs, err := f.ImportedLibraries(); err != nil {
				t.Errorf("reading the imported libraries: %s", err)
			} else if len(libs) > 0 {
				t.Errorf("the binary needs %v, and the image it is mounted into may carry none of them", libs)
			}
			if syms, err := f.ImportedSymbols(); err == nil && len(syms) > 0 {
				t.Errorf("the binary imports %d dynamic symbols", len(syms))
			}
		})
	}
}

// TestTheBuiltBinaryAnswersItsOwnCommandLine runs what was built, where the machine can run
// it, so that the three verbs are known to be in the thing that gets mounted rather than
// only in the package the tests call.
func TestTheBuiltBinaryAnswersItsOwnCommandLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Built for linux and run here: only a linux machine is both. The
		// end-to-end test is what runs it elsewhere, inside a container.
		t.Skip("the binary is built for linux, and this machine is " + runtime.GOOS)
	}
	path := buildHelper(t, "linux", runtime.GOARCH)
	out, err := exec.Command(path, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("running the built binary: %s\n%s", err, out)
	}
	for _, c := range commands {
		if !strings.Contains(string(out), c.usage) {
			t.Errorf("the built binary's own help does not carry %q:\n%s", c.usage, out)
		}
	}
}

// buildHelper builds this program for one platform, statically, and answers with the path.
//
// CGO_ENABLED=0 is the setting that makes it static, and it is here rather than in a script
// so that the test and the release build cannot disagree about what was tested. A machine
// with no Go toolchain in reach skips rather than fails: this is what the release build
// does, and a laptop without it is not a failure of this program.
func buildHelper(t *testing.T, goos, goarch string) string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the helper with")
	}
	path := filepath.Join(t.TempDir(), "agk-helper-"+goos+"-"+goarch)
	// The flags the release build passes, which is why they are written here: the
	// property this test checks has to be the property the shipped file has.
	cmd := exec.Command(tool, "build", "-trimpath", "-ldflags=-s -w", "-o", path, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building for %s/%s: %s\n%s", goos, goarch, err, out)
	}
	return path
}
