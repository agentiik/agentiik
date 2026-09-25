package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/cmd/agk/internal/local"
	"github.com/agentiik/agentiik/graph"
)

// What agk run --local can be held to with no daemon in reach is everything it does before it
// reaches one, which is deliberately most of what can go wrong: the flags, the file, the
// inputs and the secrets. The loop itself is held in cmd/agk/internal/local, and the whole
// command against the real daemon is the milestone test.

// runner drives the command line the way a person does, and answers with the exit code and the
// two streams.
func runner(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := run(context.Background(), Env{
		Out: &out, Err: &errs, Dir: dir,
		Now:        func() time.Time { return time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) },
		Getenv:     func(string) string { return "" },
		Executable: os.Executable,
	}, args)
	return code, out.String(), errs.String()
}

func TestTheTableCarriesTheRunCommandWhereTheDocumentationWritesIt(t *testing.T) {
	var at, push int = -1, -1
	for i, c := range commands {
		switch c.name {
		case "run":
			at = i
		case "push":
			push = i
		}
	}
	if at < 0 {
		t.Fatalf("the table has no run command: %v", commands)
	}
	if at != push-1 {
		t.Errorf("run is row %d and push is row %d: the documentation writes run between graph and push, and the usage text is this table printed", at, push)
	}

	// The usage text is the table, so the row appears in it without anything being
	// restated.
	_, out, _ := runner(t, t.TempDir(), "--help")
	if !strings.Contains(out, "run ") || !strings.Contains(out, "local Docker daemon") {
		t.Errorf("the usage text does not carry the run row:\n%s", out)
	}
}

// A run is local or on an installation, and one naming neither is a command line missing the flag
// that says which, as a push missing its namespace is.
func TestARunNamingNeitherKindIsACommandLineError(t *testing.T) {
	code, out, errs := runner(t, t.TempDir(), "run")
	if code != exitUsage {
		t.Errorf("the exit code is %d, want %d: the command line does not say where to run", code, exitUsage)
	}
	if out != "" {
		t.Errorf("a refusal reached standard output: %q", out)
	}
	if !strings.Contains(errs, "--local") || !strings.Contains(errs, "--namespace") {
		t.Errorf("the refusal is %q, and it has to name both flags that run something", errs)
	}
}

func TestARunWithAFormatThatIsNotJSONIsACommandLineError(t *testing.T) {
	code, _, errs := runner(t, t.TempDir(), "run", "--local", "-o", "yaml")
	if code != exitUsage {
		t.Errorf("the exit code is %d, want %d, which is flag's own", code, exitUsage)
	}
	if !strings.Contains(errs, "json") {
		t.Errorf("the refusal is %q, and it has to name the one format there is", errs)
	}
}

func TestAnInputFlagThatIsNotAPairIsACommandLineError(t *testing.T) {
	code, _, errs := runner(t, t.TempDir(), "run", "--local", "--input", "cycle")
	if code != exitUsage {
		t.Errorf("the exit code is %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errs, "name=value") {
		t.Errorf("the refusal is %q, and it has to say how the flag is written", errs)
	}
}

func TestAMissingEntryPointIsRefusedAndNamesTheFile(t *testing.T) {
	code, _, errs := runner(t, t.TempDir(), "run", "--local")
	if code != exitRefused {
		t.Errorf("the exit code is %d, want %d", code, exitRefused)
	}
	if !strings.Contains(errs, entryPoint) {
		t.Errorf("the refusal is %q, and it has to name the file that is not there", errs)
	}
}

func TestASecretNobodySuppliedRefusesBeforeADaemonIsTouched(t *testing.T) {
	dir := filepath.Join("testdata", "needs-a-secret")
	code, _, errs := runner(t, dir, "run", "--local", "--input", "cycle=2026-01")
	if code != exitRefused {
		t.Fatalf("the exit code is %d, want %d: nothing ran\n%s", code, exitRefused, errs)
	}
	for _, want := range []string{"billing_api", "charge", "--secret"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal is %q, and %s is missing from it", errs, want)
		}
	}
	// Before the daemon: the sentence the probe prints is not there, so nothing was
	// dialled to find this out.
	if strings.Contains(errs, "speaks API") {
		t.Errorf("a daemon was probed before the secrets were checked:\n%s", errs)
	}
}

func TestARequiredInputNobodySuppliedRefusesBeforeADaemonIsTouched(t *testing.T) {
	dir := filepath.Join("testdata", "needs-a-secret")
	code, _, errs := runner(t, dir, "run", "--local", "--secret", "billing_api=sk-1")
	if code != exitRefused {
		t.Fatalf("the exit code is %d, want %d\n%s", code, exitRefused, errs)
	}
	if !strings.Contains(errs, "cycle") {
		t.Errorf("the refusal is %q, and it has to name the input", errs)
	}
	if strings.Contains(errs, "speaks API") {
		t.Errorf("a daemon was probed before the inputs were bound:\n%s", errs)
	}
}

func TestAnUndeclaredInputIsRefusedWhereItIsWritten(t *testing.T) {
	dir := filepath.Join("testdata", "needs-a-secret")
	code, _, errs := runner(t, dir, "run", "--local",
		"--input", "cycle=2026-01", "--input", "cicle=2026-01", "--secret", "billing_api=sk-1")
	if code != exitRefused {
		t.Fatalf("the exit code is %d, want %d\n%s", code, exitRefused, errs)
	}
	if !strings.Contains(errs, "cicle") {
		t.Errorf("the refusal is %q, and a misspelled input reports the misspelling", errs)
	}
}

func TestAnInputIsReadAsJSONAndFallsBackToTheStringItIs(t *testing.T) {
	dir := t.TempDir()
	document := filepath.Join(dir, "inputs.json")
	if err := os.WriteFile(document, []byte(`{"cycle":"2025-12","regions":["eu"]}`), 0o600); err != nil {
		t.Fatalf("writing the document: %s", err)
	}
	list := filepath.Join(dir, "orders.json")
	if err := os.WriteFile(list, []byte("[{\"id\":1}]\n"), 0o600); err != nil {
		t.Fatalf("writing the file: %s", err)
	}

	got, err := suppliedInputs(
		[]string{`regions=["eu","us"]`, "cycle=2026-01", "count=3", "dry_run=true", "note=a sentence"},
		[]string{"orders=" + list},
		document)
	if err != nil {
		t.Fatalf("reading the inputs: %s", err)
	}
	want := map[string]any{
		// The document is read first and the flags win, which is what a person
		// retyping a value means by it.
		"cycle": "2026-01",
		// A list is a list, which is the reason the value is read as JSON at all.
		"regions": []any{"eu", "us"},
		// A number is a number and a boolean is a boolean.
		"count":   float64(3),
		"dry_run": true,
		// And anything that is not JSON is the string it is, rather than a parse
		// error.
		"note": "a sentence",
		// A file is read the same way, with the newline an editor left taken off.
		"orders": []any{map[string]any{"id": float64(1)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the inputs are %#v, want %#v", got, want)
	}
}

func TestASecretFileIsReadAsTheBytesItIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	// The newline is part of the file and part of the value: a command line that took it
	// off would hand the container a value that hashes differently from the file beside
	// it.
	if err := os.WriteFile(path, []byte("sk-1\n"), 0o600); err != nil {
		t.Fatalf("writing the file: %s", err)
	}
	e := Env{Dir: dir}
	got, err := suppliedSecrets(e, []string{"typed=sk-2"}, []string{"fromfile=token"})
	if err != nil {
		t.Fatalf("reading the secrets: %s", err)
	}
	if string(got["fromfile"]) != "sk-1\n" {
		t.Errorf("the file's value is %q, want the bytes of the file", got["fromfile"])
	}
	if string(got["typed"]) != "sk-2" {
		t.Errorf("the typed value is %q", got["typed"])
	}

	if _, err := suppliedSecrets(e, nil, []string{"absent=nowhere"}); err == nil {
		t.Errorf("a --secret-file naming a file that is not there was accepted")
	} else if strings.Contains(err.Error(), "sk-") {
		t.Errorf("the refusal carries a value: %s", err)
	}
}

func TestAFailureReportNamesTheStepTheExitCodeAndWhatWasRefused(t *testing.T) {
	var b bytes.Buffer
	failure(&b, local.Failure{
		Step:     "fan",
		Shard:    agk.Shard{Index: 2, Of: 3},
		Attempt:  2,
		State:    agk.TaskFailed,
		HasExit:  true,
		ExitCode: 17,
		Band:     agk.Band(17),
	})
	line := b.String()
	for _, want := range []string{"step fan 2/3", "attempt 2", "exit code 17", "application failure", "failed"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report is %q, and %s is missing", line, want)
		}
	}
	// The step, the exit code and what was refused, in that order, which is #voice and not a
	// preference.
	step, code := strings.Index(line, "step fan"), strings.Index(line, "exit code")
	if step < 0 || code < 0 || step > code {
		t.Errorf("the report is %q, and it has to name the step before the exit code", line)
	}
}

func TestAFailureWithNoContainerSaysThereIsNoExitCode(t *testing.T) {
	var b bytes.Buffer
	failure(&b, local.Failure{
		Step:    "only",
		Attempt: 1,
		State:   agk.TaskFailed,
		Band:    agk.BandRuntimeFailure,
		Refused: "driver: step only: the Docker daemon could not be reached, so no container was created and no exit code exists",
	})
	line := b.String()
	if !strings.Contains(line, "no exit code") {
		t.Errorf("the report is %q: the driver invents no code for a failure that produced no container, and neither does this", line)
	}
	if strings.Contains(line, "driver: ") {
		t.Errorf("the report is %q: the person holding the YAML file has never heard of the packages", line)
	}
	if !strings.Contains(line, "the Docker daemon could not be reached") {
		t.Errorf("the report is %q, and what was refused is the driver's own sentence", line)
	}
}

func TestTheNarrationTellsAStepLineFromAContainerLine(t *testing.T) {
	var b bytes.Buffer
	n := &narration{w: &b, width: len("invoice"), verbose: true}

	// A step transition carries no attempt, because a step is not a task.
	n.say(local.Event{Step: "invoice", Shards: 3, Verdict: agk.VerdictRunning})
	// One container of that step, which is what the mark says.
	n.say(local.Event{Step: "invoice", Shard: agk.Shard{Index: 2, Of: 3}, Shards: 3, Attempt: 1, State: agk.TaskRunning, Verdict: agk.VerdictRunning})
	// And the step ending, with what its ports carry.
	n.say(local.Event{Step: "invoice", Shards: 3, Verdict: agk.VerdictSucceeded, Ports: map[agk.Port]int{"out": 3}})

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines were printed: %q", len(lines), b.String())
	}
	if strings.Contains(lines[0], taskMark) {
		t.Errorf("the step line is %q and carries the mark that means one container of it", lines[0])
	}
	if strings.Contains(lines[2], taskMark) {
		t.Errorf("the step line is %q and carries the mark that means one container of it", lines[2])
	}
	if !strings.Contains(lines[1], taskMark) || !strings.Contains(lines[1], "2/3") {
		t.Errorf("the container line is %q, and it has to say which shard it is", lines[1])
	}
	if !strings.Contains(lines[2], "succeeded") || !strings.Contains(lines[2], "out 3") {
		t.Errorf("the last line is %q, and a control names its effect", lines[2])
	}
	// Identifiers are never prettified: the states and the verdicts are spelled as the
	// YAML and the API spell them.
	for _, line := range lines {
		if strings.Contains(line, "Succeeded") || strings.Contains(line, "Running") {
			t.Errorf("the line %q prettifies a state", line)
		}
	}
}

func TestWithoutVerboseOnlyNewsAboutAContainerIsPrinted(t *testing.T) {
	var b bytes.Buffer
	n := &narration{w: &b, width: 4}

	// A container going about its business is not news when the step lines are already
	// being printed.
	n.say(local.Event{Step: "only", Attempt: 1, State: agk.TaskRunning})
	n.say(local.Event{Step: "only", Attempt: 1, State: agk.TaskPublishing})
	if b.Len() != 0 {
		t.Errorf("a running container was narrated without -v: %q", b.String())
	}
	// A failure is news, and so is the retry that follows it.
	n.say(local.Event{Step: "only", Attempt: 1, State: agk.TaskFailed, ExitCode: 100})
	n.say(local.Event{Step: "only", Attempt: 2, State: agk.TaskPending, NextAttemptAt: time.Date(2026, 1, 1, 9, 0, 30, 0, time.UTC)})
	out := b.String()
	if !strings.Contains(out, "exit code 100") || !strings.Contains(out, "transient failure") {
		t.Errorf("the failure line is %q, and it names the code and the band it landed in", out)
	}
	if !strings.Contains(out, "attempt 2 at 09:00:30") {
		t.Errorf("the retry line is %q, and it says when the next attempt is", out)
	}
}

func TestACancelledRunReportsWhatWasStopped(t *testing.T) {
	var b bytes.Buffer
	reportFailure(&b, local.Outcome{
		Run: agk.Run{Workflow: "finance/slow", State: agk.Cancelled},
		State: &graph.State{
			Run: agk.Run{Workflow: "finance/slow", State: agk.Cancelled},
			Steps: map[agk.Step]graph.StepState{
				"wait":  {Verdict: agk.VerdictRunning, Shards: []graph.ShardState{{Attempt: 1, Task: agk.TaskCancelled}}},
				"after": {Verdict: agk.VerdictPending},
			},
		},
	})
	line := b.String()
	if !strings.Contains(line, "finance/slow cancelled") {
		t.Errorf("the report is %q, and the run state is the answer to what happened", line)
	}
	if !strings.Contains(line, "no step failed") {
		t.Errorf("the report is %q: nothing failed, the containers were stopped", line)
	}
	if !strings.Contains(line, "wait was stopped") {
		t.Errorf("the report is %q, and it has to name the step whose container was called off", line)
	}
	if strings.Contains(line, "after") {
		t.Errorf("the report is %q: a step that never started was not stopped", line)
	}
}

// A report names a file as the person reading it would type it next. A local run writes under
// .agk beside the entry point, so every line of the report otherwise carries the same long
// prefix and the part that differs is at the end, where it is hardest to read.
func TestTheReportNamesAFileTheWayItWouldBeTyped(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "Users", "someone", "invoicing")
	run := filepath.Join(dir, ".agk", "runs", "01M2AA21522VNDK2RHS8TADECD")

	if got, want := under(dir, filepath.Join(run, "outputs", "invoiced.json")),
		filepath.Join(".agk", "runs", "01M2AA21522VNDK2RHS8TADECD", "outputs", "invoiced.json"); got != want {
		t.Errorf("a file inside the working directory reads as %q, not %q", got, want)
	}

	// A --dir elsewhere is the news, so it is printed in full rather than climbed to.
	elsewhere := filepath.Join(string(filepath.Separator), "var", "tmp", "agk", "outputs", "invoiced.json")
	if got := under(dir, elsewhere); got != elsewhere {
		t.Errorf("a file outside the working directory reads as %q, not in full", got)
	}

	// The directory itself is not "." : a report that said the run is in "." would be
	// naming the working directory rather than the run.
	if got := under(dir, dir); got != dir {
		t.Errorf("the working directory itself reads as %q", got)
	}

	// No directory to be relative to is every path in full, which is what a caller that
	// did not set one gets rather than a panic or a guess.
	if got := under("", run); got != run {
		t.Errorf("with no directory, a path reads as %q", got)
	}
}
