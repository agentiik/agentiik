package driver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The zero value is the floor. This is the test the enumeration exists for: a Policy
// nobody filled in has to refuse a daemon with no remapping, which a bool field could
// not have done.
func TestZeroPolicyKeepsTheFloor(t *testing.T) {
	var p Policy
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("the zero value of a Policy lifts the userns floor, which is the one shape it must not have")
	}
	if got := p.RequireUsernsRemap.String(); got != "true" {
		t.Fatalf("the zero floor reads as require_userns_remap = %s, want true", got)
	}
}

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if p.RequireUsernsRemap != RemapRequired {
		t.Fatalf("DefaultPolicy lifts the floor")
	}
	if p.PidsLimit != 256 {
		t.Fatalf("PidsLimit is %d, and the settings table says the default is 256", p.PidsLimit)
	}
	if p.StopGrace <= 0 {
		t.Fatalf("StopGrace is %s: the daemon's stop takes a grace and zero is not one", p.StopGrace)
	}
	if p.Ulimits.NProc.Soft != p.PidsLimit {
		t.Fatalf("nproc is %d and the pid ceiling is %d: the two say one number", p.Ulimits.NProc.Soft, p.PidsLimit)
	}
	if p.TmpSize <= 0 {
		t.Fatalf("the /tmp tmpfs is sized, and %d is not a size", p.TmpSize)
	}
	if len(p.AllowCapAdd) != 0 {
		t.Fatalf("CapAdd is allowed by default: %v", p.AllowCapAdd)
	}
}

func TestParseUsernsFloor(t *testing.T) {
	for _, c := range []struct {
		in     string
		want   UsernsFloor
		refuse bool
	}{
		{in: "true", want: RemapRequired},
		{in: "false", want: RemapLifted},
		{in: "True", refuse: true},
		{in: "0", refuse: true},
		{in: "", refuse: true},
	} {
		got, err := ParseUsernsFloor(c.in)
		if c.refuse {
			if err == nil {
				t.Fatalf("require_userns_remap = %q was accepted", c.in)
			}
			if !strings.Contains(err.Error(), "require_userns_remap") || !strings.Contains(err.Error(), PolicyPath) {
				t.Fatalf("the refusal of %q names neither the key nor the file: %s", c.in, err)
			}
			// A refused value leaves the floor in place, so a caller that
			// ignores the error is still refused rather than lifted.
			if got.Lifted() {
				t.Fatalf("a refused value came back lifted")
			}
			continue
		}
		if err != nil {
			t.Fatalf("require_userns_remap = %q: %s", c.in, err)
		}
		if got != c.want {
			t.Fatalf("require_userns_remap = %q read as %s", c.in, got)
		}
	}
}

func TestLoadPolicyLiftsTheFloor(t *testing.T) {
	path := writePolicyFile(t, `
# this machine is a laptop and Docker Desktop does not offer the remapping
require_userns_remap = false
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if !p.RequireUsernsRemap.Lifted() {
		t.Fatalf("require_userns_remap = false did not lift the floor")
	}
}

// The value may carry a trailing comment, which is where an operator writes down why the
// floor was lifted. Reading "false # laptop" as a value that is not false would refuse
// the very file the documentation asks somebody to write.
func TestLoadPolicyReadsAValueWithAComment(t *testing.T) {
	path := writePolicyFile(t, "require_userns_remap = false  # laptop, see the installation notes\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if !p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a value with a trailing comment did not lift the floor")
	}
}

// A file that says nothing about the floor leaves it in place, because the documented
// default of the setting is true and an absent line is not a decision.
func TestLoadPolicySilentFileKeepsTheFloor(t *testing.T) {
	path := writePolicyFile(t, "[runner]\nname = \"runner-dmz-02\"\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a file that says nothing lifted the floor")
	}
}

// Every key this package does not own is ignored rather than refused, including a
// require_userns_remap written under a table, which is a different setting that happens
// to share a name. The [hooks] block of v0.2.0 is the reason the reading is permissive.
func TestLoadPolicyIgnoresWhatItDoesNotOwn(t *testing.T) {
	path := writePolicyFile(t, `
concurrency = 4
labels = ["zone=dmz", "arch=amd64"]

[hooks]
before_task = "/usr/local/bin/prepare"
require_userns_remap = false
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy refused a file it does not own the whole of: %s", err)
	}
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a require_userns_remap under [hooks] lifted the floor of the runner")
	}
}

func TestLoadPolicyRefusesAValueItCannotRead(t *testing.T) {
	path := writePolicyFile(t, "require_userns_remap = maybe\n")
	p, err := LoadPolicy(path)
	if err == nil {
		t.Fatalf("require_userns_remap = maybe was accepted")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("the refusal does not say where in the file it is: %s", err)
	}
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a refused file came back with a lifted floor")
	}
}

// A runner installed on a machine with no configuration file is the ordinary case for
// agk run --local, and the caller has to be able to tell that case from a file it could
// not read.
func TestLoadPolicyMissingFile(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "runner.toml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file came back as %v, which does not wrap fs.ErrNotExist", err)
	}
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a missing file came back with a lifted floor")
	}
}

func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %s", path, err)
	}
	return path
}
