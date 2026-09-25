package runner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
)

// Agent is everything Serve runs with, opened by whoever started it.
//
// It is a value rather than something Serve opens, so that the order a start happens in stays in
// one place, the program's: the settings are read, the floor is held and the daemon is opened
// before anything here runs, and a start refused at any of those has made no call to the API.
type Agent struct {
	Config Config

	// Driver is opened with Endings as its Observer and TaskLogs as its Logs, and Client is what
	// each task's log is shipped through.
	Driver *driver.Docker
	Client *Client

	// Endings is the driver's Observer, which it was opened with, since a task's result is
	// assembled from what the driver told it. Serve chains the task's progress onto it.
	Endings *Endings

	// Log is where the agent writes a line, which is its own log and, under systemd, the
	// journal.
	Log func(string)

	// Ready is called once the agent is ready to be counted as started, which is once its first
	// heartbeat is answered, and tells systemd. A failure to say so ends Serve: a unit of
	// Type=notify that never hears it is timed out and restarted anyway, and a start that fails
	// saying why is read where a timeout is not.
	Ready func() error

	// Key is the host's private key, and Held the credential Client carries and its window,
	// which the agent renews ahead of its rotate_by with the key and keeps in CredentialFile.
	// A nil Key renews nothing, which is a test's.
	Key            ed25519.PrivateKey
	Held           Held
	CredentialFile string

	// every is the heartbeat's interval and earlierFor how long an earlier agent's keys are
	// named, zero being HeartbeatInterval and bus.AckWait, which a test shortens.
	every, earlierFor time.Duration

	// wait is how long one take waits for work, zero being the loop's own, which a test whose
	// bus credentials last seconds shortens.
	wait time.Duration
}

// Serve runs the agent until its context ends.
//
// The floor and the daemon are held before it is called, which is everything the agent refuses a
// start for before it asks the API anything. It then names, in a first heartbeat, every key an
// earlier agent on this host held when it stopped and every result it kept, and says Ready once
// that heartbeat is answered: a runner whose API refuses it is not one to count as started, and a
// restart that waited on anything else first would leave what the earlier agent held unnamed for
// longer than the three intervals that have it declared lost. Then it asks the API for its bus
// credential, listens for stops on it, publishes the kept results, and takes work until it is
// stopped, heartbeating every interval throughout, and returns once every task it holds has been
// answered or given up with it. The runner credential is renewed at two thirds of its window and
// the bus credential at three quarters of its life, both while the work goes on.
//
// Ready comes before the bus credential and not after it. The heartbeat is where the API says it
// accepts this runner, and the credential is asked for with the same one; a bus not reachable yet is
// waited for, as OpenBus says, and a Ready held back on it would have systemd time the start out
// and restart an agent doing the right thing, its heartbeat stopping with every restart.
//
// A heartbeat answered 401 ends the agent with an error saying to join again: the credential opens
// nothing, and an agent asking again for ever would only ask again.
//
// A drain order has it take nothing new, and a message taken as the order came is put back before
// it is redeemed. What it holds is carried to its result, and a drained runner then stays up, idle
// and reporting draining, until the order is lifted or it is revoked. A revoked one returns
// ErrRevoked once it holds nothing and every result it kept is published, and one whose grace
// ends first is answered 401 at its next heartbeat, which ends it saying to join again: the grace
// is the most it is given, and not something it waits out.
func Serve(ctx context.Context, a Agent) error {
	switch {
	case a.Driver == nil:
		return errors.New("runner: the agent has no driver, and it is opened before the agent serves")
	case a.Client == nil:
		return errors.New("runner: the agent has no client for the API")
	case a.Endings == nil:
		return errors.New("runner: the agent has no Endings, and the driver is opened with them as its observer, since a result is assembled from what the driver told of the ending")
	}
	say := a.Log
	if say == nil {
		say = func(string) {}
	}

	namespaces := "every namespace its pool accepts"
	if len(a.Config.Namespaces) > 0 {
		namespaces = strings.Join(a.Config.Namespaces, ", ")
	}
	say(fmt.Sprintf("agk-runner %s serving as %s in pool %s: %d tasks at once under %s, labels %s, namespaces %s, the daemon speaking API %s",
		Version(), a.Config.Runner, a.Config.Pool, a.Config.Concurrency, a.Config.WorkDir,
		strings.Join(a.Config.Labels, ","), namespaces, a.Driver.APIVersion()))

	// A stop that arrived while the agent was starting is not followed by a ready it would
	// contradict.
	if ctx.Err() != nil {
		return nil
	}
	// The agent stops on its own context ending and on a heartbeat refused, and the second is
	// what it then returns.
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	// The results an earlier agent kept are read now, so that the first heartbeat names their
	// keys, and published once the bus is open. A result that could not be read back is said,
	// and does not stop the rest.
	later := &laterBus{}
	results, err := OpenResults(a.Config.WorkDir, a.Config.Runner, later)
	if results == nil {
		return err
	}
	if err != nil {
		say(err.Error())
	}
	earlier, err := a.Driver.Dispatched()
	if err != nil {
		say(err.Error() + ": the keys an earlier agent held are not named, and the sweep declares lost those still bound to this runner")
	}

	loop, beat, stops := a.parts(results, earlier, say)
	defer stops.Wait()
	defer beat.Wait()
	if err := beat.First(ctx); err != nil || ctx.Err() != nil {
		return err
	}
	if a.Ready != nil {
		if err := a.Ready(); err != nil {
			return err
		}
	}

	// Each wait below is preceded by what ends what it waits on, so that a return that is not
	// the context ending, a bus refused or a loop that could not start, does not wait for ever.
	// The heartbeat goes on past a stop, until Serve returns: the loop answers for every task
	// it holds before it does, and a result kept or a key redeemed again while it winds down
	// is still this host's to name, which a heartbeat that ended with the stop would leave
	// unnamed for the wind-down and the restart together.
	beatCtx, endBeat := context.WithCancel(context.WithoutCancel(ctx))
	var beating sync.WaitGroup
	defer beating.Wait()
	defer endBeat()
	beating.Add(1)
	go func() {
		defer beating.Done()
		if err := beat.Run(beatCtx); err != nil {
			stop(err)
		}
	}()

	// The runner credential is renewed from the start, the one join wrote at once, since the
	// agent was never told its window. A renewal refused for good ends the agent as a refused
	// heartbeat does.
	if a.Key != nil {
		rotator := NewRotator(a.Client, a.Config.Runner, a.Key, a.CredentialFile, a.Held)
		rotator.Log = say
		rotator.Revoked = beat.Revoked
		var rotating sync.WaitGroup
		defer rotating.Wait()
		defer stop(nil)
		rotating.Add(1)
		go func() {
			defer rotating.Done()
			if err := rotator.Run(ctx); err != nil {
				stop(err)
			}
		}()
	}

	b, expires, err := OpenBus(ctx, a.Client, a.Config, say)
	switch {
	case errors.Is(err, ErrCredentialRefused):
		return fmt.Errorf("runner: %s: %w", joinAgain, err)
	case err != nil:
		return err
	case b == nil:
		return refused(ctx)
	}

	// Stops are listened for before anything is taken, since a task taken first could be
	// stopped in the moment before anybody was listening, and for as long as Serve runs rather
	// than as long as its context, since the loop answers for what it holds after a stop and a
	// container still running then is still one to stop. The subscription ends as the bus is
	// closed. A subscription refused, or one the bus does not confirm, ends the agent before it
	// has taken anything, to be started again: a runner that cannot hear stops would run every
	// stopped container to its deadline, and the heartbeat's cancel only catches what it misses.
	hearing, endHearing := context.WithCancel(context.WithoutCancel(ctx))
	defer endHearing()
	if err := stops.Hear(hearing, b); err != nil {
		b.Close()
		return err
	}

	// Everything that takes, publishes and says goes through tb, which replaces the connection
	// ahead of its credential's expiry, the replacement heard from for stops before anything
	// moves onto it. It is renewed for as long as Serve runs, as the heartbeat goes on, since
	// the results of the wind-down are published on it.
	tb := newTaskBus(b, expires, func(ctx context.Context) (busConn, time.Time, error) {
		b, expires, err := dialBus(ctx, a.Client, a.Config)
		if err != nil {
			return nil, time.Time{}, err
		}
		if err := stops.Hear(hearing, b); err != nil {
			b.Close()
			return nil, time.Time{}, err
		}
		return b, expires, nil
	}, say)
	defer tb.Close()
	later.attach(tb)
	keepCtx, endKeep := context.WithCancel(context.WithoutCancel(ctx))
	var keeping sync.WaitGroup
	defer keeping.Wait()
	defer endKeep()
	keeping.Add(1)
	go func() {
		defer keeping.Done()
		if err := tb.Keep(keepCtx); err != nil {
			stop(err)
		}
	}()

	// A result an earlier agent kept is published before anything new is taken, and one the bus
	// does not take now goes out with the loop's later flushes.
	if err := results.Flush(ctx); err != nil && ctx.Err() == nil {
		say(err.Error())
	}

	progress := NewProgress(a.Config.Runner, tb, say)
	a.Endings.Next = progress
	var publishing sync.WaitGroup
	defer publishing.Wait()
	defer stop(nil)
	publishing.Add(1)
	go func() {
		defer publishing.Done()
		progress.Run(ctx)
	}()

	loop.Queue, loop.Progress = tb, progress
	if err := loop.Run(ctx); err != nil {
		return err
	}
	return refused(ctx)
}

// parts are the agent's loop, heartbeat and stops, bound to each other: the heartbeat names what the
// loop holds and every result kept, the loop takes nothing while the heartbeat's last answer orders
// a drain and ends once a revoked runner has answered for what it held, and a key the heartbeat's answer cancels is stopped through the same Stops as one heard
// on the bus, which stops what the loop and the earlier agent hold. The loop's bus and progress are
// given once the bus is open.
func (a Agent) parts(results *Results, earlier []agk.TaskID, say func(string)) (*Loop, *Heartbeat, *Stops) {
	loop := &Loop{
		Runner: a.Config.Runner, Pool: a.Config.Pool, Concurrency: a.Config.Concurrency, Labels: a.Config.Labels,
		Redeemer: a.Client, Holder: a.Driver,
		Carrier: &Carrier{
			Runner: a.Config.Runner, Driver: a.Driver, Endings: a.Endings, Results: results,
			// The driver is opened with TaskLogs, and each task's log is shipped through the
			// client, with the task's grant.
			Logs: a.Client, Log: say,
		},
		Assembly: Assembly{WorkRoot: a.Config.WorkDir},
		Log:      say,
		Wait:     a.wait,
	}
	// A result kept is of a task that has ended, so there is nothing of it to stop.
	stops := &Stops{
		Stopper: a.Driver, Log: say,
		Holding: func() []string {
			held := loop.Held()
			for _, key := range earlier {
				held = append(held, string(key))
			}
			return held
		},
	}
	beat := &Heartbeat{
		Client: a.Client, Runner: a.Config.Runner, Concurrency: a.Config.Concurrency,
		Holding: func() []string { return append(loop.Held(), results.Keys()...) },
		Earlier: earlier, EarlierFor: a.earlierFor,
		Stopper: stops, Log: say, Every: a.every,
	}
	loop.Draining = func() bool { return beat.Drain().Ordered }
	loop.Revoked = beat.Revoked
	loop.HeldBefore = func(key string) bool { return slices.Contains(earlier, agk.TaskID(key)) }
	loop.LetGo = stops.Forget
	return loop, beat, stops
}

// refused is the heartbeat's refusal where that is what ended the agent, and nil where its own
// context did.
func refused(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// laterBus is where the kept results are published through once the bus is open. They are read
// before it is, for the first heartbeat to name, and nothing publishes one until it is.
type laterBus struct {
	mu sync.Mutex
	b  Publisher
}

func (l *laterBus) attach(b Publisher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = b
}

func (l *laterBus) Report(ctx context.Context, r bus.TaskResult) error {
	l.mu.Lock()
	b := l.b
	l.mu.Unlock()
	if b == nil {
		return fmt.Errorf("runner: the result of %s: %w: the task bus is not open yet", r.TaskID, ErrUnavailable)
	}
	return b.Report(ctx, r)
}
