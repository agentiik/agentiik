// Package stoptest holds the histories in which a rule of the language stops a task while its
// run goes on, and the shard states each one ends in. agk run --local and the controller are both
// held to them, each through its own loop, so that one history gives one answer whichever of the
// two ran it: the rule is the evaluator's, and a loop that decided it again would drift.
//
// Test support: nothing at runtime reaches for it.
package stoptest

import "github.com/agentiik/agentiik/agk"

// Task is one task of a history, by step and shard index, 0 for a step with no fan-out.
type Task struct {
	Step  agk.Step
	Shard int
}

// Ending is what one task ends as: its state and the exit code its container exited with.
type Ending struct {
	State    agk.TaskState
	ExitCode int

	// NeverStarted says no container ran it: nobody handed it out before its stop, so it
	// has no exit code and no start, whatever ExitCode says.
	NeverStarted bool
}

// History is one run: the workflow, what its tasks do, and what it ends as.
type History struct {
	Name string

	// Workflow is the file, in script steps so that neither loop needs a manifest. It is
	// monthly-invoicing in finance, which is what the controller's tests hold a version of.
	Workflow string
	Inputs   map[string]any

	// Exits is the code each task's container exits with, and a task it does not name exits 0.
	// A task exits as soon as it is started, except Late, and one whose Ending is NeverStarted
	// is never started.
	Exits map[Task]int

	// Late exits in the moment before the stop reaches it: its report comes after the stop
	// went out, carrying exit 0 and what it published.
	Late Task

	// Want is what every task ends as, Steps what every step ends as, and Run the run.
	Want  map[Task]Ending
	Steps map[agk.Step]agk.Verdict
	Run   agk.RunState
}

// Histories are the two rules that stop a task while the run goes on: fail_fast and merge: first.
var Histories = []History{
	{
		Name: "fail_fast",
		Workflow: `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
steps:
  invoice:
    image: alpine:3.21
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy: { fan_out: item, fail_fast: true }
    script: [ "true" ]
    outputs: [ok]
  archive:
    image: alpine:3.21
    inputs:
      orders: ${{ workflow.inputs.orders }}
    script: [ "true" ]
    outputs: [ok]
`,
		Inputs: orders,
		Exits:  map[Task]int{{"invoice", 1}: 7},
		Late:   Task{"invoice", 2},
		Want: map[Task]Ending{
			{"invoice", 1}: {State: agk.TaskFailed, ExitCode: 7},
			{"invoice", 2}: {State: agk.TaskCancelled},
			{"archive", 0}: {State: agk.TaskSucceeded},
		},
		Steps: map[agk.Step]agk.Verdict{"invoice": agk.VerdictFailed, "archive": agk.VerdictSucceeded},
		Run:   agk.Failed,
	},
	{
		Name: "merge first",
		Workflow: `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
steps:
  normalize:
    image: alpine:3.21
    inputs:
      orders: ${{ workflow.inputs.orders }}
    script: [ "true" ]
    outputs: [ok]
  archive:
    image: alpine:3.21
    inputs:
      orders: ${{ workflow.inputs.orders }}
    strategy: { fan_out: item, max_parallel: 1 }
    script: [ "true" ]
    outputs: [ok]
  pick:
    image: alpine:3.21
    merge: first
    needs:
      - { step: normalize, port: ok, as: orders }
      - { step: archive, port: ok, as: orders }
    script: [ "true" ]
    outputs: [ok]
`,
		Inputs: orders,
		// archive goes one order at a time, so the barrier lifts with its first shard
		// running and its second not handed out yet, which never starts.
		Late: Task{"archive", 1},
		Want: map[Task]Ending{
			{"normalize", 0}: {State: agk.TaskSucceeded},
			{"archive", 1}:   {State: agk.TaskCancelled},
			{"archive", 2}:   {State: agk.TaskCancelled, NeverStarted: true},
			{"pick", 0}:      {State: agk.TaskSucceeded},
		},
		Steps: map[agk.Step]agk.Verdict{"normalize": agk.VerdictSucceeded, "archive": agk.VerdictCancelled, "pick": agk.VerdictSucceeded},
		Run:   agk.Succeeded,
	},
}

// orders is two, so that a fan-out over them has a shard to fail and one beside it, and one
// handed out one at a time has a shard running and one not started yet.
var orders = map[string]any{"orders": []any{
	map[string]any{"customer_id": "C-1042"},
	map[string]any{"customer_id": "C-1043"},
}}
