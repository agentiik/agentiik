package local

import (
	"time"

	"github.com/agentiik/agentiik/agk"
)

// Event is one thing that became true about one task, as a narrator needs it and no more.
//
// It is a value rather than a line of text because this package holds no terminal: what
// the screen says, how wide the step column is and which transitions are worth a line are
// cmd/agk's, and a package that printed here would be a package two callers could not
// share.
//
// Every Event is delivered from the one goroutine that calls Next and Record, so the
// narration arrives in the order the evaluator saw it and the narrator needs no lock. The
// driver's own observations are posted onto that goroutine's queue first, which is why a
// running transition never overtakes the dispatch that preceded it.
type Event struct {
	// At is how long into the run this was, so that a narration reads as a sequence
	// rather than as a clock. It is a duration and not a moment because the moment is
	// the run's and is already in the state beside it.
	At time.Duration

	Step  agk.Step  `json:"step"`
	Shard agk.Shard `json:"shard,omitzero"`

	// Shards is how many shards the step was divided into, which is what makes a
	// fan-out worth one line: a shard of one of three is news in a way a shard of one
	// of one is not.
	Shards  int `json:"shards,omitempty"`
	Attempt int `json:"attempt"`

	// State is the task transition, and Verdict is what the step came to once that
	// transition was recorded. A narrator that prints one line per step transition
	// reads the second; -v reads the first.
	//
	// An Event whose Attempt is zero is the step itself and not one of its shards, and its
	// State means nothing: an attempt is numbered from one everywhere in this module, so
	// zero cannot name a task. That is how the two kinds of line are told apart, and it is
	// what lets a step that skipped be narrated at all, having no task to report.
	State   agk.TaskState `json:"state"`
	Verdict agk.Verdict   `json:"verdict"`

	// Ports is the item count of each port the task or the step published, which is
	// what a person wants at the end of a line and the only thing about an envelope
	// worth carrying here: the envelopes themselves are in the state.
	Ports map[agk.Port]int `json:"ports,omitempty"`

	ExitCode int `json:"exit_code,omitempty"`

	// NextAttemptAt is when the retry backoff places the attempt after this one,
	// which is the one piece of news a failure that is not final carries.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

// counts is the item count of each port of an envelope map, which is what an Event
// carries instead of the envelopes.
func counts(ports map[agk.Port]agk.Envelope) map[agk.Port]int {
	if len(ports) == 0 {
		return nil
	}
	out := make(map[agk.Port]int, len(ports))
	for port, envelope := range ports {
		out[port] = len(envelope.Items)
	}
	return out
}
