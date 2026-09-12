package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What this package has to get right is an order and a refusal. The order is the one doc.go
// writes, and every step of it is tested here with files in a temporary directory. The refusal
// is about a file that cannot run inside a container, which is tested against a binary the
// toolchain actually builds, because a test that asserted its own idea of an ELF header would
// pass on a release that shipped the wrong one.

func TestHelperNoneMountsNothingDeliberately(t *testing.T) {
	for _, c := range []struct{ flag, env string }{
		{flag: None},
		{env: None},
	} {
		path, ok, err := Resolve(c.flag, c.env, "linux", "arm64", t.TempDir())
		if err != nil {
			t.Fatalf("--helper=none answered %s", err)
		}
		if ok || path != "" {
			t.Errorf("--helper=none answered %q: nothing is mounted, deliberately", path)
		}
	}
}

func TestAFileThatCouldNotRunInsideAContainerRefusesRatherThanBinding(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agk")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatalf("writing the file: %s", err)
	}

	_, ok, err := Resolve(script, "", "linux", "arm64", dir)
	if err == nil {
		t.Fatalf("a shell script was accepted as the static helper")
	}
	if ok {
		t.Errorf("a refused file was still reported as mountable")
	}
	if !strings.Contains(err.Error(), script) {
		t.Errorf("the refusal is %q, and it has to name the path", err)
	}
	if !strings.Contains(err.Error(), "/agk/bin/agk") {
		t.Errorf("the refusal is %q, and it has to say what that path has to be", err)
	}

	// A path that is not there at all is the same refusal, because the driver would
	// otherwise let the daemon create a directory where a program was promised.
	if _, _, err := Resolve(filepath.Join(dir, "nowhere"), "", "linux", "arm64", dir); err == nil {
		t.Errorf("a path that is not there was accepted as the static helper")
	}
}

func TestABuildThatCarriesNoHelperBindsNothingAndSaysSo(t *testing.T) {
	// This is the state of the tree: bin/ carries a README so that //go:embed compiles,
	// and the binaries are put there by the release build. The test therefore reads what
	// this build actually carries rather than asserting a number.
	dir := t.TempDir()
	path, ok, err := Resolve("", "", "linux", "aarch64", dir)
	if err != nil {
		t.Fatalf("resolving with nothing named: %s", err)
	}
	if carried := Embedded(); len(carried) == 0 {
		if ok {
			t.Errorf("this build carries no helper and Resolve answered %q", path)
		}
	} else if !ok {
		t.Errorf("this build carries %v and Resolve found none", carried)
	}
}

func TestTheEmbeddedBinaryIsLaidDownOncePerDirectoryAndExecutable(t *testing.T) {
	if len(Embedded()) == 0 {
		t.Skip("this build carries no helper: the release build puts them in bin/")
	}
	dir := t.TempDir()
	first, ok, err := Extract(dir, "linux", "arm64")
	if err != nil || !ok {
		t.Fatalf("extracting: %v %v", ok, err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat: %s", err)
	}
	if info.Mode().Perm() != helperMode {
		t.Errorf("the mode is %s, want %s: a container runs as an account this side never resolves", info.Mode().Perm(), os.FileMode(helperMode))
	}
	second, _, err := Extract(dir, "linux", "arm64")
	if err != nil {
		t.Fatalf("extracting again: %s", err)
	}
	if second != first {
		t.Errorf("the second extraction answered %s and the first %s: it is laid down once per directory", second, first)
	}
}

func TestTheDaemonsSpellingOfItsMachineIsNotGos(t *testing.T) {
	// Docker answers aarch64 where Go says arm64, and x86_64 where Go says amd64. A name
	// chosen by the daemon's spelling would be a file that is not there.
	for daemon, want := range map[string]string{
		"aarch64": "agk-linux-arm64",
		"arm64":   "agk-linux-arm64",
		"x86_64":  "agk-linux-amd64",
		"amd64":   "agk-linux-amd64",
	} {
		if got := carriedName("linux", daemon); got != want {
			t.Errorf("a daemon running %s would be given %q, want %q", daemon, got, want)
		}
	}
	if got := carriedName("linux", "mips"); got != "" {
		t.Errorf("a daemon running mips would be given %q, and this release ships nothing for it", got)
	}
}

func TestARealStaticBinaryPassesAndTheWrongMachineDoesNot(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build the helper with")
	}
	dir := t.TempDir()
	built := filepath.Join(dir, "agk-linux-arm64")

	// Built the way the release builds it, which is the only way to hold the check to
	// something real: statically linked, linux, for one machine.
	cmd := exec.Command("go", "build", "-o", built, "github.com/agentiik/agentiik/cmd/agk-helper")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("building the helper: %s\n%s", err, out)
	}

	if err := Check(built, "linux", "aarch64"); err != nil {
		t.Errorf("a statically linked linux arm64 binary was refused: %s", err)
	}
	// The same file on a daemon that runs the other machine is exactly what this refusal
	// exists for: a shell would report an exec format error and say nothing useful.
	err := Check(built, "linux", "x86_64")
	if err == nil {
		t.Fatalf("an arm64 binary was accepted for an x86_64 daemon")
	}
	if !strings.Contains(err.Error(), "exec format error") {
		t.Errorf("the refusal is %q, and it has to say what the container would have said", err)
	}
	// And a daemon that does not run linux containers at all.
	if err := Check(built, "windows", "amd64"); err == nil {
		t.Errorf("a linux program was accepted for a windows daemon")
	}
}
