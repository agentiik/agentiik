package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/runner"
)

// serve is the agent. Everything it refuses, it refuses before any call to the API, in this order:
// the account it runs as, its settings and the host's policy, then the daemon, whose floors the
// driver reads when it is opened. Only a start that passes all of them says it is ready.
func serve(ctx context.Context, e env, args []string) int {
	log := logger(e.Err)
	if len(args) > 0 {
		// No flag, so that none can lift the floor or stand in for a setting: the settings
		// are in the environment and runner.env, and the host's in runner.toml.
		fmt.Fprintln(e.Err, "agk-runner serve: it takes no arguments, and reads its settings from the environment and "+e.EnvFile+" and the host's from "+e.PolicyFile)
		return exitUsage
	}
	if e.Geteuid() == 0 {
		fmt.Fprintln(e.Err, "agk-runner serve: refusing to run as root. The agent runs as its own account, in the group that owns the daemon socket and holding CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE, and a root agent is one whose every mistake is made as the host's root")
		return exitRefused
	}

	cfg, err := runner.ReadConfig(e.Lookup, e.EnvFile)
	policy, perr := loadPolicy(e.PolicyFile, log)
	// Both are reported on the one start, so that an operator fixing a unit is told
	// everything that is wrong with it rather than one thing per restart.
	if err := errors.Join(err, perr); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(e.Err, "agk-runner serve: "+line)
		}
		return exitRefused
	}

	// The work root is created here rather than at the first task, so that a path the agent
	// cannot write is a refused start and not a first task failed on the runner's account.
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		fmt.Fprintf(e.Err, "agk-runner serve: %s, the work root %s names, cannot be created: %s\n", cfg.WorkDir, runner.WorkDir, err)
		return exitRefused
	}

	// Limits is left at its zero value, which is agk's own size rules: no installation setting
	// changes them, and the controller holds a task's envelopes to the same ones.
	socket, _ := e.Lookup("DOCKER_HOST")
	d, err := openDriver(ctx, driver.Config{
		Socket:   socket,
		WorkRoot: cfg.WorkDir,
		Policy:   policy,
		Announce: log,
		Host:     e.Host,
	})
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(e.Err, "agk-runner serve: stopped while the daemon was being opened, before it was ready")
		return exitSucceeded
	case err != nil:
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}
	defer d.Close()

	client, err := runner.NewClient(cfg.API, cfg.Credential, nil)
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}

	notify, _ := e.Lookup(runner.NotifySocket)
	err = runner.Serve(ctx, runner.Agent{
		Config: cfg, Driver: d, Client: client, Log: log,
		Ready: func() error { return runner.Notify(notify, runner.Ready) },
	})
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}
	return exitSucceeded
}

// openDriver opens the driver on the daemon, or gives up on it when the agent is stopped.
//
// driver.New takes no context, and it asks the daemon what it is with none: a daemon that answers
// its ping and then hangs would hold the start until the service manager killed it, since the
// signal that should stop it is the one this process has taken over. A driver that opens after
// the agent gave up on it is closed as it arrives.
func openDriver(ctx context.Context, cfg driver.Config) (*driver.Docker, error) {
	type opened struct {
		d   *driver.Docker
		err error
	}
	done := make(chan opened, 1)
	go func() {
		d, err := driver.New(cfg)
		done <- opened{d, err}
	}()
	select {
	case o := <-done:
		return o.d, o.err
	case <-ctx.Done():
		go func() {
			if o := <-done; o.d != nil {
				o.d.Close()
			}
		}()
		return nil, context.Canceled
	}
}

// loadPolicy reads the host's runner.toml.
//
// A file that is not there is the one case that falls back, to DefaultPolicy, whose floors are in
// place: an absent line is not a decision, and neither is an absent file. A file that is there and
// cannot be read or parsed refuses the start, since falling back from it would drop whatever it
// said, a lifted floor as readily as a lowered pids limit, and the operator who wrote it would not
// be told.
func loadPolicy(path string, log func(string)) (driver.Policy, error) {
	p, err := driver.LoadPolicy(path)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, fs.ErrNotExist):
		log("there is no " + path + ", so every host setting keeps its default, and require_userns_remap holds")
		return driver.DefaultPolicy(), nil
	}
	return driver.Policy{}, err
}

// logger writes one line of the agent's log at a time, from whichever goroutine says it.
func logger(w io.Writer) func(string) {
	var mu sync.Mutex
	return func(s string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(w, "agk-runner: "+s)
	}
}
