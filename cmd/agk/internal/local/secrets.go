package local

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// secrets answers the driver off what the command line supplied, which is the laptop's
// answer to the question a server runner answers by redeeming a per-task grant at the API.
//
// Nothing about the mounting changes. The driver writes each value to a file, binds it
// read-only at /agk/secrets/<name>, masks it out of the log and out of the payload by
// literal match before anything is written, and nothing reaches an environment variable.
// That is the whole point of answering this interface rather than taking a shortcut: a
// local run mounts a secret exactly as a server run does, so a workflow that works here
// works there.
type secrets struct{ s *Session }

// Value is the value of one secret, at the last moment, which is when the driver asks.
//
// A name that was never supplied is refused rather than answered with nothing. An empty
// value is a value, and a brick that read an empty file where it expected a token would
// fail for a reason nobody could see from the outside. The run has already been held to the
// names every step mounts before any container existed, so reaching this refusal means the
// workflow asked for a secret the check could not see, and naming the flag is the only
// useful thing to say.
func (x secrets) Value(ctx context.Context, name string) ([]byte, error) {
	f := x.s.current()
	if f == nil {
		return nil, fmt.Errorf("local: the secret %s was asked for and no run is in flight", name)
	}
	value, ok := f.secrets[name]
	if !ok {
		return nil, fmt.Errorf("local: the secret %s was not supplied: a local run takes its values from --secret %s=<value> or --secret-file %s=<path>", name, name, name)
	}
	return value, nil
}

// Missing names the secrets a workflow's steps mount and the command line did not supply.
//
// It is here rather than in cmd/agk because the rule it applies is about what the driver
// will ask for, and this package is what answers the driver. It is called before a daemon
// is touched: a run that would die at the fourth shard of a fan-out for a value that was
// never coming should die at second zero instead, and a refusal that names every missing
// name at once is one trip to the terminal rather than four.
//
// The names are the step's own secrets keyword, which is what a task's secret mounts are
// built from and the only thing the driver ever asks for a value of. A secret a step reads
// in params is not here on purpose: it travels as an opaque reference and "the task message
// names the secrets it needs and carries none of them", so nothing on this side has to have
// a value for it.
func Missing(wf *graph.Workflow, supplied map[string][]byte) map[string][]agk.Step {
	if wf == nil {
		return nil
	}
	out := map[string][]agk.Step{}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		for _, secret := range wf.Steps[name].Secrets {
			if _, ok := supplied[secret]; ok {
				continue
			}
			if !slices.Contains(out[secret], name) {
				out[secret] = append(out[secret], name)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
