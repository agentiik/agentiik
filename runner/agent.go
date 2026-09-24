package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/agentiik/agentiik/driver"
)

// Agent is everything Serve runs with, opened by whoever started it.
//
// It is a value rather than something Serve opens, so that the order a start happens in stays in
// one place, the program's: the settings are read, the floor is held and the daemon is opened
// before anything here runs, and a start refused at any of those has made no call to the API.
type Agent struct {
	Config Config
	Driver *driver.Docker
	Client *Client

	// Endings is the driver's Observer, which it was opened with, since a task's result is
	// assembled from what the driver told it. Serve chains the task's progress onto it.
	Endings *Endings

	// Log is where the agent writes a line, which is its own log and, under systemd, the
	// journal.
	Log func(string)

	// Ready is called once the agent is ready to be counted as started, which tells systemd. A
	// failure to say so ends Serve: a unit of Type=notify that never hears it is timed out and
	// restarted anyway, and a start that fails saying why is read where a timeout is not.
	Ready func() error
}

// Serve runs the agent until its context ends.
//
// Ready is said once the floor holds and the driver is open, which is everything the agent refuses
// a start for before it asks the API anything: those two are what a runner that should not be
// taking work refuses at. Then it asks the API for its bus credential, publishes whatever results
// an earlier agent on this host kept and never saw published, and takes work until it is stopped,
// returning once every task it holds has been answered or given up with it. The heartbeat, a later
// part, moves Ready to after its first answer, since a runner whose API refuses it is not one to
// count as started.
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
	if a.Ready != nil {
		if err := a.Ready(); err != nil {
			return err
		}
	}

	b, err := OpenBus(ctx, a.Client, a.Config, say)
	if err != nil || b == nil {
		return err
	}
	defer b.Close()

	// A result an earlier agent kept is published before anything new is taken, and one the bus
	// does not take now goes out with the loop's later flushes. One that could not be read back
	// is said, and does not stop the rest.
	results, err := OpenResults(a.Config.WorkDir, a.Config.Runner, b)
	if results == nil {
		return err
	}
	if err != nil {
		say(err.Error())
	}
	if err := results.Flush(ctx); err != nil && ctx.Err() == nil {
		say(err.Error())
	}

	progress := NewProgress(a.Config.Runner, b, say)
	a.Endings.Next = progress
	var publishing sync.WaitGroup
	defer publishing.Wait()
	publishing.Add(1)
	go func() {
		defer publishing.Done()
		progress.Run(ctx)
	}()

	loop := &Loop{
		Runner: a.Config.Runner, Pool: a.Config.Pool, Concurrency: a.Config.Concurrency, Labels: a.Config.Labels,
		Queue: b, Redeemer: a.Client, Holder: a.Driver,
		Carrier: &Carrier{
			Runner: a.Config.Runner, Driver: a.Driver, Endings: a.Endings, Results: results,
			// The driver is given nowhere to write a task's log yet, so a result addresses
			// none.
			Logs: false, Log: say,
		},
		Assembly: Assembly{WorkRoot: a.Config.WorkDir},
		Progress: progress,
		Log:      say,
	}
	return loop.Run(ctx)
}
