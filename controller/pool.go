package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// The pool's policy, applied where a task leaves.
//
// "The pool holds accepted namespaces and ceilings {cpu, memory, pids}, and the controller applies
// both at dispatch. A step whose pool does not accept the run's namespace, or names no pool that
// exists, is not published: it fails on the infrastructure's account and names the pool.
// Resources are capped to the pool's ceilings on the task message." A step whose labels select no
// pool, or more than one, is refused the same way, since no runner could be handed it either.
//
// Here rather than on the runner, because a runner of the pool would only ever refuse the task
// once it held it, and every runner of the pool refuses it alike: the task would go round the pool
// until its deadline. The API checks the namespace again at the redemption, for a pool whose policy
// changed while the task waited and for a grant presented by a runner of a pool that does not run
// the namespace.

// platformFailure is the exit code a task refused at dispatch ends with: 125, the first code of the
// band "read as an infrastructure failure, charged to the runner and not to the brick", which no
// retry policy can name. The brick never ran, and running it again changes nothing until an
// administrator changes the pools or the step's author changes its runs_on.
const platformFailure = 125

// unpublishable is a task no runner may be handed, and why, in words that name the pool or the
// labels no one pool answers.
type unpublishable struct{ why string }

func (u unpublishable) Error() string { return u.why }

// poolOf is the pool a task's labels select among those given, or why no runner of any pool may be
// handed it.
//
// "A step goes to the pool whose labels include every label of its runs_on, among the namespace's
// allowed pools", and bus.Route is that rule: the pool found here is the pool the dispatch names and
// the bus publishes to, so the pool whose policy is applied and the pool whose runners are handed
// the task cannot be two different pools. The policy is the step's, since every shard of a step
// carries its labels and its run's namespace, so a refusal of one task is a refusal of each of them.
//
// A namespace reaches a pool only where both sides agree: the namespace allows the pool and the pool
// accepts the namespace. No namespace carries a list of allowed pools yet, so the pools chosen among
// are every pool that accepts it. A pool that does not is never a candidate, since a pool dedicated
// to one namespace, carrying the labels a shared pool carries, would otherwise make every other
// namespace's steps on those labels ambiguous. Only where no pool that accepts the namespace carries
// the labels and one that refuses it does is the step told which pool refused it, as it was before
// labels chose the pool.
func poolOf(namespace string, t graph.Task, pools []db.RunnerPool) (db.RunnerPool, error) {
	var reachable, refusing []bus.Pool
	for _, p := range pools {
		if p.Accepts(namespace) {
			reachable = append(reachable, bus.Pool{Name: p.Name, Labels: p.Labels})
		} else {
			refusing = append(refusing, bus.Pool{Name: p.Name, Labels: p.Labels})
		}
	}
	name, err := bus.Route(t.RunsOn, reachable)
	var unrouted *bus.Unrouted
	if errors.As(err, &unrouted) && len(unrouted.Pools) == 0 {
		if other, err := bus.Route(t.RunsOn, refusing); err == nil {
			return db.RunnerPool{}, unpublishable{fmt.Sprintf("step %s runs on the runner pool %s, which does not accept the namespace %s, so no runner may be handed it: the step fails on the infrastructure's account until an administrator lets the pool accept it", t.Step, other, namespace)}
		}
	}
	switch {
	case errors.As(err, &unrouted) && len(unrouted.RunsOn) == 0:
		return db.RunnerPool{}, unpublishable{fmt.Sprintf("step %s names no runner label and runs on the runner pool %s, which does not exist, so no runner may be handed it: the step fails on the infrastructure's account until an administrator creates the pool", t.Step, bus.DefaultPool)}
	case errors.As(err, &unrouted) && len(unrouted.Pools) == 0:
		return db.RunnerPool{}, unpublishable{fmt.Sprintf("step %s runs on [%s], and no runner pool the namespace %s may use carries every one of those labels, so no runner may be handed it: the step fails on the infrastructure's account until an administrator creates a pool that does", t.Step, strings.Join(t.RunsOn, ", "), namespace)}
	case errors.As(err, &unrouted):
		return db.RunnerPool{}, unpublishable{fmt.Sprintf("step %s runs on [%s], and the runner pools %s each carry every one of those labels, so no runner may be handed it: a step goes to one pool, and it fails on the infrastructure's account until its runs_on names a label only one of them carries", t.Step, strings.Join(t.RunsOn, ", "), strings.Join(unrouted.Pools, " and "))}
	case err != nil:
		return db.RunnerPool{}, unpublishable{fmt.Sprintf("step %s names no runner pool that can exist, so no runner may be handed it: %s", t.Step, err)}
	}
	return pools[slices.IndexFunc(pools, func(p db.RunnerPool) bool { return p.Name == name })], nil
}

// refuseUnpooled ends every step whose pool will not run the namespace, each of its pending shards
// failed with platformFailure and the reason, and asks the evaluator again, since what follows a
// failed step may be a step that runs when: [failed], until the plan holds nothing it refuses.
//
// The whole step at once and not the tasks the plan holds, because max_parallel and the quota cut a
// plan down to a slice of a fan-out: refused a slice at a time, a step of ten thousand shards would
// take a decision per slice, each writing every task of the run again. Each round ends at least one
// step for good, since 125 is retried by no policy and requeued only after a loss, so there are at
// most as many rounds as steps.
//
// The plan answered carries the stops of every round. The evaluator names a stop sent while the
// run goes on only in the pass that ends its task, so a round that dropped them would leave a
// fail_fast sibling or a superseded task running with nobody told.
func refuseUnpooled(ev *graph.Evaluator, namespace string, pools []db.RunnerPool, plan graph.Plan, now time.Time) (graph.Plan, error) {
	var stops []graph.Stop
	for {
		for _, s := range plan.Stop {
			if !slices.Contains(stops, s) {
				stops = append(stops, s)
			}
		}
		refused := map[agk.Step]string{}
		for _, t := range plan.Start {
			if _, err := poolOf(namespace, t, pools); err != nil {
				refused[t.Step] = err.Error()
			}
		}
		if len(refused) == 0 {
			plan.Stop = stops
			return plan, nil
		}
		state := ev.State()
		var ending []graph.Result
		for step, why := range refused {
			for _, sh := range state.Steps[step].Shards {
				if sh.Task != agk.TaskPending {
					continue
				}
				ending = append(ending, graph.Result{
					Task: agk.NewTaskID(state.Run.ID, step, sh.Attempt, sh.Shard), State: agk.TaskFailed,
					ExitCode: platformFailure, Requeue: sh.Requeue, FinishedAt: now, Reason: why,
				})
			}
		}
		for _, r := range ending {
			if err := ev.Record(r, now); err != nil {
				return graph.Plan{}, fmt.Errorf("the refusal of %s could not be recorded: %w", r.Task, err)
			}
		}
		var err error
		if plan, err = ev.Next(now); err != nil {
			return graph.Plan{}, err
		}
	}
}

// policed reads the pools in the transaction that issues a task's grant, and answers the pool the
// task's labels select there and what the task may be given on it: the step's resources capped to
// the pool's ceilings. A pool that will not run it, which the pass found running it when it read
// the pools, is refused here too, and the task stays pending for the next pass to end. Every pool
// and not the one the pass found, since the rule chooses among all of them: a pool created since
// can make one that matched alone one of two.
func policed(ctx context.Context, w *db.Wide, namespace string, t graph.Task) (string, graph.Resources, error) {
	pools, err := w.RunnerPools(ctx)
	if err != nil {
		return "", graph.Resources{}, err
	}
	pool, err := poolOf(namespace, t, pools)
	if err != nil {
		return "", graph.Resources{}, err
	}
	return pool.Name, capped(t.Resources, pool.Ceilings), nil
}

// capped holds what a step asked for to a pool's ceilings, each of its own kind.
//
// A ceiling left empty is no ceiling, and an ask left empty takes the ceiling, which is the rule
// the runner applies to its own caps: a ceiling that only applied to steps that named a number
// would not be a ceiling, since a step naming no memory is a container with none. An ask that is
// not a number of its kind is left as it is, for the runner to refuse on the brick's account as it
// refuses one wherever no ceiling applies: capping it would turn a step written wrong into one that
// runs.
func capped(asked graph.Resources, ceiling db.Ceilings) graph.Resources {
	out := asked
	if ceiling.CPU != "" {
		if asked.CPU == "" || above(cores(asked.CPU), cores(ceiling.CPU)) {
			out.CPU = ceiling.CPU
		}
	}
	if ceiling.Memory != "" {
		if asked.Memory == "" || above(bytesOf(asked.Memory), bytesOf(ceiling.Memory)) {
			out.Memory = ceiling.Memory
		}
	}
	if ceiling.PIDs > 0 && (asked.PIDs <= 0 || asked.PIDs > ceiling.PIDs) {
		out.PIDs = ceiling.PIDs
	}
	return out
}

// above says whether an ask is over a ceiling, and says no where either could not be read.
func above(asked, ceiling *big.Rat) bool {
	return asked != nil && ceiling != nil && asked.Cmp(ceiling) > 0
}

// cores reads a number of cores exactly, "0.5" or "4", and answers nil for anything else. Exactly,
// because 0.1 is no binary fraction, and an ask compared through a float could come out a hair
// above a ceiling it equals.
func cores(v string) *big.Rat {
	n, ok := new(big.Rat).SetString(v)
	if !ok || n.Sign() <= 0 || strings.ContainsAny(v, "/eE") {
		return nil
	}
	return n
}

// bytesOf reads a whole number with a binary suffix, "512Mi", as a number of bytes, and answers nil
// for anything else. As a rational rather than an int64, so that no size a step could write
// overflows on the way to being compared.
func bytesOf(v string) *big.Rat {
	for _, s := range []struct {
		suffix string
		shift  uint
	}{{"Ki", 10}, {"Mi", 20}, {"Gi", 30}, {"Ti", 40}} {
		digits, ok := strings.CutSuffix(v, s.suffix)
		if !ok {
			continue
		}
		n, ok := new(big.Int).SetString(digits, 10)
		if !ok || n.Sign() <= 0 || strings.HasPrefix(digits, "+") {
			return nil
		}
		return new(big.Rat).SetInt(n.Lsh(n, s.shift))
	}
	return nil
}
