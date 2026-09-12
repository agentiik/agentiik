package local

import (
	"context"

	"github.com/agentiik/agentiik/driver"
)

// observationQueue is how many of the driver's observations are held between two passes of
// the loop.
//
// It is generous rather than exact because the cost of one is a struct on a channel, and
// because the number of observations in flight is three per task: a fan-out of a hundred
// shards under max_parallel is still bounded by what the plan started.
const observationQueue = 256

// observer is driver.Observer onto the loop's queue.
//
// The driver calls Observe on the goroutine running the task and says it must not block, so
// this posts and returns. A send that would block is dropped, which is the one place in
// this package where something is thrown away: an observation is narration, the run is the
// Results and the state, and a narration that stalled the goroutine collecting a brick's
// outputs would be a terminal holding up a container.
//
// Nothing is read out of these but the transition. The terminal event carries the log
// reference, the artifacts and the usage block, and all three are a server's business: the
// log is already a file whose path the layout derives, and the artifacts are in the
// envelopes the Result carries.
type observer struct{ events chan<- driver.Event }

func (o observer) Observe(ctx context.Context, e driver.Event) {
	select {
	case o.events <- e:
	default:
	}
}
