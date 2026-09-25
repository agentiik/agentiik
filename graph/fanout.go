package graph

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// The four fan-out strategies, which are what a step is divided into shards by. Shards
// of one step have their envelopes concatenated port by port before publication, so a
// fan-out is invisible to whatever reads the result and is a decision about how the work
// is spread and never about what the step means.
//
//	none        one container receives the whole envelope. The default.
//	item        one container per item, each receiving a single-item envelope.
//	batch(n)    one container per batch of n items.
//	matrix      one container per combination of the matrix variables, with the
//	            combination injected into params.
//
// A strategy carrying a matrix is a matrix fan-out whether or not it says so, because
// that is how the language is written: .region-matrix declares matrix and max_parallel
// and no fan_out, and a step extending it reads ${{ matrix.region }}.
//
// Three readings the documentation does not state are taken here.
//
// A step declaring no input port runs as one container under item and under batch(n)
// alike. There is nothing to shard on, and reading the absence of items as an empty
// fan-out would mean a step with no inputs and a batch size never ran at all.
//
// A step whose input ports carry a different number of items each is sharded on the
// longest, and a port with fewer items gives the shards past its end a batch of nothing.
// A port publishing fewer items is the batch it is, and nothing in the documentation
// makes a step fail for it.
//
// The current item, under fan_out: item, is the item of the first input port in name
// order that carries one. A step reading ${{ item }} has one item in mind, which is what
// a single input port gives it; naming the order settles what happens when there are two
// rather than leaving it to a map iteration.

// shard is one unit of work a fan-out produced: what it receives, where it sits in the
// series, and the combination or the item it was cut on.
type shard struct {
	Shard  agk.Shard
	Inputs map[agk.Port]agk.Envelope
	Item   *agk.Item
	Matrix map[string]any
}

// fanOut divides a step into the shards it runs as, given what arrived on its ports.
//
// Splitting is agk.Split, which is where an envelope is cut into shards and where the
// identity of an item is held through the cut, so that a shard, a replay and a run
// inspector speak about the same element by the same name.
func fanOut(name agk.Step, st *Step, in map[agk.Port]agk.Envelope) ([]shard, error) {
	switch fanOutOf(st) {
	case FanOutMatrix:
		return matrixShards(name, st, in)
	case FanOutItem:
		return splitShards(name, st, in, 1)
	case FanOutBatch:
		size := st.Strategy.Batch
		if size < 1 {
			return nil, fmt.Errorf("graph: step %s: fan_out: batch(n) runs one container per batch of n items, and the size starts at 1, not %d", name, size)
		}
		return splitShards(name, st, in, size)
	default:
		return []shard{{Inputs: in}}, nil
	}
}

// splitShards cuts every input port into pieces of at most size items and gives one
// piece of each port to each shard.
func splitShards(name agk.Step, st *Step, in map[agk.Port]agk.Envelope, size int) ([]shard, error) {
	if len(in) == 0 {
		// Nothing to shard on. The step runs once, and carries no shard index,
		// because there is no series for it to sit in.
		return []shard{{Inputs: in}}, nil
	}

	ports := slices.Sorted(maps.Keys(in))
	pieces := make(map[agk.Port][]agk.Envelope, len(in))
	count := 0
	for _, port := range ports {
		cut, err := agk.Split(in[port], size)
		if err != nil {
			return nil, fmt.Errorf("graph: step %s: port %s: %w", name, port, err)
		}
		pieces[port] = cut
		count = max(count, len(cut))
	}
	if count == 0 {
		// A fan-out over an empty batch starts no containers. The step publishes an
		// empty envelope on each of its ports and the graph moves on.
		return nil, nil
	}

	shards := make([]shard, 0, count)
	for i := range count {
		s := shard{
			Shard:  agk.Shard{Index: i + 1, Of: count},
			Inputs: make(map[agk.Port]agk.Envelope, len(ports)),
		}
		for _, port := range ports {
			if i < len(pieces[port]) {
				s.Inputs[port] = pieces[port][i]
			} else {
				s.Inputs[port] = emptyLike(in[port])
			}
		}
		if fanOutOf(st) == FanOutItem {
			for _, port := range ports {
				if items := s.Inputs[port].Items; len(items) > 0 {
					it := items[0]
					s.Item = &it
					break
				}
			}
		}
		shards = append(shards, s)
	}
	return shards, nil
}

// matrixShards runs the cartesian product of the variable lists, one shard per
// combination, each receiving the whole of every input port.
//
// The combinations are enumerated in variable name order with the last variable moving
// fastest, so that the same matrix always produces the same shard at the same index and
// a replay lands where the run did.
func matrixShards(name agk.Step, st *Step, in map[agk.Port]agk.Envelope) ([]shard, error) {
	names := slices.Sorted(maps.Keys(st.Strategy.Matrix))
	count := 1
	for _, key := range names {
		values := st.Strategy.Matrix[key]
		if len(values) == 0 {
			return nil, fmt.Errorf("graph: step %s: strategy: matrix: %s: every combination is a shard, and a variable with no value is a product of nothing", name, key)
		}
		count *= len(values)
	}

	shards := make([]shard, 0, count)
	for i := range count {
		combination := make(map[string]any, len(names))
		rest := i
		for k := len(names) - 1; k >= 0; k-- {
			values := st.Strategy.Matrix[names[k]]
			combination[names[k]] = values[rest%len(values)]
			rest /= len(values)
		}
		shards = append(shards, shard{
			Shard:  agk.Shard{Index: i + 1, Of: count},
			Inputs: in,
			Matrix: combination,
		})
	}
	return shards, nil
}

// emptyLike is the batch of nothing a port gives a shard past the end of what it
// published. It is stamped from the envelope that did travel, so the shard says where
// its empty port came from and no clock is read to say it.
func emptyLike(e agk.Envelope) agk.Envelope {
	m := e.Meta
	m.Count = 0
	return agk.Envelope{Meta: m, Items: []agk.Item{}}
}

// fanOutOf reads how the step is divided. A matrix is a matrix fan-out even where the
// file does not spell fan_out out, which is how the documentation's own hidden block
// writes it.
func fanOutOf(st *Step) FanOut {
	if len(st.Strategy.Matrix) > 0 {
		return FanOutMatrix
	}
	return st.Strategy.FanOut
}

// slotsFree says how many more shards of this step may be started now.
//
// max_parallel is a ceiling on how many shards of this step run at the same time, and it
// is read here per step and across attempts: a retry occupies a runner exactly as a
// first attempt does, and the keyword exists to bound what one step takes of the fleet.
// A step that states no ceiling may start every shard it has.
func slotsFree(st *Step, ss StepState) int {
	ceiling := st.Strategy.MaxParallel
	if ceiling < 1 {
		return math.MaxInt
	}
	return max(ceiling-inFlight(ss), 0)
}

// inFlight counts the shards of a step that hold a runner at this moment: dispatched,
// running or publishing. A shard that has not been handed out yet and a shard that has
// reached a terminal state hold nothing.
func inFlight(ss StepState) int {
	n := 0
	for _, sh := range ss.Shards {
		if holdsARunner(sh) {
			n++
		}
	}
	return n
}

// holdsARunner says whether one shard is occupying a runner at this moment. It is the
// reading max_parallel counts on, the reading a merge: first stops on and the reading a
// cancelled run is called off by, written once so that the three cannot drift: a shard
// that has not been handed out yet holds nothing, and a shard that has reached a terminal
// state has let go.
func holdsARunner(sh ShardState) bool {
	return sh.Task != agk.TaskPending && !sh.Task.Terminal()
}

// failFast says whether the first shard to fail stops the shards still running, rather
// than letting every shard finish before the step is judged.
func failFast(st *Step) bool { return st.Strategy.FailFast }

// siblingsLeft names, by their place in the step, the shards that are not over beside one
// that has failed for good, which is what fail_fast stops: the ones in flight and the ones
// nobody has handed out yet. The shard that failed is not among them: it has already ended.
func siblingsLeft(ss StepState, failed agk.Shard) []int {
	var out []int
	for i, sh := range ss.Shards {
		if sh.Shard == failed || sh.Task.Terminal() {
			continue
		}
		out = append(out, i)
	}
	return out
}
