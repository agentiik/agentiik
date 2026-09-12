package helper

import (
	"debug/elf"
	"fmt"
	"os"

	"github.com/agentiik/agentiik/driver"
)

// Check holds a file to what /agk/bin/agk has to be, before anything binds it.
//
// A helper that cannot run is worse than no helper: the step says agk: not found either way,
// and one of the two took a mount to say it. Worse still is a binary for the wrong machine,
// which a shell reports as an exec format error, a sentence that says nothing about what a
// person should do next.
//
// Four things, and each is a fact a machine can check rather than a sentence somebody wrote:
//
//	a regular file        the driver refuses anything else anyway, rather than letting the
//	                      daemon create a directory where a program was promised
//	a linux ELF           which is what a container on a linux daemon can execute
//	the daemon's machine  because the architecture a container runs natively is the
//	                      daemon's and never this host's
//	no PT_INTERP          which is what "static" means spelled as something checkable: a
//	                      dynamic binary needs an interpreter the image may not carry
func Check(path, ostype, arch string) error {
	if path == "" {
		return fmt.Errorf("helper: there is no path to check")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("helper: %s is named as the static helper and is not there: it is mounted read-only at %s for a script step: %w", path, driver.BinPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("helper: %s is %s and not a file: %s is one program, and a path that is not one would be bound as a directory", path, info.Mode().Type(), driver.BinPath)
	}
	if ostype != "" && ostype != "linux" {
		return fmt.Errorf("helper: the daemon runs %s containers, and %s is a linux program: %s is mounted into a container and has to be one the container can execute", ostype, path, driver.BinPath)
	}

	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("helper: %s is not a linux ELF program: %s is executed inside a container, so a shell script or a macOS binary there is an exec format error rather than a program: %w", path, driver.BinPath, err)
	}
	defer f.Close()

	if want, ok := machine(arch); ok && f.Machine != want {
		return fmt.Errorf("helper: %s is built for %s and this daemon runs %s: a binary for the wrong machine gives a script an exec format error rather than a program", path, f.Machine, arch)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("helper: %s is dynamically linked and asks for an interpreter: %s is mounted into an image this project does not control, which may carry no interpreter at all, so it is built CGO_ENABLED=0 and statically linked", path, driver.BinPath)
		}
	}
	return nil
}

// machine is the ELF machine of one daemon architecture, and whether this side knows it.
//
// An architecture it does not know is not a refusal. The three other checks still apply, and a
// machine nobody here has a name for is a machine this release does not ship a binary for,
// which is the caller's own --helper and not a file to refuse on a guess.
func machine(arch string) (elf.Machine, bool) {
	switch architecture(arch) {
	case "arm64":
		return elf.EM_AARCH64, true
	case "amd64":
		return elf.EM_X86_64, true
	default:
		return 0, false
	}
}
