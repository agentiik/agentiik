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

	socket, _ := e.Lookup("DOCKER_HOST")
	d, err := driver.New(driver.Config{
		Socket:   socket,
		WorkRoot: cfg.WorkDir,
		Policy:   policy,
		Announce: log,
	})
	if err != nil {
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
