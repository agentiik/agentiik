package driver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// scriptTask is a step written as a script, with the three keyword lists a caller fills
// in. The image is a base image, as it is for every script step.
func scriptTask(before, script, after []string) graph.Task {
	return graph.Task{
		ID:          agk.NewTaskID("01JMZ8W4K7A1B2C3D4E5F6G7H8", "check-vat", 1, agk.Shard{}),
		Run:         "01JMZ8W4K7A1B2C3D4E5F6G7H8",
		Step:        "check-vat",
		Attempt:     1,
		Image:       "alpine:3.21@sha256:5f8b1ec704",
		Outputs:     []agk.Port{"out"},
		Script:      script,
		AfterScript: after,

		BeforeScript: before,
	}
}

// runProgram runs the composed command the way the container would run it, on this
// machine's own shell. The three keywords become one invocation, so what is asserted here
// is the invocation itself rather than a transcription of it.
func runProgram(t *testing.T, task graph.Task, policy []string) (code int, stdout, stderr string) {
	t.Helper()
	entrypoint, cmd := scriptCommand(task, policy)
	if len(entrypoint) == 0 {
		t.Fatal("the step is a script and it composed no command")
	}
	if _, err := os.Stat(entrypoint[0]); err != nil {
		t.Skipf("no %s on this machine to run the program with", entrypoint[0])
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, entrypoint[0], append(entrypoint[1:], cmd...)...)
	var out, errs strings.Builder
	c.Stdout, c.Stderr = &out, &errs
	err := c.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running the program: %v", err)
	}
	return code, out.String(), errs.String()
}

func TestAStepThatNamesNoShellGetsTheDefault(t *testing.T) {
	entrypoint, cmd := scriptCommand(scriptTask(nil, []string{"true"}, nil), nil)
	if !slices.Equal(entrypoint, DefaultShell) {
		t.Errorf("entry point: got %v, want %v", entrypoint, DefaultShell)
	}
	if len(cmd) != 1 {
		t.Fatalf("command: got %v, want the program as one argument", cmd)
	}
}

func TestTheStepsOwnShellWinsOverThePolicysAndThePolicysOverTheDefault(t *testing.T) {
	task := scriptTask(nil, []string{"true"}, nil)
	task.Shell = []string{"/bin/sh", "-eu", "-o", "pipefail"}
	policy := []string{"/bin/bash", "-e"}

	entrypoint, _ := scriptCommand(task, policy)
	want := []string{"/bin/sh", "-eu", "-o", "pipefail", "-c"}
	if !slices.Equal(entrypoint, want) {
		t.Errorf("entry point: got %v, want the step's own shell with the flag that takes a program, %v", entrypoint, want)
	}

	task.Shell = nil
	entrypoint, _ = scriptCommand(task, policy)
	if want := []string{"/bin/bash", "-e", "-c"}; !slices.Equal(entrypoint, want) {
		t.Errorf("entry point: got %v, want the policy's shell, %v", entrypoint, want)
	}
}

func TestAShellThatAlreadyTakesAProgramIsNotGivenTheFlagTwice(t *testing.T) {
	task := scriptTask(nil, []string{"true"}, nil)
	task.Shell = []string{"/bin/sh", "-c"}

	entrypoint, _ := scriptCommand(task, nil)
	if want := []string{"/bin/sh", "-c"}; !slices.Equal(entrypoint, want) {
		t.Errorf("entry point: got %v, want %v", entrypoint, want)
	}
}

func TestResolvingTheShellDoesNotWriteIntoTheDefault(t *testing.T) {
	before := slices.Clone(DefaultShell)
	for range 2 {
		scriptCommand(scriptTask(nil, []string{"true"}, nil), nil)
	}
	if !slices.Equal(DefaultShell, before) {
		t.Errorf("DefaultShell: got %v, want %v", DefaultShell, before)
	}
}

func TestABrickStepComposesNoCommand(t *testing.T) {
	task := graph.Task{Step: "normalize", Image: "ghcr.io/acme/agk-normalize@sha256:9f2c1db7e0"}
	entrypoint, cmd := scriptCommand(task, nil)
	if entrypoint != nil || cmd != nil {
		t.Errorf("got %v %v, want nothing: what runs in a brick is the entry point its image declares", entrypoint, cmd)
	}
}

func TestTheThreeKeywordsRunInOrderInOneInvocation(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	task := scriptTask(
		[]string{"printf b >> " + marks},
		[]string{"printf s >> " + marks},
		[]string{"printf a >> " + marks},
	)

	code, _, _ := runProgram(t, task, nil)
	if code != 0 {
		t.Errorf("exit: got %d, want 0", code)
	}
	if got := markFile(t, marks); got != "bsa" {
		t.Errorf("order: got %q, want before_script, then script, then after_script", got)
	}
}

func TestTheFirstNonZeroExitEndsTheStepWithThatCode(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	task := scriptTask(nil, []string{
		"printf 1 >> " + marks,
		"exit 42",
		"printf 3 >> " + marks,
	}, nil)

	code, _, _ := runProgram(t, task, nil)
	if code != 42 {
		t.Errorf("exit: got %d, want 42, the code of the command that failed", code)
	}
	if got := markFile(t, marks); got != "1" {
		t.Errorf("commands run: got %q, want only the ones before the failure", got)
	}
}

func TestAFailingCommandIsTheStepsCodeUnderTheDefaultShell(t *testing.T) {
	// Not exit, which ends the shell whatever its flags: an ordinary command
	// failing is what -e makes the end of the step, and the default carries it.
	task := scriptTask(nil, []string{"false", "printf ran"}, nil)

	code, stdout, _ := runProgram(t, task, nil)
	if code != 1 {
		t.Errorf("exit: got %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("standard output: got %q, want nothing after the first non-zero exit", stdout)
	}
}

func TestAfterScriptRunsWhenScriptFailedAndDoesNotChangeTheVerdict(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "dump")
	task := scriptTask(nil, []string{"exit 7"}, []string{"printf dumped > " + dump})

	code, _, _ := runProgram(t, task, nil)
	if code != 7 {
		t.Errorf("exit: got %d, want 7, the code script failed with", code)
	}
	if got := markFile(t, dump); got != "dumped" {
		t.Errorf("after_script: got %q, want the diagnostic dump to survive the failure", got)
	}
}

func TestAfterScriptsOwnExitCodeIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	task := scriptTask(nil, []string{"true"}, []string{
		"exit_code_of_a_command_that_does_not_exist",
		"false",
		"printf ran >> " + marks,
	})

	code, _, _ := runProgram(t, task, nil)
	if code != 0 {
		t.Errorf("exit: got %d, want 0: after_script's own exit code does not change the step's verdict", code)
	}
	if got := markFile(t, marks); got != "ran" {
		t.Errorf("after_script: got %q, want it to run on past a failing diagnostic", got)
	}
}

func TestAfterScriptRunsWhenBeforeScriptFailed(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	task := scriptTask(
		[]string{"exit 9"},
		[]string{"printf s >> " + marks},
		[]string{"printf a >> " + marks},
	)

	code, _, _ := runProgram(t, task, nil)
	if code != 9 {
		t.Errorf("exit: got %d, want 9, the code before_script failed with", code)
	}
	if got := markFile(t, marks); got != "a" {
		t.Errorf("marks: got %q, want after_script to have run and script not to have", got)
	}
}

func TestAStepWithNoAfterScriptIsJustItsCommands(t *testing.T) {
	task := scriptTask([]string{"printf b"}, []string{"printf s"}, nil)
	_, cmd := scriptCommand(task, nil)
	if strings.Contains(cmd[0], afterFunc) {
		t.Errorf("program: got %q, want no handler where there is nothing to run whatever happened", cmd[0])
	}

	code, stdout, _ := runProgram(t, task, nil)
	if code != 0 || stdout != "bs" {
		t.Errorf("got exit %d and %q, want 0 and \"bs\"", code, stdout)
	}
}

func TestACommandKeepsItsOwnTextIncludingAHereDocument(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "written")
	task := scriptTask(nil, []string{"cat > " + out + " <<'EOF'\none\ntwo\nEOF"}, []string{"printf done"})

	code, stdout, stderr := runProgram(t, task, nil)
	if code != 0 {
		t.Fatalf("exit: got %d, want 0: %s", code, stderr)
	}
	if got := markFile(t, out); got != "one\ntwo\n" {
		t.Errorf("the here-document: got %q, want the two lines the command wrote", got)
	}
	if stdout != "done" {
		t.Errorf("standard output: got %q, want after_script to have run", stdout)
	}
}

func TestStandardOutputAndStandardErrorStayApart(t *testing.T) {
	task := scriptTask(nil, []string{"printf payload", "printf trouble >&2"}, nil)

	code, stdout, stderr := runProgram(t, task, nil)
	if code != 0 {
		t.Fatalf("exit: got %d, want 0", code)
	}
	if stdout != "payload" {
		t.Errorf("standard output: got %q, want what the shorthand captures", stdout)
	}
	if stderr != "trouble" {
		t.Errorf("standard error: got %q, want what the log keeps", stderr)
	}
}

// The shorthand.

// collected is what brick.Collect returns for a step that wrote nothing: one empty
// envelope per declared port.
func emptyPorts(t graph.Task, ports ...agk.Port) map[agk.Port]agk.Envelope {
	at := time.Date(2026, 9, 10, 6, 0, 12, 0, time.UTC)
	out := make(map[agk.Port]agk.Envelope, len(ports))
	for _, p := range ports {
		out[p] = agk.Empty(t.Run, t.Step, p, t.Attempt, at)
	}
	return out
}

func anOutputFile(name string) agk.File {
	return agk.File{
		Name:      name,
		URI:       agk.URI{Run: "01JMZ8W4K7A1B2C3D4E5F6G7H8", Step: "check-vat", Port: "out", Name: name},
		MediaType: "application/json",
		Size:      12,
		SHA256:    strings.Repeat("ab", 32),
	}
}

func TestAScriptThatWroteNothingAndExitedZeroPublishesItsStandardOutput(t *testing.T) {
	task := scriptTask(nil, []string{"echo hello"}, nil)
	files := []agk.File{anOutputFile("result.json")}

	out := stdoutShorthand(task, 0, emptyPorts(task, "out"), []byte("hello\n"), files)

	e := out["out"]
	if len(e.Items) != 1 {
		t.Fatalf("items: got %d, want the one item the shorthand publishes", len(e.Items))
	}
	item := e.Items[0]
	if got := item.Data[StdoutField]; got != "hello\n" {
		t.Errorf("data.%s: got %q, want the captured standard output as it arrived", StdoutField, got)
	}
	if item.ID == "" {
		t.Error("the item carries no identifier")
	}
	if len(item.Files) != 1 || item.Files[0].Name != "result.json" {
		t.Errorf("files: got %v, want what the script left in /agk/out/files/", item.Files)
	}
	if e.Meta.Count != 1 {
		t.Errorf("meta.count: got %d, want 1: count is how many items the envelope holds", e.Meta.Count)
	}
	if err := e.Validate(agk.DefaultLimits()); err != nil {
		t.Errorf("the envelope the shorthand published is not one that travels: %v", err)
	}
}

func TestTheShorthandCarriesNoFilesAsAnEmptyList(t *testing.T) {
	task := scriptTask(nil, []string{"echo hello"}, nil)

	out := stdoutShorthand(task, 0, emptyPorts(task, "out"), []byte("hello\n"), nil)

	if files := out["out"].Items[0].Files; files == nil || len(files) != 0 {
		t.Errorf("files: got %v, want the empty list an item with nothing attached carries", files)
	}
}

func TestTheShorthandLeavesTheOtherPortsEmpty(t *testing.T) {
	task := scriptTask(nil, []string{"echo hello"}, nil)
	task.Outputs = []agk.Port{"out", "error"}

	out := stdoutShorthand(task, 0, emptyPorts(task, "out", "error"), []byte("hello\n"), nil)

	if len(out["error"].Items) != 0 {
		t.Errorf("error: got %d items, want the empty envelope a port nobody wrote publishes", len(out["error"].Items))
	}
}

func TestAScriptThatWroteAPortGetsNoShorthand(t *testing.T) {
	task := scriptTask(nil, []string{"agk emit out --from /tmp/result.json"}, nil)
	in := emptyPorts(task, "out")
	e := in["out"]
	e.Items = []agk.Item{agk.NewItem(map[string]any{"vat_number": "FR12345678901"})}
	e.Meta.Count = 1
	in["out"] = e

	out := stdoutShorthand(task, 0, in, []byte("noise\n"), nil)

	if len(out["out"].Items) != 1 || out["out"].Items[0].Data["vat_number"] != "FR12345678901" {
		t.Errorf("items: got %v, want what the script published and nothing added to it", out["out"].Items)
	}
}

func TestAScriptThatFailedGetsNoShorthand(t *testing.T) {
	task := scriptTask(nil, []string{"exit 1"}, nil)

	out := stdoutShorthand(task, 1, emptyPorts(task, "out"), []byte("half a line"), nil)

	if len(out["out"].Items) != 0 {
		t.Errorf("items: got %d, want none: the shorthand is what a script that exited 0 publishes", len(out["out"].Items))
	}
}

func TestABrickThatWroteNothingGetsNoShorthand(t *testing.T) {
	task := graph.Task{
		Run:     "01JMZ8W4K7A1B2C3D4E5F6G7H8",
		Step:    "normalize",
		Attempt: 1,
		Image:   "ghcr.io/acme/agk-normalize@sha256:9f2c1db7e0",
		Outputs: []agk.Port{"out"},
	}

	out := stdoutShorthand(task, 0, emptyPorts(task, "out"), []byte("a brick talking to its log"), nil)

	if len(out["out"].Items) != 0 {
		t.Errorf("items: got %d, want none: a brick that wrote nothing publishes empty envelopes", len(out["out"].Items))
	}
}

func TestAScriptDeclaringNoOutPortGetsNoShorthand(t *testing.T) {
	task := scriptTask(nil, []string{"echo hello"}, nil)
	task.Outputs = []agk.Port{"ok", "rejected"}

	out := stdoutShorthand(task, 0, emptyPorts(task, "ok", "rejected"), []byte("hello\n"), nil)

	for port, e := range out {
		if len(e.Items) != 0 {
			t.Errorf("%s: got %d items, want none: the shorthand publishes on out and no other port", port, len(e.Items))
		}
	}
}

func markFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
