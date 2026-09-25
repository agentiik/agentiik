package controller

import (
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// What the controller tells whoever counts.
//
// "The controller knows dispatch, retries, losses and latency": each of them is a decision this
// package takes, and each is told here once, when the transaction that writes it has committed and
// never before. A pass the fence or a newer decision refused tells nothing, and the pass that
// decides the same thing again tells it then; a result delivered twice is news once, and the
// evaluator already says which delivery that was by moving its sequence. So what is counted is
// what the database holds, whatever the bus redelivered and however often a pass was refused.
//
// Counted in the process that decides, and so in the one that leads: a standby decides nothing and
// counts nothing, and a new leader counts from zero, which a scraper reads as the reset of a
// counter, as it reads a restart. What exists at a moment rather than what happened, a queue's
// depth or a runner's tasks, is not told here: the program reads it when it is scraped.

// Observer is told what the controller did, once it is written down. Nil counts nothing.
type Observer interface {
	// Dispatched is one dispatch handed to the queue of pool and recorded as handed out: a first
	// dispatch, a further attempt and a requeue alike, each of which is one message a runner
	// of the pool may take.
	Dispatched(pool string)

	// Ended is an ending a runner reported of a container that ran, taken as news: how long it
	// ran, by the runner's clock, from the start to the end it reported. brick and version are
	// the manifest's, and empty for a script step, which runs no brick.
	Ended(brick, version string, state agk.TaskState, ran time.Duration)

	// Retried is a further attempt a step's retry policy granted a shard after a failure: the
	// attempt is a new key, and the one that failed is over.
	Retried(brick, version string)

	// Lost is a dispatch declared lost, heard by the evaluator, charged to the pool of the
	// runner that held it. Whether it was then requeued is the requeue's own dispatch.
	Lost(pool string)

	// RunEnded is a run reaching its verdict, took from its creation to its end, a wait for its
	// concurrency group and for quota included.
	RunEnded(namespace, workflow string, state agk.RunState, took time.Duration)
}

// told is what a pass tells once it has committed, in the order it happened.
type told []func(Observer)

// tell says it, where there is anybody to say it to.
func (co *Core) tell(t told) {
	if co.observer == nil {
		return
	}
	for _, f := range t {
		f(co.observer)
	}
}

// brickOf is the brick a step runs and its version, empty for a step that runs none.
func brickOf(g *graph.Graph, step agk.Step) (string, string) {
	name, version, _ := g.Brick(step)
	return name, version
}

// shardKey names a shard within a run.
type shardKey struct {
	step  agk.Step
	shard agk.Shard
}

// attemptsOf is the attempt every shard of a state is on.
func attemptsOf(s *graph.State) map[shardKey]int {
	out := map[shardKey]int{}
	for name, st := range s.Steps {
		for _, sh := range st.Shards {
			out[shardKey{name, sh.Shard}] = sh.Attempt
		}
	}
	return out
}

// retried tells a retry for every attempt a shard moved on by between before and s. A shard the
// state holds and before did not has just been split out of its step, and starts at its first
// attempt: nothing was retried.
func retried(t told, g *graph.Graph, before map[shardKey]int, s *graph.State) told {
	for k, now := range attemptsOf(s) {
		was, ok := before[k]
		if !ok {
			continue
		}
		brick, version := brickOf(g, k.step)
		for range now - was {
			t = append(t, func(o Observer) { o.Retried(brick, version) })
		}
	}
	return t
}

// runEnded tells a run's verdict where the state has one and the run read had none.
func runEnded(t told, namespace, workflow string, was agk.RunState, s *graph.State, created time.Time) told {
	if was.Terminal() || !s.Run.State.Terminal() {
		return t
	}
	// Two clocks, the database's for the creation and this process's for the end, as the
	// runs table keeps them; a controller behind the database by more than the run took would
	// make it negative, and it is counted as nothing rather than as time running backwards.
	state, took := s.Run.State, max(s.Run.FinishedAt.Sub(created), 0)
	return append(t, func(o Observer) { o.RunEnded(namespace, workflow, state, took) })
}
