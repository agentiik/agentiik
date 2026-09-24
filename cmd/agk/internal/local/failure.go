package local

import (
	"maps"
	"os"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// Failure is one task that ended badly, in the order a report names it: the step, the exit
// code, and what was refused.
//
// That order is #voice and it is the field order here, so a report is formatted out of the
// struct rather than out of a string somebody assembled. What this type does not hold is
// the words: the log is a path and not its contents, and the sentence is the driver's own
// rather than a paraphrase, because what to print and how much of it is cmd/agk's.
type Failure struct {
	Step    agk.Step
	Shard   agk.Shard
	Attempt int

	// State is what became of the task, spelled as the task states are spelled:
	// failed, timed_out, lost. It is here because a report that could not name the
	// state would have to invent prose for a task that timed out, and an identifier is
	// never prettified.
	State agk.TaskState

	// HasExit says whether a container reported a code at all. The driver invents none
	// for a failure that produced no container, and a command line that invented one
	// for it would report somebody else's failure, so the report reads "no exit code"
	// where this is false.
	HasExit  bool
	ExitCode int

	// Band is the row of the exit-code table the code landed in, which is where the
	// verdict came from. It is read for a task that failed and for no other state,
	// which is the rule graph.Result already states about an exit code.
	Band agk.ExitBand

	// Charge says whose failure this was, where the driver decided: the brick or the
	// runtime. It is the driver's own word and not a judgement taken here.
	Charge driver.Charge

	// Refused is the driver's own sentence about a task that produced no Result,
	// naming the rule in the documentation's words. It is empty for a container that
	// ran and exited, because an exit code is not a refusal.
	Refused string

	// Log is the absolute path of what the container wrote, or empty where nothing
	// opened one. The last lines of it belong in the report and reading them is
	// cmd/agk's, because how many lines fit on a screen is not this package's business.
	Log string
}

// failures reads the failures of one run out of the state it ended in.
//
// Out of the state and not out of a list the loop kept, so that the report of a run and the
// state beside it cannot disagree: what a person reads and what a second process would
// resume from are the same value. The only thing the state cannot hold is the driver's own
// sentence about a task that produced no Result, because a graph.Result has nowhere to
// carry one, and that arrives beside it.
//
// A shard waiting for another attempt is not a failure. It failed and it is going again,
// which the narration already said; what is reported here is what the run ended with.
//
// A failure a step tolerated under continue_on_error is reported like any other. It is
// still a failure and the step still says so; what it did not do is fail the run, and the
// run state is where that is read.
func failures(state *graph.State, g *graph.Graph, refused map[agk.TaskID]trouble, l Layout) []Failure {
	if state == nil {
		return nil
	}
	var out []Failure
	for _, name := range order(state, g) {
		ss, known := state.Steps[name]
		if !known {
			continue
		}
		for _, sh := range ss.Shards {
			if !failed(sh.Task) {
				continue
			}
			task := agk.NewTaskID(state.Run.ID, name, sh.Attempt, sh.Shard)
			f := Failure{
				Step:    name,
				Shard:   sh.Shard,
				Attempt: sh.Attempt,
				State:   sh.Task,
			}
			if t, ok := refused[task]; ok {
				// The driver reported its own trouble rather than a container's
				// exit, so there is no code to name. The code the evaluator was
				// given is still the code the band was read from, and the band
				// is what a person can act on.
				f.Refused, f.Charge = t.refused, t.charge
				f.Band = agk.Band(sh.ExitCode)
				if t.exited {
					f.HasExit, f.ExitCode = true, sh.ExitCode
				}
			} else if sh.Task == agk.TaskFailed {
				f.HasExit, f.ExitCode = true, sh.ExitCode
				f.Band = agk.Band(sh.ExitCode)
			}
			if path, err := l.Log(task); err == nil {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					f.Log = path
				}
			}
			out = append(out, f)
		}
	}
	return out
}

// failed says whether a task state is one a report names. Cancelled is not one of them: a
// cancelled task is the consequence of a stop and never its cause, and a run cancelled by
// an interrupt would otherwise report every container it called off as a failure.
func failed(s agk.TaskState) bool {
	switch s {
	case agk.TaskFailed, agk.TaskTimedOut, agk.TaskLost:
		return true
	default:
		return false
	}
}

// order is the graph's own order where there is a graph, and the state's own keys where
// there is not, so that a report made from a state alone still has one.
func order(state *graph.State, g *graph.Graph) []agk.Step {
	if g != nil {
		return g.Order()
	}
	// Sorted, because a map is not an order and two reports of one state have to read
	// the same way.
	return slices.Sorted(maps.Keys(state.Steps))
}
