package driver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/dockertest"
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
	if p.RequireSeccomp.Lifted() {
		t.Fatalf("the zero value of a Policy lifts the seccomp floor")
	}
}

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if p.RequireUsernsRemap != RemapRequired {
		t.Fatalf("DefaultPolicy lifts the floor")
	}
	if p.RequireSeccomp != SeccompRequired {
		t.Fatalf("DefaultPolicy lifts the seccomp floor")
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

// Every key of the reference table, written once, lands where the driver reads it. A key
// that parsed and went nowhere would be a setting an operator wrote and nothing applied.
func TestLoadPolicyReadsEveryKey(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "seccomp.json")
	if err := os.WriteFile(profile, []byte("{\n  \"defaultAction\": \"SCMP_ACT_ERRNO\",\n  \"syscalls\": []\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := writePolicyFile(t, `
require_userns_remap = false
secrets_dir = "/run/agentiik/secrets"
stop_grace = "30s"
helper = "/usr/local/lib/agentiik/agk-helper"
seccomp_profile = "`+profile+`"
apparmor_profile = "agentiik-brick"
selinux_label = "level:s0:c100,c200"
allow_cap_add = ["NET_BIND_SERVICE", "SYS_PTRACE"]
pids_limit = 512
memory_cap = "8Gi"
cpu_cap = "4"
tmp_size = "128Mi"
log_max_bytes = 1048576
log_max_lines = 1000

[ulimits]
nofile = { soft = 2048, hard = 8192 }
nproc = { soft = 400, hard = 500 }
`)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}

	for _, c := range []struct {
		key  string
		ok   bool
		read any
	}{
		{"require_userns_remap", p.RequireUsernsRemap.Lifted(), p.RequireUsernsRemap},
		{"secrets_dir", p.SecretsDirSkipped == "/run/agentiik/secrets", p.SecretsDirSkipped},
		{"stop_grace", p.StopGrace == 30*time.Second, p.StopGrace},
		{"helper", p.Helper == "/usr/local/lib/agentiik/agk-helper", p.Helper},
		{"seccomp_profile", p.Seccomp == `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[]}`, p.Seccomp},
		{"apparmor_profile", p.AppArmor == "agentiik-brick", p.AppArmor},
		{"selinux_label", p.SELinuxLabel == "level:s0:c100,c200", p.SELinuxLabel},
		{"allow_cap_add", slices.Equal(p.AllowCapAdd, []string{"NET_BIND_SERVICE", "SYS_PTRACE"}), p.AllowCapAdd},
		{"pids_limit", p.PidsLimit == 512, p.PidsLimit},
		{"memory_cap", p.MemoryCap == 8<<30, p.MemoryCap},
		{"cpu_cap", p.CPUCap == 4, p.CPUCap},
		{"tmp_size", p.TmpSize == 128<<20, p.TmpSize},
		{"log_max_bytes", p.LogMaxBytes == 1<<20, p.LogMaxBytes},
		{"log_max_lines", p.LogMaxLines == 1000, p.LogMaxLines},
		{"ulimits.nofile", p.Ulimits.NoFile == Ulimit{Soft: 2048, Hard: 8192}, p.Ulimits.NoFile},
		{"ulimits.nproc", p.Ulimits.NProc == Ulimit{Soft: 400, Hard: 500}, p.Ulimits.NProc},
	} {
		if !c.ok {
			t.Errorf("%s was read as %v", c.key, c.read)
		}
	}
	if p.Source != path {
		t.Errorf("the policy says it came from %q, and it was read from %s", p.Source, path)
	}
	if p.HooksSkipped {
		t.Errorf("a file with no [hooks] table says hooks were skipped")
	}
	// Nothing in the file speaks of seccomp as a floor, and nothing in it can lift one.
	if p.RequireSeccomp.Lifted() {
		t.Errorf("a runner's file lifted the seccomp floor")
	}
}

// A key the file does not write keeps its default, so a file of one line changes one
// setting and leaves the floor and every other default where they were.
func TestLoadPolicyKeepsTheDefaultsOfWhatItDoesNotWrite(t *testing.T) {
	p, err := LoadPolicy(writePolicyFile(t, "tmp_size = \"32Mi\"\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	want := DefaultPolicy()
	want.TmpSize = 32 << 20
	if p.RequireUsernsRemap.Lifted() {
		t.Fatalf("a file that says nothing about the floor lifted it")
	}
	if p.PidsLimit != want.PidsLimit || p.Ulimits != want.Ulimits || p.StopGrace != want.StopGrace || p.TmpSize != want.TmpSize || p.SecretsDirSkipped != "" || p.LogMaxBytes != want.LogMaxBytes {
		t.Fatalf("one line moved more than its setting: %+v", p)
	}
}

// nproc says the pid ceiling's number unless the file says otherwise, which is the reason
// DefaultPolicy gives the two one number: raising pids_limit alone would otherwise leave
// the ulimit refusing the processes the cgroup was just told to allow.
func TestPidsLimitMovesNprocWithIt(t *testing.T) {
	p, err := LoadPolicy(writePolicyFile(t, "pids_limit = 1024\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if p.Ulimits.NProc != (Ulimit{Soft: 1024, Hard: 1024}) {
		t.Fatalf("pids_limit = 1024 left nproc at %+v", p.Ulimits.NProc)
	}

	p, err = LoadPolicy(writePolicyFile(t, "pids_limit = 1024\n\n[ulimits]\nnproc = { soft = 100, hard = 200 }\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if p.Ulimits.NProc != (Ulimit{Soft: 100, Hard: 200}) {
		t.Fatalf("an nproc the file wrote was overridden by pids_limit: %+v", p.Ulimits.NProc)
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
	for _, body := range []string{"", "# nothing decided here yet\n", "stop_grace = \"5s\"\n"} {
		p, err := LoadPolicy(writePolicyFile(t, body))
		if err != nil {
			t.Fatalf("LoadPolicy of %q: %s", body, err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Fatalf("a file that says nothing about the floor lifted it: %q", body)
		}
	}
}

// A misspelled key is refused naming its line, and a table no setting has is refused the
// same way. The permissive reading would have let pids_limt = 64 leave the default of 256
// in force without a word.
func TestLoadPolicyRefusesAKeyItDoesNotRead(t *testing.T) {
	for _, c := range []struct {
		body string
		want []string
	}{
		{"require_userns_remap = true\npids_limt = 64\n", []string{"pids_limt on line 2", "is not a setting"}},
		{"require_userns_remap = false\npids_limt = 64\n", []string{"pids_limt on line 2", "is not a setting"}},
		{"concurrency = 4\nlabels = [\"zone=dmz\"]\n", []string{"concurrency on line 1 and labels on line 2", "are not settings"}},
		{"[runner]\nname = \"runner-dmz-02\"\n", []string{"runner", "is not a setting"}},
		{"[ulimits]\nnofiles = { soft = 1, hard = 2 }\n", []string{"ulimits.nofiles on line 2"}},
		{"[ulimits.nofile]\nsoft = 1\nhardd = 2\n", []string{"ulimits.nofile.hardd on line 3"}},
	} {
		p, err := LoadPolicy(writePolicyFile(t, c.body))
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		for _, want := range append(c.want, "pids_limit") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %q does not say %q: %s", c.body, want, err)
			}
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("a refused file came back with a lifted floor")
		}
	}
}

// A key is read exactly as the documentation writes it. The decoder alone would match
// Require_Userns_Remap to the floor and lift it under a spelling nobody documented.
func TestLoadPolicyRefusesAKeyInAnotherCase(t *testing.T) {
	for _, c := range []struct{ body, spelled, want string }{
		{"Require_Userns_Remap = false\n", "Require_Userns_Remap", "require_userns_remap"},
		{"[ULIMITS]\nnofile = { soft = 1, hard = 2 }\n", "ULIMITS", "ulimits"},
		{"[ulimits]\nnofile = { Soft = 1, hard = 2 }\n", "ulimits.nofile.Soft", "ulimits.nofile.soft"},
	} {
		p, err := LoadPolicy(writePolicyFile(t, c.body))
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		if !strings.Contains(err.Error(), c.spelled+" is spelled "+c.want) {
			t.Errorf("the refusal of %q does not name the spelling: %s", c.body, err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("%q came back with a lifted floor", c.body)
		}
	}
}

// A value of the wrong type is refused naming its line, and answered with the type it
// should have had in the file's own terms rather than the Go type it failed to decode into.
func TestLoadPolicyRefusesAWrongType(t *testing.T) {
	for _, c := range []struct {
		body string
		want string
	}{
		{"require_userns_remap = \"false\"\n", "line 1: require_userns_remap is true or false"},
		{"require_userns_remap = 0\n", "line 1: require_userns_remap is true or false"},
		{"\npids_limit = \"many\"\n", "line 2: pids_limit is a whole number above zero"},
		// The floor lifted before the refused line, so the check below has a floor to
		// find lifted.
		{"require_userns_remap = false\npids_limit = \"many\"\n", "line 2: pids_limit is a whole number above zero"},
		{"pids_limit = 2.5\n", "line 1: pids_limit is a whole number above zero"},
		{"memory_cap = 8589934592\n", "line 1: memory_cap is a whole number of at least 6Mi with a binary suffix"},
		{"cpu_cap = 4\n", "line 1: cpu_cap is a number of cores of at least 0.01 in quotation marks"},
		{"allow_cap_add = \"NET_ADMIN\"\n", "line 1: allow_cap_add is a list of capability names"},
		{"allow_cap_add = [\"NET_ADMIN\", 1]\n", "line 1: allow_cap_add is a list of capability names"},
		{"[ulimits]\nnofile = 4096\n", "line 2: ulimits.nofile is a table of soft and hard"},
		{"[ulimits.nofile]\nsoft = \"1024\"\nhard = 4096\n", "line 2: ulimits.nofile.soft is a whole number above zero"},
		{"ulimits = [1]\n", "line 1: ulimits is a table holding nofile and nproc"},
		{"hooks = 3\n", "line 1: hooks is a table"},
		{"stop_grace = 10\n", "line 1: stop_grace is a whole number of seconds"},
	} {
		p, err := LoadPolicy(writePolicyFile(t, c.body))
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("the refusal of %q does not say %q: %s", c.body, c.want, err)
		}
		if strings.Contains(err.Error(), "runnerFile") || strings.Contains(err.Error(), "int64") {
			t.Errorf("the refusal of %q speaks of this package's types: %s", c.body, err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("a refused file came back with a lifted floor")
		}
	}
}

// A line that is not TOML is refused in TOML's own words, with its line. The first two
// are the spellings of a boolean TOML does not have; the third is a key written twice.
func TestLoadPolicyRefusesALineThatIsNotTOML(t *testing.T) {
	for _, c := range []struct {
		body string
		line string
	}{
		{"require_userns_remap = maybe\n", "line 1"},
		{"\nrequire_userns_remap = True\n", "line 2"},
		{"pids_limit = 1\npids_limit = 2\n", "line 2"},
		{"require_userns_remap = false\npids_limit = 1\npids_limit = 2\n", "line 3"},
	} {
		p, err := LoadPolicy(writePolicyFile(t, c.body))
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		if !strings.Contains(err.Error(), c.line+" is not TOML") {
			t.Errorf("the refusal of %q does not say where in the file it is: %s", c.body, err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("a refused file came back with a lifted floor")
		}
	}
}

// What a type cannot say is checked after the decode: a path that is not absolute, a limit
// that is not above zero, a size or a core count off the grammar resources uses, a ulimit
// with half of it missing.
func TestLoadPolicyRefusesAValueOutsideItsSetting(t *testing.T) {
	for _, c := range []struct {
		body string
		want string
	}{
		{"helper = \"bin/agk\"\n", `helper is "bin/agk"`},
		{"stop_grace = \"1500ms\"\n", `stop_grace is "1500ms"`},
		{"stop_grace = \"0s\"\n", `stop_grace is "0s"`},
		{"stop_grace = \"soon\"\n", `stop_grace is "soon"`},
		{"pids_limit = 0\n", "pids_limit is 0"},
		{"pids_limit = -1\n", "pids_limit is -1"},
		{"memory_cap = \"8G\"\n", `memory_cap is "8G"`},
		{"memory_cap = \"0Mi\"\n", `memory_cap is "0Mi"`},
		{"memory_cap = \"+8Gi\"\n", `memory_cap is "+8Gi"`},
		{"memory_cap = \"99999999999Ti\"\n", `memory_cap is "99999999999Ti"`},
		{"tmp_size = \"64MB\"\n", `tmp_size is "64MB"`},
		// The daemon's own least: a cap is what a step naming no resources gets, and
		// one under it fails every such step.
		{"memory_cap = \"4Mi\"\n", `memory_cap is "4Mi"`},
		{"memory_cap = \"6143Ki\"\n", "of at least 6Mi"},
		{"cpu_cap = \"0.005\"\n", `cpu_cap is "0.005"`},
		{"cpu_cap = \"0.001\"\n", "of at least 0.01"},
		{"cpu_cap = \"0\"\n", `cpu_cap is "0"`},
		{"cpu_cap = \"Inf\"\n", `cpu_cap is "Inf"`},
		{"cpu_cap = \"1e3\"\n", `cpu_cap is "1e3"`},
		{"cpu_cap = \"99999999999\"\n", `cpu_cap is "99999999999"`},
		{"log_max_bytes = 0\n", "log_max_bytes is 0"},
		{"log_max_lines = -5\n", "log_max_lines is -5"},
		{"apparmor_profile = \"\"\n", `apparmor_profile is ""`},
		{"apparmor_profile = \"unconfined\"\n", "which confines nothing"},
		{"selinux_label = \"disable\"\n", `selinux_label is "disable"`},
		{"selinux_label = \"level:\"\n", `selinux_label is "level:"`},
		{"selinux_label = \"colour:blue\"\n", `selinux_label is "colour:blue"`},
		{"selinux_label = \"type:spc_t\"\n", "spc_t is a type the SELinux policy leaves unconfined"},
		{"selinux_label = \"type:unconfined_t\"\n", "which confines nothing"},
		{"allow_cap_add = [\"ALL\"]\n", "Privileged"},
		{"allow_cap_add = [\"CAP_NET_ADMIN\"]\n", "as NET_ADMIN"},
		{"allow_cap_add = [\"net_admin\"]\n", "as NET_ADMIN"},
		{"allow_cap_add = [\"NET_ADMN\"]\n", "not a Linux capability"},
		{"[ulimits]\nnofile = { soft = 1024 }\n", "ulimits.nofile in"},
		{"[ulimits]\nnproc = { hard = 10 }\n", "hard alone"},
		{"[ulimits]\nnofile = { soft = 0, hard = 10 }\n", "ulimits.nofile.soft is 0"},
		{"[ulimits]\nnofile = { soft = 20, hard = 10 }\n", "ulimits.nofile.hard is 10"},
		// Above the kernel's default fs.nr_open, every container fails as it starts.
		{"[ulimits]\nnofile = { soft = 1024, hard = 2000000 }\n", "ulimits.nofile.hard is 2000000 in"},
		{"seccomp_profile = \"seccomp.json\"\n", `seccomp_profile is "seccomp.json"`},
	} {
		// Each file lifts the floor before the line that is refused, so that a refusal
		// coming back with what the file had read so far would come back lifted.
		body := "require_userns_remap = false\n" + c.body
		path := writePolicyFile(t, body)
		p, err := LoadPolicy(path)
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), path) {
			t.Errorf("the refusal of %q does not say %q in %s: %s", body, c.want, path, err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("the refusal of %q came back with the floor its first line lifted", body)
		}
	}
}

// The least a cap may be and the most nofile may be are the daemon's and the kernel's own
// figures, and each is taken exactly.
func TestLoadPolicyTakesTheBoundsItHoldsTo(t *testing.T) {
	p, err := LoadPolicy(writePolicyFile(t, "memory_cap = \"6Mi\"\ncpu_cap = \"0.01\"\n\n[ulimits]\nnofile = { soft = 1048576, hard = 1048576 }\n"))
	if err != nil {
		t.Fatalf("the bounds themselves were refused: %s", err)
	}
	if p.MemoryCap != 6<<20 || p.CPUCap != 0.01 || p.Ulimits.NoFile.Hard != 1<<20 {
		t.Fatalf("the bounds were read as %d, %v and %d", p.MemoryCap, p.CPUCap, p.Ulimits.NoFile.Hard)
	}
	// nproc has no ceiling of the kernel's to be held to.
	if _, err := LoadPolicy(writePolicyFile(t, "[ulimits]\nnproc = { soft = 4096, hard = 2000000 }\n")); err != nil {
		t.Fatalf("an nproc above nofile's ceiling was refused: %s", err)
	}
}

// seccomp_profile names a file, and the file is read when the policy is, so that a
// profile that is missing or is not one refuses the start rather than every create after
// it.
func TestLoadPolicyReadsTheSeccompProfileItNames(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, body, want string
	}{
		{"missing.json", "", "could not be read"},
		{"array.json", "[]", "is not a seccomp profile"},
		{"empty.json", "{}", "is not a seccomp profile"},
		{"broken.json", "{\"defaultAction\": ", "is not a seccomp profile"},
		// The daemon takes any action at the create and the runtime refuses one it does
		// not know at the start, every container.
		{"misspelled.json", `{"defaultAction":"SCMP_ACT_ERRON"}`, `action "SCMP_ACT_ERRON" is not one seccomp has`},
		{"rule.json", `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALOW"}]}`, `action "SCMP_ACT_ALOW"`},
		{"unnamed.json", `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"]}]}`, `action ""`},
		// A profile that filters nothing would lift the floor under the name of meeting it.
		{"allow.json", `{"defaultAction":"SCMP_ACT_ALLOW"}`, "lets every system call through"},
		{"log.json", `{"defaultAction":"SCMP_ACT_LOG","syscalls":[{"names":["reboot"],"action":"SCMP_ACT_ALLOW"}]}`, "lets every system call through"},
	} {
		file := filepath.Join(dir, c.name)
		if c.body != "" {
			if err := os.WriteFile(file, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := LoadPolicy(writePolicyFile(t, "seccomp_profile = \""+file+"\"\n"))
		if err == nil {
			t.Fatalf("a seccomp_profile naming %s was accepted", c.name)
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), file) {
			t.Errorf("the refusal of %s does not say %q and name the file: %s", c.name, c.want, err)
		}
		// fs.ErrNotExist is LoadPolicy saying there is no runner.toml, which a caller
		// answers with DefaultPolicy. A profile the file names and nobody deployed is a
		// file that says something, and reading it as no file would drop every other
		// setting in it with no refusal.
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refusal of %s reads as a runner.toml that is not there: %s", c.name, err)
		}
	}
}

// A profile that refuses one call and lets the rest through filters something, and is
// the operator's to write: only a profile that filters nothing at all is refused.
func TestLoadPolicyTakesAProfileThatRefusesOneCall(t *testing.T) {
	file := filepath.Join(t.TempDir(), "seccomp.json")
	if err := os.WriteFile(file, []byte(`{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["reboot"],"action":"SCMP_ACT_ERRNO"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(writePolicyFile(t, "seccomp_profile = \""+file+"\"\n"))
	if err != nil {
		t.Fatalf("a profile refusing reboot was refused: %s", err)
	}
	if p.Seccomp == "" {
		t.Fatalf("the profile was not kept")
	}
}

// [hooks] runs nothing until v0.9.0, and a file already written for the hooks, the
// documentation's own example among them, is neither refused nor run.
func TestLoadPolicyReadsTheHooksTableAndRunsNothing(t *testing.T) {
	p, err := LoadPolicy(writePolicyFile(t, `
pids_limit = 300

[hooks]
timeout = "30s"

# before the container is created, after the image is resolved
pre_task = [
  "/usr/local/sbin/attach-licence --slot $AGK_TASK_ID",
  "nvidia-smi -L > /dev/null",
]

# after the container is destroyed, whatever the verdict
post_task = [
  "/usr/local/sbin/release-licence --slot $AGK_TASK_ID",
  "find $AGK_WORKDIR -mindepth 1 -delete",
]
`))
	if err != nil {
		t.Fatalf("a file carrying the documentation's own [hooks] table was refused: %s", err)
	}
	if !p.HooksSkipped {
		t.Fatalf("the [hooks] table was read without saying none of it runs")
	}
	if p.PidsLimit != 300 {
		t.Fatalf("the settings before [hooks] were not read: pids_limit is %d", p.PidsLimit)
	}

	// An empty table is still a table somebody wrote.
	p, err = LoadPolicy(writePolicyFile(t, "[hooks]\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}
	if !p.HooksSkipped {
		t.Fatalf("an empty [hooks] table was not noticed")
	}
}

// TOML puts every key after a table's header inside that table, so a setting appended
// below the [hooks] the documentation's example ends with is a key of [hooks]. Taken as a
// hook's, it would be dropped: a runner starting green with none of the confinement its
// operator believes is in force. It is refused, and the refusal says where it belongs.
func TestLoadPolicyRefusesASettingWrittenBelowTheHooksHeader(t *testing.T) {
	for _, c := range []struct {
		body string
		want []string
	}{
		{
			"[hooks]\ntimeout = \"30s\"\npre_task = [\"nvidia-smi -L\"]\nmemory_cap = \"8Gi\"\napparmor_profile = \"agentiik-brick\"\n",
			[]string{"hooks.memory_cap on line 4 and hooks.apparmor_profile on line 5", "memory_cap and apparmor_profile are settings of the runner written below the header of [hooks]", "above the first table"},
		},
		{
			"[hooks]\nrequire_userns_remap = false\n",
			[]string{"hooks.require_userns_remap on line 2", "require_userns_remap is a setting of the runner written below the header of [hooks]"},
		},
		{
			"[ulimits]\nnofile = { soft = 1, hard = 2 }\npids_limit = 64\n",
			[]string{"ulimits.pids_limit on line 3", "pids_limit is a setting of the runner written below the header of [ulimits]"},
		},
		// A key no hook has is refused as any other is, and is not taken for a setting.
		{"[hooks]\npids_limt = 64\n", []string{"hooks.pids_limt on line 2", "is not a setting"}},
		{"[hooks]\npre_tasks = []\n", []string{"hooks.pre_tasks on line 2"}},
		{"[hooks.v0_9_0]\nanything = { at = \"all\" }\n", []string{"hooks.v0_9_0"}},
	} {
		p, err := LoadPolicy(writePolicyFile(t, c.body))
		if err == nil {
			t.Fatalf("%q was accepted", c.body)
		}
		for _, want := range c.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %q does not say %q: %s", c.body, want, err)
			}
		}
		if strings.Contains(c.body, "pids_limt") && strings.Contains(err.Error(), "written below the header") {
			t.Errorf("a misspelling under [hooks] was taken for a setting written below it: %s", err)
		}
		if p.RequireUsernsRemap.Lifted() {
			t.Errorf("%q came back with a lifted floor", c.body)
		}
	}
}

// A key of [hooks] is held to its type, as a setting is, though none of it runs.
func TestLoadPolicyHoldsAHookToItsType(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"[hooks]\ntimeout = 30\n", "line 2: hooks.timeout is a duration in quotation marks"},
		{"[hooks]\npre_task = \"nvidia-smi -L\"\n", "line 2: hooks.pre_task is a list of commands"},
		{"[hooks]\npost_task = [1]\n", "line 2: hooks.post_task is a list of commands"},
	} {
		if _, err := LoadPolicy(writePolicyFile(t, c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("the refusal of %q does not say %q: %v", c.body, c.want, err)
		}
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

// secrets_dir named the host tmpfs a runner once wrote secret values on, and a task's values
// are on a tmpfs volume of its own now. A file written for the version before this one still
// starts its runner, and the runner says once that the line can go, naming the file.
func TestASecretsDirIsReadAndSaidToBeUnused(t *testing.T) {
	path := writePolicyFile(t, "require_userns_remap = false\nsecrets_dir = \"/run/agentiik/secrets\"\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("a file naming a secrets_dir was refused: %s", err)
	}
	if p.SecretsDirSkipped != "/run/agentiik/secrets" {
		t.Fatalf("the secrets_dir was read as %q, and it is said to be unused by its path", p.SecretsDirSkipped)
	}

	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	var said []string
	d, err := New(Config{Socket: daemon.Socket(), Policy: p, WorkRoot: t.TempDir(), Host: holding(0), Announce: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatalf("a runner whose file names a secrets_dir was refused: %s", err)
	}
	d.Close()
	n := 0
	for _, s := range said {
		if strings.Contains(s, path+" names a secrets_dir") && strings.Contains(s, "the line can go") && strings.Contains(s, "/run/agentiik/secrets/agentiik") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the unused secrets_dir was said %d times: %v", n, said)
	}
}
