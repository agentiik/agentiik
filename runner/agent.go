package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// Ready is said once the floor holds and the driver is open, which is everything this version of
// the agent does before it would take work: those two are what a runner that should not be taking
// work refuses at. The parts that take, run, heartbeat and report are composed here as they
// arrive, and the heartbeat moves Ready to after its first answer, since a runner whose API
// refuses it is not one to count as started.
func Serve(ctx context.Context, a Agent) error {
	switch {
	case a.Driver == nil:
		return errors.New("runner: the agent has no driver, and it is opened before the agent serves")
	case a.Client == nil:
		return errors.New("runner: the agent has no client for the API")
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
	<-ctx.Done()
	return nil
}
