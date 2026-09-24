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
// driver reads when it is opened. Only a start that passes all of them asks the API anything, and it
// says it is ready once the API has answered its first heartbeat.
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

	if policy.Helper == "" {
		host := e.Host
		if host == nil {
			host = driver.KernelHost()
		}
		helper, err := layHelper(e.HelperFile, cfg.WorkDir, host, log)
		if err != nil {
			fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
			return exitRefused
		}
		policy.Helper = helper
	}

	// Limits is left at its zero value, which is agk's own size rules: no installation setting
	// changes them, and the controller holds a task's envelopes to the same ones.
	socket, _ := e.Lookup("DOCKER_HOST")
	endings := &runner.Endings{}
	d, err := openDriver(ctx, driver.Config{
		Socket:   socket,
		WorkRoot: cfg.WorkDir,
		Policy:   policy,
		Observer: endings,
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

	// Before any work is taken, so that nothing this agent is carrying can be taken for
	// something an earlier one left. A sweep that could not look is said and does not refuse
	// the start: what it would have removed only costs address space, and the start that
	// follows has everything else it needs of the daemon.
	if err := d.Sweep(ctx); err != nil {
		log(err.Error())
	}

	client, err := runner.NewClient(cfg.API, cfg.Credential, nil)
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}

	notify, _ := e.Lookup(runner.NotifySocket)
	err = runner.Serve(ctx, runner.Agent{
		Config: cfg, Driver: d, Client: client, Endings: endings, Log: log,
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
		d, err := newDriver(cfg)
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

// newDriver is driver.New, and a variable so that a test can read the configuration serve opens the
// driver with, which is where every host setting it settled on ends up.
var newDriver = driver.New

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

// layHelper is the helper a script step is given where runner.toml names none: the one installed
// beside the agent, laid down under the work root, or none where none is installed.
//
// The default is the installed helper rather than none because the page offers /agk/bin/agk to
// every script step, and a runner that binds it only where an operator thought to write a line is
// one whose scripts work on some hosts of a pool and say agk: not found on others. A helper that
// runner.toml names is taken as written, since the operator who wrote it has chosen the file; the
// driver stats it in the agent's filesystem and the daemon binds it from the host's, so in the
// container form it has to be at the same path in both.
//
// A work root on a filesystem mounted noexec binds none. The copy's bind carries the mount's
// flags, so /agk/bin/agk would be a program no script may run, and every script step that called
// it would fail on the brick's account for a choice about the host's disk.
func layHelper(installed, workDir string, host driver.Host, log func(string)) (string, error) {
	fs, err := host.Filesystem(workDir)
	if err != nil {
		return "", fmt.Errorf("%s, the work root, could not be asked what it is mounted as: %w", workDir, err)
	}
	if fs.NoExec {
		log("the work root " + workDir + " is on a filesystem mounted noexec, which a bind of the static helper from it would carry, so a script step finds no " + driver.BinPath + ": name a helper outside it with helper in runner.toml")
		return "", nil
	}
	path, ok, err := runner.LayHelper(installed, workDir)
	switch {
	case err != nil:
		return "", err
	case !ok:
		log("there is no " + installed + " and runner.toml names no helper, so a script step finds no " + driver.BinPath + ", and reads its inputs with jq instead")
		return "", nil
	}
	log("the static helper " + installed + " is bound read-only at " + driver.BinPath + " for a script step, from its copy " + path + " under the work root, where the daemon finds it in either form")
	return path, nil
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
