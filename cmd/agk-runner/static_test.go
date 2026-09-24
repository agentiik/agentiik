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

// What static means for the agent, spelled as something a machine can check.
//
// The runner "ships as a static binary and as a container image", and the binary is copied onto a
// host whose libc nobody chose for it, then run by a unit before anything else about the host is
// known. The image is built FROM scratch around it. An executable that needed a dynamic loader
// would fail on either with an exec error the kernel reports as a missing file, which is the least
// informative failure available, so the property is checked rather than claimed, on the two
// architectures a runner host is.

// architectures are the machines the release builds the agent for, with the ELF machine each one
// has to report.
var architectures = []struct {
	arch    string
	machine elf.Machine
}{
	{"amd64", elf.EM_X86_64},
	{"arm64", elf.EM_AARCH64},
}

func TestTheAgentIsAStaticLinuxELFForEveryArchitectureARunnerHostIs(t *testing.T) {
	for _, c := range architectures {
		t.Run(c.arch, func(t *testing.T) {
			path := buildRunner(t, "linux", c.arch, true)
			f, err := elf.Open(path)
			if err != nil {
				t.Fatalf("the build for linux/%s is not an ELF object: %s", c.arch, err)
			}
			defer f.Close()

			if f.Class != elf.ELFCLASS64 {
				t.Errorf("the class is %s, and both architectures are 64 bit", f.Class)
			}
			if f.Machine != c.machine {
				t.Errorf("the machine is %s, and linux/%s is %s", f.Machine, c.arch, c.machine)
			}
			if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
				t.Errorf("the OS ABI is %s", f.OSABI)
			}
			// No PT_INTERP: nothing outside this file is loaded to run it, which is
			// what a scratch image and a host of unknown libc can both rely on.
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					t.Errorf("the binary carries a PT_INTERP segment, so it asks for a dynamic loader the host or the image may not have")
				}
			}
			if libs, err := f.ImportedLibraries(); err != nil {
				t.Errorf("reading the imported libraries: %s", err)
			} else if len(libs) > 0 {
				t.Errorf("the binary needs %v", libs)
			}
			if syms, err := f.ImportedSymbols(); err == nil && len(syms) > 0 {
				t.Errorf("the binary imports %d dynamic symbols", len(syms))
			}
		})
	}
}

// TestTheBuiltAgentAnswersItsOwnCommandLine runs what was built, where the machine can run it, so
// that the three verbs are known to be in the shipped file rather than only in the package the
// other tests call.
func TestTheBuiltAgentAnswersItsOwnCommandLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the binary is built for linux, and this machine is " + runtime.GOOS)
	}
	path := buildRunner(t, "linux", runtime.GOARCH, true)
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

// buildRunner builds this program for one platform, statically, and answers with the path.
//
// CGO_ENABLED=0 is the setting that makes it static, and it is here rather than in a script so
// that the test and the release build cannot disagree about what was tested. stripped is the
// release's -s -w; the symbol check in boundary_test.go builds without it, since a stripped binary
// has no symbol table to read and would pass any check of one. A machine with no Go toolchain in
// reach skips rather than fails.
func buildRunner(t *testing.T, goos, goarch string, stripped bool) string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no Go toolchain to build the agent with")
	}
	path := filepath.Join(t.TempDir(), "agk-runner-"+goos+"-"+goarch)
	args := []string{"build", "-trimpath"}
	if stripped {
		args = append(args, "-ldflags=-s -w")
	}
	cmd := exec.Command(tool, append(args, "-o", path, ".")...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building for %s/%s: %s\n%s", goos, goarch, err, out)
	}
	return path
}
