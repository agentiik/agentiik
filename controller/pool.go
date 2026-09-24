package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// The pool's policy, applied where a task leaves.
//
// "The pool holds accepted namespaces and ceilings {cpu, memory, pids}, and the controller applies
// both at dispatch. A step whose pool does not accept the run's namespace, or names no pool that
// exists, is not published: it fails on the infrastructure's account and names the pool.
// Resources are capped to the pool's ceilings on the task message."
//
// Here rather than on the runner, because a runner of the pool would only ever refuse the task
// once it held it, and every runner of the pool refuses it alike: the task would go round the pool
// until its deadline. The API checks the namespace again at the redemption, for a pool whose policy
// changed while the task waited and for a grant presented by somebody the task was never offered to.

// platformFailure is the exit code a task refused at dispatch ends with: 125, the first code of the
// band "read as an infrastructure failure, charged to the runner and not to the brick", which no
// retry policy can name. The brick never ran, and running it again changes nothing until an
// administrator changes the pool.
const platformFailure = 125

// unpublishable is a task no runner may be handed, and why, in words that name the pool.
type unpublishable struct{ why string }

func (u unpublishable) Error() string { return u.why }

// policed reads the pool a task's labels select and holds the task to it: refused where the pool
// does not exist or does not accept the namespace, and capped to its ceilings otherwise.
//
// The pool is the one package bus routes the message to, read off the same labels by the same
// function, so that the pool whose policy is applied and the pool whose runners are handed the task
// cannot be two different pools.
func policed(ctx context.Context, w *db.Wide, namespace string, t graph.Task) (graph.Resources, error) {
	name, err := bus.PoolOf(t.RunsOn)
	if err != nil {
		return graph.Resources{}, unpublishable{fmt.Sprintf("step %s names no runner pool that can exist, so no runner may be handed it: %s", t.Step, err)}
	}
	pool, err := w.RunnerPoolNamed(ctx, name)
	if errors.Is(err, db.ErrNoRunnerPool) {
		return graph.Resources{}, unpublishable{fmt.Sprintf("step %s runs on the runner pool %s, which does not exist, so no runner may be handed it: the step fails on the infrastructure's account until an administrator creates the pool", t.Step, name)}
	}
	if err != nil {
		return graph.Resources{}, err
	}
	if !pool.Accepts(namespace) {
		return graph.Resources{}, unpublishable{fmt.Sprintf("step %s runs on the runner pool %s, which does not accept the namespace %s, so no runner may be handed it: the step fails on the infrastructure's account until an administrator lets the pool accept it", t.Step, name, namespace)}
	}
	return capped(t.Resources, pool.Ceilings), nil
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
