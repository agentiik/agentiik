package driver

import (
	"context"

	"github.com/agentiik/agentiik/agk"
)

// Observer is told what graph.Result has nowhere to carry.
//
// The documented result message carries its ports by digest, a log reference, an artifact
// list and a usage block, and graph.Result carries none of the four: it holds the
// envelopes themselves and not what names them in the store. Adding them there would be
// changing the evaluator's contract to suit its executor, so they leave through an
// interface this package owns instead. It is also how a runner gets the dispatched, running and
// publishing transitions its heartbeat needs while a task is still in flight, which are
// states agk.TaskState already names and which no Result is ever returned for.
//
// Observe is called on the goroutine running the task and must not block: a runner that
// wanted to write one of these to a database posts it to its own queue here.
type Observer interface {
	Observe(ctx context.Context, e Event)
}

// Event is one thing that became true about one task.
//
// State is the transition. The three non-terminal ones are the point of the interface:
// dispatched when the container was created, running when it was started, publishing
// when it has exited and its outputs are being collected, which "is its own state
// because the work is done and the result is not yet safe".
type Event struct {
	Task  agk.TaskID    `json:"task"`
	State agk.TaskState `json:"state"`

	// Container is the daemon's identifier for it, which is what an operator needs
	// to reach a container this runner started.
	Container string `json:"container,omitempty"`

	// Log is where the task's log went, filled in on the terminal event.
	Log LogRef `json:"log,omitzero"`

	// Outputs names the envelope of every port a success published, by the digest the
	// store holds it under and its count, which is how the result message names them and
	// what a Result, carrying the envelopes themselves, cannot say. Of a success it is
	// never absent, the empty list included, and on every other event it is.
	Outputs []EndedPort `json:"outputs,omitzero"`

	// Artifacts are what this task put in the store, which the result message
	// carries and a Result does not.
	Artifacts []agk.File `json:"artifacts,omitempty"`

	Usage Usage `json:"usage,omitzero"`

	// Err is why, on a task that produced no container at all, and on the failed
	// ending of a container that exited 0 and whose outputs were refused or could not
	// be written, which is the error Run answers with. It is nil on every other event,
	// including a task whose container failed: a container that exited reported, and
	// the exit code is not an error.
	Err error `json:"-"`
}

// Usage is what the task cost. It is the result message's own block, and the three
// members are spelled on the wire exactly as that message spells them, cpu_seconds,
// max_rss_bytes and image_pull_ms, because this is the block a runner forwards rather
// than a block it composes.
//
// ImagePullMS is here rather than beside the other two because it is the one part that is
// not spent inside the container: a task that waited four minutes on a cold image did not
// spend four minutes of anybody's CPU, and reporting it inside the run time would make a
// slow registry look like a slow brick. The other two are sampled from the daemon's
// statistics while the container runs, which sampler says the limits of: each is what the
// samples saw, and a container that exited before any was read carries neither. They are
// filled in on the terminal event of a container this driver watched run, and are absent
// from one it found already exited.
type Usage struct {
	CPUSeconds  float64 `json:"cpu_seconds,omitempty"`
	MaxRSSBytes int64   `json:"max_rss_bytes,omitempty"`
	ImagePullMS int64   `json:"image_pull_ms,omitempty"`
}

// IsZero says there is nothing to report, which is what lets the field be omitted.
func (u Usage) IsZero() bool { return u == Usage{} }

// observe tells the observer, where there is one. A nil observer is the ordinary case
// for a library: agk brick test has nothing to tell.
//
// An ending is kept on the task in flight first, whoever is listening, because what the
// observer is told then is what the record of the ending keeps beside the Result.
func (d *Docker) observe(ctx context.Context, e Event) {
	if e.State.Terminal() {
		if h := d.lookup(e.Task); h != nil {
			h.end(e)
		}
	}
	if d.cfg.Observer == nil {
		return
	}
	d.cfg.Observer.Observe(ctx, e)
}
