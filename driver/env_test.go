package driver

import (
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// The table of the brick contract, read back off a container that would have been
// created. Every row of it is here, because a variable the driver forgets is a variable
// a brick cannot be written against.
func TestEnvironmentTable(t *testing.T) {
	task := graph.Task{
		ID:        agk.NewTaskID("01JMZ8V1P9C4", "invoice", 2, agk.Shard{Index: 3, Of: 8}),
		Run:       "01JMZ8V1P9C4",
		Workflow:  "finance/monthly-invoicing@a3f9c1e",
		Namespace: "finance",
		Commit:    "a3f9c1e",
		Step:      "invoice",
		Attempt:   2,
		Shard:     agk.Shard{Index: 3, Of: 8},
		Outputs:   []agk.Port{"out", "error"},
		Deadline:  time.Date(2026, 9, 10, 6, 12, 0, 0, time.UTC),
	}

	env := envMap(t, environment(task, task.Deadline))
	for name, want := range map[string]string{
		EnvRunID:     "01JMZ8V1P9C4",
		EnvWorkflow:  "finance/monthly-invoicing@a3f9c1e",
		EnvNamespace: "finance",
		EnvStep:      "invoice",
		EnvAttempt:   "2",
		EnvShard:     "3/8",
		EnvDeadline:  "2026-09-10T06:12:00Z",
		EnvRepo:      "/agk/repo",
		EnvCommit:    "a3f9c1e",
		EnvOutPorts:  "out,error",
	} {
		if env[name] != want {
			t.Errorf("%s is %q, want %q", name, env[name], want)
		}
	}
}

// "AGK_SHARD: shard index and cardinality, for example 3/8. Absent when there is no
// fan-out." Absent and not empty: a brick testing for the variable gets one answer.
func TestEnvironmentWithoutAFanOut(t *testing.T) {
	env := envMap(t, environment(graph.Task{Run: "01JMZ8V1P9C4", Step: "invoice", Attempt: 1}, time.Time{}))
	if _, ok := env[EnvShard]; ok {
		t.Fatalf("%s is set to %q on a step with no fan-out", EnvShard, env[EnvShard])
	}
	if env[EnvAttempt] != "1" {
		t.Fatalf("%s is %q on a first attempt: the number is there whether or not it is a retry", EnvAttempt, env[EnvAttempt])
	}
}

// A deadline nobody set is not a deadline of the zero time. The variable is what a brick
// reads to know when it will be stopped, and an empty one would promise a stop at a
// moment nobody named.
func TestEnvironmentWithoutADeadline(t *testing.T) {
	env := envMap(t, environment(graph.Task{Step: "invoice", Attempt: 1}, time.Time{}))
	if _, ok := env[EnvDeadline]; ok {
		t.Fatalf("%s is set to %q on a task with no deadline", EnvDeadline, env[EnvDeadline])
	}
}

// The deadline travels in UTC and in RFC 3339, whatever zone the moment was written in,
// because a brick comparing it with its own clock compares two instants and not two
// spellings.
func TestEnvironmentDeadlineIsUTC(t *testing.T) {
	zone := time.FixedZone("CEST", 2*3600)
	task := graph.Task{Step: "invoice", Attempt: 1, Deadline: time.Date(2026, 9, 10, 8, 12, 0, 0, zone)}
	if got := envMap(t, environment(task, task.Deadline))[EnvDeadline]; got != "2026-09-10T06:12:00Z" {
		t.Fatalf("%s is %q, want the same moment in UTC", EnvDeadline, got)
	}
}

// A task carrying a timeout and no moment is told the moment the timeout lands on, which
// is the one the watch will stop it at. "AGK_DEADLINE: the timestamp past which the
// container will be stopped": a container that is going to be stopped and reads nothing
// there is a stop at a moment nobody named.
func TestEnvironmentDeadlineOfATaskCarryingOnlyATimeout(t *testing.T) {
	dispatched := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)
	task := graph.Task{Step: "invoice", Attempt: 1, Timeout: graph.Duration(12 * time.Minute)}

	got := envMap(t, environment(task, deadlineOf(task, dispatched)))[EnvDeadline]
	if got != "2026-09-10T06:12:00Z" {
		t.Fatalf("%s is %q, and the watch stops this container at %s", EnvDeadline, got, dispatched.Add(12*time.Minute))
	}
}

// A brick's parameters reach it through /agk/params.json and not through the
// environment. The manifest may mark one sensitive, and the environment is the one place
// the secrets rule refuses to put a value, "readable by its children" and in "diagnostic
// dumps".
func TestEnvironmentDoesNotExportABricksParams(t *testing.T) {
	task := graph.Task{
		Step:    "invoice",
		Attempt: 1,
		Params:  map[string]any{"endpoint": "https://api.billing.example.com/v2"},
	}
	for name := range envMap(t, environment(task, task.Deadline)) {
		if strings.HasPrefix(name, EnvParamPrefix) {
			t.Fatalf("a brick step exported %s, and a brick reads its parameters from %s", name, ParamsPath)
		}
	}
}

// "params has no manifest to be validated against here, so each entry is written to
// /agk/params.json and exported as AGK_PARAM_<NAME>." Here is a script step, and the
// documentation's own example reads one straight into a URL.
func TestEnvironmentExportsAScriptsParams(t *testing.T) {
	task := graph.Task{
		Step:    "check-vat",
		Attempt: 1,
		Script:  []string{`curl -sSf "$AGK_PARAM_ENDPOINT/check"`},
		Params: map[string]any{
			"endpoint": "https://vat.example.com/v2",
			"retries":  float64(3),
			"budget":   float64(1000000),
			"strict":   true,
			"headers":  map[string]any{"accept": "application/json"},
			"max-age":  "refused as a variable name",
		},
	}

	env := envMap(t, environment(task, task.Deadline))
	for name, want := range map[string]string{
		"AGK_PARAM_ENDPOINT": "https://vat.example.com/v2",
		"AGK_PARAM_RETRIES":  "3",
		"AGK_PARAM_BUDGET":   "1000000",
		"AGK_PARAM_STRICT":   "true",
		"AGK_PARAM_HEADERS":  `{"accept":"application/json"}`,
	} {
		if env[name] != want {
			t.Errorf("%s is %q, want %q", name, env[name], want)
		}
	}
	// A parameter name the grammar refuses cannot be an environment variable name
	// either, and no shell could read it. The file still carries it.
	if _, ok := env["AGK_PARAM_MAX-AGE"]; ok {
		t.Errorf("a parameter no shell can name was exported anyway")
	}
}

// The same task twice is the same environment twice, which is what makes a container
// legible when it is read a second time.
func TestEnvironmentIsOrdered(t *testing.T) {
	task := graph.Task{
		Step:    "check-vat",
		Attempt: 1,
		Script:  []string{"true"},
		Params:  map[string]any{"zeta": "z", "alpha": "a", "mu": "m"},
	}
	first, second := environment(task, task.Deadline), environment(task, task.Deadline)
	if len(first) != len(second) {
		t.Fatalf("two runs of one task gave %d and %d variables", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("variable %d is %q and then %q", i, first[i], second[i])
		}
	}
	// The table first, in its own order, then the parameters sorted by name.
	var params []string
	for _, v := range first {
		if strings.HasPrefix(v, EnvParamPrefix) {
			params = append(params, v)
		}
	}
	want := []string{"AGK_PARAM_ALPHA=a", "AGK_PARAM_MU=m", "AGK_PARAM_ZETA=z"}
	for i := range want {
		if i >= len(params) || params[i] != want[i] {
			t.Fatalf("the exported parameters are %v, want %v", params, want)
		}
	}
}

// envMap reads an environment back the way a container would: NAME=value, with the
// value's own equals signs left alone.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, v := range env {
		name, value, ok := strings.Cut(v, "=")
		if !ok {
			t.Fatalf("%q is not a NAME=value", v)
		}
		if _, seen := out[name]; seen {
			t.Fatalf("%s is set twice", name)
		}
		out[name] = value
	}
	return out
}
