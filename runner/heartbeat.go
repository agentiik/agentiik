package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/graph"
)

// The heartbeat.
//
// "A runner posts one heartbeat every 10 seconds to the API, listing the idempotency keys it
// currently holds. One request covers every in-flight task on that host." The controller counts a
// task bound to this runner as held only while a heartbeat goes on naming its key, and three
// intervals without one move it to lost, so every key the host answers for is named: from the
// moment the loop writes it down, through a redemption asked again, the run and the result's
// publication, and a kept result until the bus has taken it. The answer is where every order
// reaches a runner, since nothing ever connects to one: a key to stop, a drain, and the
// installation's clock.

// HeartbeatInterval is how often a runner says it is there, "every 10 seconds".
//
// A constant and not something an answer tells the runner, since the wire's answer carries no
// interval: the controller's sweep counts silence in the same figure, db.HeartbeatInterval, and a
// test holds the two together.
const HeartbeatInterval = 10 * time.Second

// heartbeatPath is the route, as the router spells it.
const heartbeatPath = "/api/v1/runners/heartbeat"

// clockSkew is how far apart sent_at and received_at may be before the agent says so. The two
// differ by the request's own transit as well, which is milliseconds, so a second is drift and not
// a slow network.
const clockSkew = time.Second

// beatMaxTasks is how many keys one heartbeat names, "at most 4,096" as the page says and as the
// API takes in one: a heartbeat naming more is refused whole. A test holds it at no fewer than
// MaxConcurrency, so that a host holding as many tasks as it may never has one left out.
const beatMaxTasks = 4096

// keyForm is the wire's idempotencyKey, copied from wire.schema.json and held to it by a test. The
// API refuses a whole heartbeat naming one key off it, so a key off it is left out rather than
// sent: one key the runner took and could not name is one task the sweep may declare lost, and a
// heartbeat refused is every task on the host.
var keyForm = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]+/[A-Za-z0-9][A-Za-z0-9_-]*/[1-9][0-9]*(?:/[1-9][0-9]*/[1-9][0-9]*)?$`)

// beatRequest is runnerHeartbeat.request.
type beatRequest struct {
	Runner       string   `json:"runner"`
	AgentVersion string   `json:"agent_version"`
	State        string   `json:"state"`
	Concurrency  int      `json:"concurrency"`
	Tasks        []string `json:"tasks"`
	SentAt       string   `json:"sent_at"`
}

// beatAnswer is runnerHeartbeat.response. Cancel is a pointer so that an answer without it, which
// the wire refuses, is told from an empty list.
type beatAnswer struct {
	ReceivedAt           string    `json:"received_at"`
	Drain                bool      `json:"drain"`
	ResultsAcceptedUntil string    `json:"results_accepted_until"`
	Reason               string    `json:"reason"`
	Cancel               *[]string `json:"cancel"`
}

// Stopper stops a task in flight on this host, which is driver.Docker.
type Stopper interface {
	Stop(ctx context.Context, s graph.Stop) error
}

// Drain is the drain order the last answer carried: "take nothing new; finish what is held".
type Drain struct {
	Ordered bool

	// Reason is why, for this runner's log, and never what the order is obeyed on.
	Reason string

	// ResultsAcceptedUntil is the end of a revocation's grace, and zero where the runner was
	// drained and not revoked.
	ResultsAcceptedUntil time.Time
}

// Heartbeat posts one runner's heartbeat and acts on each answer.
type Heartbeat struct {
	Client      *Client
	Runner      string
	Concurrency int

	// Holding answers the keys this host answers for now: the loop's and those of the results
	// still to publish. Nil holds nothing.
	Holding func() []string

	// Earlier are the keys an earlier agent on this host took and never ended, as the driver's
	// record lists them when this one starts, newest first. They are named from the first
	// heartbeat until EarlierFor has passed since it, zero being bus.AckWait.
	//
	// Some were redeemed, and so are bound to this runner with their containers perhaps
	// still running, and a restart that stopped naming them would have each declared lost 30
	// seconds on, spending one of its key's requeues. A message the earlier agent took and
	// never acknowledged comes round once AckWait has passed since it was handed out, which
	// is before this agent started plus AckWait, and one that comes back here is redeemed
	// again as its holder and its container adopted by label, the loop then naming the key.
	// Past that, a key no message will bring back is one whose message was acknowledged, and
	// naming it longer would keep its run waiting on a container nobody collects until the
	// deadline, where letting it go has it declared lost and its requeue, likeliest to come
	// back to this host, adopts the container.
	Earlier    []agk.TaskID
	EarlierFor time.Duration

	// Stopper is what a key the answer cancels is stopped through.
	Stopper Stopper

	// Log is where the agent writes a line.
	Log func(string)

	// Every is the interval, zero being HeartbeatInterval. Now is the clock sent_at is read
	// from, nil being the time of day.
	Every time.Duration
	Now   func() time.Time

	mu       sync.Mutex
	first    time.Time
	answered time.Time
	drain    Drain
	drifted  bool
	cut      bool
	unnamed  map[string]bool
	stopping sync.WaitGroup
}

// joinAgain is what the agent ends saying when a heartbeat is answered 401: the credential opens
// nothing, whether it was revoked past its grace, rotated past or never existed, and asking again
// gets the same answer, so the one thing left to do is the operator's.
const joinAgain = "this runner's credential was refused: join it again with agk-runner join --replace"

func (h *Heartbeat) every() time.Duration {
	if h.Every > 0 {
		return h.Every
	}
	return HeartbeatInterval
}

func (h *Heartbeat) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Heartbeat) say(s string) {
	if h.Log != nil {
		h.Log(s)
	}
}

// Drain is the drain order the last answer carried.
func (h *Heartbeat) Drain() Drain {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drain
}

// First sends heartbeats until one is answered, which is when the agent counts as started: a
// runner whose API refuses it is not one to count as started.
//
// An API that does not answer is asked again, soon and then once an interval, since the keys an
// earlier agent held are declared lost three intervals after its last heartbeat. Any other refusal
// is the same on every try and ends the start: a 401 says the credential opens nothing, a 403 that
// it is another runner's than the one runner.env names. A context that ends answers nil.
//
// Under systemd a start that waits here longer than the unit's start timeout is timed out and
// restarted, and that is accepted: an API that does not answer hears no heartbeat either way, so
// the restart loses nothing, and until it answers the runner is not one to count as started.
func (h *Heartbeat) First(ctx context.Context) error {
	wait := retryFirst
	for {
		err := h.Beat(ctx)
		switch {
		case err == nil, ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrCredentialRefused):
			return fmt.Errorf("runner: %s: %w", joinAgain, err)
		case !errors.Is(err, ErrUnavailable):
			return fmt.Errorf("runner: the first heartbeat was refused, and a runner the API refuses takes no work: %w", err)
		}
		wait = min(wait, h.every())
		h.say(fmt.Sprintf("the first heartbeat got no answer, and is sent again in %s: %s", wait, err))
		if !sleep(ctx, wait) {
			return nil
		}
		wait *= 2
	}
}

// Run sends a heartbeat every interval until ctx ends, and answers nil then. A heartbeat answered
// 401 ends it with an error saying the runner is to join again, which the agent stops on: its
// credential opens nothing, and a loop asking again would only ask again. Any other failure is said
// and the next interval tries again, since the tasks this host holds are lost only after three.
func (h *Heartbeat) Run(ctx context.Context) error {
	tick := time.NewTicker(h.every())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		err := h.Beat(ctx)
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, ErrCredentialRefused):
			return fmt.Errorf("runner: %s: %w", joinAgain, err)
		default:
			h.mu.Lock()
			last := h.answered
			h.mu.Unlock()
			h.say(fmt.Sprintf("a heartbeat was not answered, and the next goes in %s; the last answered was at %s, and a task this host holds is declared lost %s after it: %s",
				h.every(), last.UTC().Format(time.RFC3339), 3*HeartbeatInterval, err))
		}
	}
}

// Wait returns once every stop an answer ordered has been sent or given up.
func (h *Heartbeat) Wait() { h.stopping.Wait() }

// Beat sends one heartbeat and acts on its answer.
//
// It is given up after one interval, so that a slow API delays the next heartbeat rather than
// holding it for the client's own bound.
func (h *Heartbeat) Beat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.every())
	defer cancel()

	sent := h.now()
	h.mu.Lock()
	if h.first.IsZero() {
		h.first = sent
	}
	state := "ready"
	if h.drain.Ordered {
		// "draining is what a runner reports once a drain order or a revocation has
		// reached it."
		state = "draining"
	}
	h.mu.Unlock()
	named := h.tasks(sent)

	var a beatAnswer
	err := h.Client.Do(ctx, http.MethodPost, heartbeatPath, beatRequest{
		Runner: h.Runner, AgentVersion: Version(), State: state, Concurrency: h.Concurrency,
		Tasks: named, SentAt: sent.UTC().Format(time.RFC3339Nano),
	}, &a)
	if err != nil {
		return err
	}
	received, err := time.Parse(time.RFC3339Nano, a.ReceivedAt)
	switch {
	case err != nil:
		return fmt.Errorf("runner: POST %s: the answer's received_at %q is not an RFC 3339 instant", heartbeatPath, printable(a.ReceivedAt))
	case a.Cancel == nil:
		return fmt.Errorf("runner: POST %s: the answer carries no cancel, which the wire's always does", heartbeatPath)
	}

	arrived := h.now()
	h.mu.Lock()
	h.answered = sent
	h.mu.Unlock()
	h.clock(sent, received, arrived)
	h.ordered(a)
	h.cancel(ctx, named, *a.Cancel)
	return nil
}

// tasks are the keys a heartbeat sent at now names: those held, then the earlier agent's while
// they are named, each once, and no more than the API takes in one.
func (h *Heartbeat) tasks(now time.Time) []string {
	named := []string{}
	seen := map[string]bool{}
	cut := false
	var off []string
	add := func(key string) {
		switch {
		case seen[key]:
		case !keyForm.MatchString(key) || agk.TaskID(key).Validate() != nil:
			seen[key] = true
			off = append(off, key)
		case len(named) == beatMaxTasks:
			cut = true
		default:
			seen[key] = true
			named = append(named, key)
		}
	}
	if h.Holding != nil {
		for _, key := range h.Holding() {
			add(key)
		}
	}
	h.mu.Lock()
	window := h.EarlierFor
	if window <= 0 {
		window = bus.AckWait
	}
	earlier := now.Sub(h.first) < window
	h.mu.Unlock()
	if earlier {
		for _, key := range h.Earlier {
			add(string(key))
		}
	}

	h.mu.Lock()
	said := h.cut
	h.cut = cut
	var fresh []string
	for _, key := range off {
		if !h.unnamed[key] {
			fresh = append(fresh, key)
		}
	}
	h.unnamed = map[string]bool{}
	for _, key := range off {
		h.unnamed[key] = true
	}
	h.mu.Unlock()
	for _, key := range fresh {
		h.say(fmt.Sprintf("the key %.100q is not named in the heartbeat, since it is not an idempotency key as the wire writes one and the API would refuse the heartbeat whole", key))
	}
	if cut && !said {
		// The ones held come first, and of the earlier agent's the newest, so what is left
		// out is what an earlier agent took longest ago.
		h.say(fmt.Sprintf("this host answers for more than %d keys, which is as many as one heartbeat names, and the rest are left out until it answers for fewer", beatMaxTasks))
	}
	return named
}

// clock says, once each time it happens, that this host's clock and the installation's are more
// than clockSkew apart, and that they are no longer.
//
// The request's own transit is not drift: on clocks that agree, received_at falls between sent_at
// and the answer's arrival however slow the API was. So the host is ahead by what sent_at is past
// received_at, and behind by what received_at is past the arrival, and by nothing else.
func (h *Heartbeat) clock(sent, received, arrived time.Time) {
	ahead, behind := sent.Sub(received), received.Sub(arrived)
	drifted := ahead > clockSkew || behind > clockSkew
	h.mu.Lock()
	was := h.drifted
	h.drifted = drifted
	h.mu.Unlock()
	switch {
	case drifted && !was && ahead > clockSkew:
		h.say(fmt.Sprintf("this host's clock is %s ahead of the installation's, by the heartbeat's sent_at and received_at: a deadline is an instant the installation wrote, so a container here is stopped that much early. Correct the host's clock", ahead.Round(time.Millisecond)))
	case drifted && !was:
		h.say(fmt.Sprintf("this host's clock is %s behind the installation's, by the heartbeat's sent_at and received_at: a deadline is an instant the installation wrote, so a container here is stopped that much late. Correct the host's clock", behind.Round(time.Millisecond)))
	case !drifted && was:
		h.say("this host's clock is back within a second of the installation's")
	}
}

// ordered keeps the drain order an answer carried, and says so when it changes.
func (h *Heartbeat) ordered(a beatAnswer) {
	d := Drain{Ordered: a.Drain}
	if a.Drain {
		d.Reason = printable(a.Reason)
		if until, err := time.Parse(time.RFC3339Nano, a.ResultsAcceptedUntil); err == nil {
			d.ResultsAcceptedUntil = until
		}
	}
	h.mu.Lock()
	was := h.drain
	h.drain = d
	h.mu.Unlock()
	// Said again where the order changes while it stands, a drain followed by a revocation
	// above all, whose reason and grace are what the host's journal is read for.
	changed := d.Reason != was.Reason || !d.ResultsAcceptedUntil.Equal(was.ResultsAcceptedUntil)
	switch {
	case d.Ordered && (!was.Ordered || changed):
		s := "the API orders this runner to drain: it is to take nothing new and finish what it holds"
		if d.Reason != "" {
			s += ", because " + d.Reason
		}
		if !d.ResultsAcceptedUntil.IsZero() {
			s += fmt.Sprintf(". It is revoked, and its results are accepted until %s", d.ResultsAcceptedUntil.UTC().Format(time.RFC3339))
		}
		h.say(s)
	case !d.Ordered && was.Ordered:
		h.say("the API no longer orders this runner to drain")
	}
}

// cancel stops each key the answer names, "by sending the container SIGTERM and then SIGKILL after
// the grace period", which is the driver's stop. Only a key the request named is stopped, since
// "only keys the request listed ever appear here", and one that does not is no order to this host.
//
// The stops go out beside the next heartbeat rather than before it: a stop waits on the daemon,
// and a daemon slow to answer would otherwise hold up the heartbeat that keeps every other task
// alive. The key stays named until its task is answered, and so stays in cancel, which stops a
// stopped task again, and the driver takes a second stop as it takes a duplicate one.
func (h *Heartbeat) cancel(ctx context.Context, named, cancel []string) {
	var stop []agk.TaskID
	for _, key := range cancel {
		if slices.Contains(named, key) {
			stop = append(stop, agk.TaskID(key))
		}
	}
	if len(stop) == 0 || h.Stopper == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	h.stopping.Add(1)
	go func() {
		defer h.stopping.Done()
		ctx, cancel := context.WithTimeout(ctx, h.every())
		defer cancel()
		for _, key := range stop {
			// The answer says only that the controller ended the dispatch, cancelled or
			// timed_out, and the ending it recorded stands whatever this host then
			// reports, so the stop is a cancellation either way.
			if err := h.Stopper.Stop(ctx, graph.Stop{Task: key, Reason: graph.StopCancelled}); err != nil {
				h.say(fmt.Sprintf("task %s, which the heartbeat's answer cancels, could not be stopped, and is stopped again at the next: %s", key, err))
			}
		}
	}()
}
