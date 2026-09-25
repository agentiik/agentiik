package driver

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

func settingsTask() graph.Task {
	return graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "invoice", 2, agk.Shard{Index: 3, Of: 8}),
		Run:       "01JMZ8V1P9C4",
		Namespace: "finance",
		Step:      "invoice",
		Attempt:   2,
		Shard:     agk.Shard{Index: 3, Of: 8},
		Resources: graph.Resources{CPU: "0.5", Memory: "256Mi", PIDs: 128},
	}
}

func settings(t *testing.T, task graph.Task, p Policy, mode string) docker.HostConfig {
	t.Helper()
	g := &given{
		Mounts: []docker.Mount{bind("/var/lib/agentiik/work/task/out", brick.OutDir, false)},
		Tmpfs:  map[string]string{TmpDir: tmpfsOptions(p.TmpSize)},
	}
	h, err := hostConfig(task, p, g, mode)
	if err != nil {
		t.Fatalf("hostConfig: %s", err)
	}
	return h
}

// The settings table of the documentation, row by row, read back off the JSON that goes
// on the wire rather than off the struct, because a field with the wrong name on the
// wire is a setting the daemon never applied.
func TestTheSettingsAppliedToEveryContainer(t *testing.T) {
	h := settings(t, settingsTask(), DefaultPolicy(), networkModeNone)

	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshalling the host configuration: %s", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("reading the host configuration back: %s", err)
	}

	if wire["NetworkMode"] != networkModeNone {
		t.Errorf("NetworkMode is %v, and the default of the table is none", wire["NetworkMode"])
	}
	if wire["ReadonlyRootfs"] != true {
		t.Errorf("ReadonlyRootfs is %v", wire["ReadonlyRootfs"])
	}
	if got := strings.Join(strings.Fields(stringsOf(t, wire["CapDrop"])), ","); got != "ALL" {
		t.Errorf("CapDrop is %v, and the table says ALL", wire["CapDrop"])
	}
	if _, ok := wire["CapAdd"]; ok {
		t.Errorf("CapAdd is %v: it is refused unless the runner policy allows it explicitly, and nothing asked", wire["CapAdd"])
	}
	if opt := stringsOf(t, wire["SecurityOpt"]); !strings.Contains(opt, "no-new-privileges") {
		t.Errorf("SecurityOpt is %q, with no no-new-privileges in it", opt)
	}
	if wire["AutoRemove"] != false {
		t.Errorf("AutoRemove is %v: the driver removes the container itself once the logs and the exit code are collected, so that nothing is lost on a fast exit", wire["AutoRemove"])
	}
	if wire["Memory"] != float64(256<<20) {
		t.Errorf("Memory is %v, want %d, which is 256Mi", wire["Memory"], 256<<20)
	}
	if wire["NanoCpus"] != float64(500_000_000) {
		t.Errorf("NanoCpus is %v, want half a core", wire["NanoCpus"])
	}
	if wire["PidsLimit"] != float64(128) {
		t.Errorf("PidsLimit is %v, and the step asked for 128", wire["PidsLimit"])
	}
	tmpfs, ok := wire["Tmpfs"].(map[string]any)
	if !ok || tmpfs[TmpDir] == nil {
		t.Errorf("Tmpfs is %v, with no %s in it", wire["Tmpfs"], TmpDir)
	}
	limits, ok := wire["Ulimits"].([]any)
	if !ok || len(limits) != 2 {
		t.Fatalf("Ulimits is %v, and the table names nofile and nproc", wire["Ulimits"])
	}
	names := map[string]bool{}
	for _, limit := range limits {
		names[limit.(map[string]any)["Name"].(string)] = true
	}
	if !names["nofile"] || !names["nproc"] {
		t.Errorf("Ulimits carries %v", names)
	}
	if wire["Mounts"] == nil {
		t.Errorf("Mounts is empty, and every brick receives bind mounts under /agk")
	}
}

// "PidsLimit: from resources.pids, default 256." A step that asks for nothing gets the
// number the table names, and never the daemon's unlimited.
func TestPidsDefaultsToTheNumberTheTableNames(t *testing.T) {
	task := settingsTask()
	task.Resources.PIDs = 0
	h := settings(t, task, DefaultPolicy(), networkModeNone)
	if h.PidsLimit == nil {
		t.Fatalf("PidsLimit is unset, which the daemon reads as unlimited")
	}
	if *h.PidsLimit != 256 {
		t.Fatalf("PidsLimit is %d, want 256", *h.PidsLimit)
	}
}

// "Capped by the runner policy and the namespace quota." The quota is applied where
// quotas live; this is the runner's half, and it caps a step that asked for more as well
// as one that asked for nothing at all.
func TestTheRunnerPolicyCapsWhatAStepAsksFor(t *testing.T) {
	p := DefaultPolicy()
	p.MemoryCap = 128 << 20
	p.CPUCap = 0.25
	p.PidsLimit = 64

	task := settingsTask()
	h := settings(t, task, p, networkModeNone)
	if h.Memory != 128<<20 {
		t.Errorf("Memory is %d, and the policy caps at %d", h.Memory, p.MemoryCap)
	}
	if h.NanoCPUs != 250_000_000 {
		t.Errorf("NanoCpus is %d, and the policy caps at a quarter of a core", h.NanoCPUs)
	}
	if *h.PidsLimit != 64 {
		t.Errorf("PidsLimit is %d, and the policy caps at %d", *h.PidsLimit, p.PidsLimit)
	}

	// A step that asks for nothing gets the ceiling, because a ceiling that only
	// applied to steps naming a number would not be a ceiling.
	task.Resources = graph.Resources{}
	h = settings(t, task, p, networkModeNone)
	if h.Memory != 128<<20 || h.NanoCPUs != 250_000_000 {
		t.Errorf("a step that asked for nothing got Memory %d and NanoCpus %d", h.Memory, h.NanoCPUs)
	}
}

// A policy nobody filled in caps nothing and sets no limit, which is what lets a Policy{}
// be a usable value on a laptop.
func TestAPolicyWithNoCeilingsSetsNoLimit(t *testing.T) {
	task := settingsTask()
	task.Resources = graph.Resources{}
	h := settings(t, task, Policy{}, networkModeNone)
	if h.Memory != 0 || h.NanoCPUs != 0 {
		t.Errorf("Memory is %d and NanoCpus is %d with no policy and no request", h.Memory, h.NanoCPUs)
	}
	if h.PidsLimit != nil {
		t.Errorf("PidsLimit is %d with no policy and no request", *h.PidsLimit)
	}
	if len(h.Ulimits) != 0 {
		t.Errorf("Ulimits is %v with no policy", h.Ulimits)
	}
}

// Memory is "a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so that
// 512Mi cannot be read as 512 bytes". A value that got past the parser and arrives here
// malformed is refused rather than read as bytes.
func TestMemoryWrittenWithoutABinarySuffixIsRefused(t *testing.T) {
	task := settingsTask()
	task.Resources.Memory = "512M"
	_, err := hostConfig(task, DefaultPolicy(), &given{}, networkModeNone)
	if err == nil {
		t.Fatalf("512M was accepted")
	}
	if !strings.Contains(err.Error(), "invoice") || !strings.Contains(err.Error(), "512Mi") {
		t.Fatalf("the refusal names neither the step nor the rule: %s", err)
	}
	// A number the workflow file wrote is not the runtime's failure, and charging it
	// to the platform would retry a step that will be refused the same way next time.
	if charge, ok := Charged(err); !ok || charge != ChargeBrick {
		t.Fatalf("the refusal is charged to %s, decided %v", charge, ok)
	}
}

func TestCPUNotWrittenAsTheManifestWritesItIsRefused(t *testing.T) {
	task := settingsTask()
	task.Resources.CPU = "half"
	if _, err := hostConfig(task, DefaultPolicy(), &given{}, networkModeNone); err == nil {
		t.Fatalf("a cpu of half was accepted")
	} else if !strings.Contains(err.Error(), "0.5") {
		t.Fatalf("the refusal does not quote the rule: %s", err)
	}
}

// The profiles of the SecurityOpt row are named only where the policy names one, because
// a daemon applies its own defaults and an empty profile name would replace a default
// that is already the right answer with nothing at all.
func TestTheSecurityProfilesComeFromThePolicy(t *testing.T) {
	p := DefaultPolicy()
	p.Seccomp = `{"defaultAction":"SCMP_ACT_ERRNO"}`
	p.AppArmor = "agentiik-brick"
	p.SELinuxLabel = "level:s0:c100,c200"

	opt := strings.Join(securityOptions(p), " ")
	for _, want := range []string{"no-new-privileges:true", `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`, "apparmor=agentiik-brick", "label=level:s0:c100,c200"} {
		if !strings.Contains(opt, want) {
			t.Errorf("SecurityOpt is %q, with no %s in it", opt, want)
		}
	}
	if got := securityOptions(Policy{}); len(got) != 1 || got[0] != "no-new-privileges:true" {
		t.Errorf("a policy naming no profile gives %v, and the daemon's own defaults are what it should leave alone", got)
	}
}

// The create carries the seccomp profile a runner's file names as the JSON in the file,
// because that is what the Engine API decodes: seccomp=<path> is the docker command
// reading the file on its caller's behalf, and a daemon handed the path fails to decode it
// as a profile when the container starts. The profile goes through LoadPolicy and onto
// the wire, so the path from the file to the create is the one under test.
func TestTheCreateCarriesTheSeccompProfileAsJSON(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "seccomp.json")
	written := "{\n  \"defaultAction\": \"SCMP_ACT_ALLOW\",\n  \"syscalls\": [\n    { \"names\": [\"mkdirat\"], \"action\": \"SCMP_ACT_ERRNO\" }\n  ]\n}\n"
	if err := os.WriteFile(profile, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(writePolicyFile(t, "seccomp_profile = \""+profile+"\"\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %s", err)
	}

	h := settings(t, settingsTask(), p, networkModeNone)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshalling the host configuration: %s", err)
	}
	var wire struct{ SecurityOpt []string }
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("reading the host configuration back: %s", err)
	}

	var sent string
	for _, opt := range wire.SecurityOpt {
		if v, ok := strings.CutPrefix(opt, "seccomp="); ok {
			sent = v
		}
	}
	if sent == "" {
		t.Fatalf("SecurityOpt is %q, with no seccomp in it", wire.SecurityOpt)
	}
	if strings.Contains(sent, profile) {
		t.Fatalf("the create carries the path of the profile, which the daemon cannot decode: %s", sent)
	}
	want := `{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["mkdirat"],"action":"SCMP_ACT_ERRNO"}]}`
	if sent != want {
		t.Fatalf("the create carries seccomp=%s, and the file holds %s", sent, want)
	}
}

// The container half: standard input is open because "the envelope on standard input is
// half of what a brick is given", and there is no pseudo-terminal because one would merge
// the two output streams that the shorthand and the masked log need kept apart.
func TestTheContainerHalfOpensStandardInputAndNoTerminal(t *testing.T) {
	c := containerConfig(settingsTask(), "ghcr.io/acme/agk-invoice@sha256:1ab74e", "65532:65532", []string{"AGK_STEP=invoice"}, nil, nil)

	if !c.OpenStdin || !c.StdinOnce || !c.AttachStdin || !c.AttachStdout || !c.AttachStderr {
		t.Fatalf("the standard streams are not all attached: %+v", c)
	}
	if c.Tty {
		t.Fatalf("a pseudo-terminal was asked for, which merges standard output and standard error")
	}
	if c.User != "65532:65532" {
		t.Fatalf("User is %q: the account is the image's, required by the manifest", c.User)
	}
	if c.WorkingDir != "" {
		t.Fatalf("WorkingDir is %q: the image declares one and a brick's entry point is written against it", c.WorkingDir)
	}
}

// The task identifier is the label a redelivered task, a Stop and a startup sweep all
// resolve through, so it is the one that has to be there.
func TestTheLabelsCarryTheTaskIdentity(t *testing.T) {
	l := labels(settingsTask())
	for name, want := range map[string]string{
		LabelTask:      "01JMZ8V1P9C4/invoice/2/3/8",
		LabelRun:       "01JMZ8V1P9C4",
		LabelNamespace: "finance",
		LabelStep:      "invoice",
		LabelAttempt:   "2",
		LabelShard:     "3/8",
	} {
		if l[name] != want {
			t.Errorf("%s is %q, want %q", name, l[name], want)
		}
	}
	if !strings.HasPrefix(LabelTask, "dev.agentiik.") {
		t.Errorf("%s is not under the project's own domain", LabelTask)
	}
}

// A shard that does not exist carries no label, on the reading AGK_SHARD already takes.
func TestAStepWithNoFanOutCarriesNoShardLabel(t *testing.T) {
	task := settingsTask()
	task.Shard = agk.Shard{}
	if value, ok := labels(task)[LabelShard]; ok {
		t.Fatalf("%s is %q on a step with no fan-out", LabelShard, value)
	}
}

// The container joins its own network at creation and not afterwards, because a
// container connected after it started has already run on the default bridge for as long
// as the two calls took.
func TestTheContainerJoinsItsOwnNetworkAtCreation(t *testing.T) {
	if got := networkingConfig(networkModeNone); len(got.EndpointsConfig) != 0 {
		t.Fatalf("the none posture asked for an endpoint: %v", got)
	}
	got := networkingConfig("agk-01JMZ8V1P9C4_invoice_2_3_8")
	if len(got.EndpointsConfig) != 1 {
		t.Fatalf("a task joined %d networks", len(got.EndpointsConfig))
	}
	if _, ok := got.EndpointsConfig["agk-01JMZ8V1P9C4_invoice_2_3_8"]; !ok {
		t.Fatalf("the container joined %v", got.EndpointsConfig)
	}
}

// stringsOf prints a JSON array of strings for a message, and fails where it is not one.
func stringsOf(t *testing.T, v any) string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a list", v)
	}
	var out []string
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%v is not a list of strings", v)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// A cpu the grammar reads and no host has is counted as the most there is, never as a number below
// zero, which a runner counting it against its capacity would read as room given back.
func TestACPUNoHostHasIsCountedAsTheMostThereIs(t *testing.T) {
	_, nanos, err := Declared("invoice", "", "10000000000")
	if err != nil {
		t.Fatal(err)
	}
	if nanos != math.MaxInt64 {
		t.Errorf("ten billion cores are %d billionths of a core", nanos)
	}
}
