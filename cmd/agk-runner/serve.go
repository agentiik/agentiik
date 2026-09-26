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
	"time"

	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/runner"
)

// serve is the agent. Everything it refuses, it refuses before any call to the API, in this order:
// the account it runs as, its settings and the host's policy, its key and credential, then the
// daemon, whose floors the driver reads when it is opened. Only a start that passes all of them asks
// the API anything, and it says it is ready once the API has answered its first heartbeat.
func serve(ctx context.Context, e env, args []string) int {
	log := logger(e.Err)
	if len(args) > 0 {
		// No flag, so that none can lift the floor or stand in for a setting: the settings
		// are in the environment and runner.env, and the host's in runner.toml.
		fmt.Fprintln(e.Err, "agk-runner serve: it takes no arguments, and reads its settings from the environment and "+e.EnvFile+" and the host's from "+e.PolicyFile)
		return exitUsage
	}
	if e.Geteuid() == 0 {
		return asRoot(e, log)
	}

	policy, perr := loadPolicy(e.PolicyFile, log)
	socket, derr := dockerHost(e)
	// Measured as join measured what it declared, and refused as join refuses it: a runner that
	// cannot say how much it has cannot put back what it has no room for.
	capacity, merr := runner.HostRoom(e.MemInfo)
	token, terr := runner.ReadJoinToken(e.Lookup)
	// A join token is used only once everything else read so far holds, so that none is spent
	// on a start that is refused anyway.
	if token != "" && errors.Join(perr, derr, merr) == nil {
		if code, done := joinFirst(ctx, e, token, socket, log); done {
			return code
		}
	}
	cfg, err := runner.ReadConfig(e.Lookup, e.EnvFile)
	// All are reported on the one start, so that an operator fixing a unit is told
	// everything that is wrong with it rather than one thing per restart.
	if err := errors.Join(err, perr, derr, merr, terr); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(e.Err, "agk-runner serve: "+line)
		}
		return exitRefused
	}

	// The key and the credential are read with the settings, before the daemon: a host whose
	// key is gone can never renew its credential, and is a new runner that joins again rather
	// than one to start and let run into its rotate_by. The credential is the one the agent
	// renewed to where it has, which runner.env, read-only to the agent, cannot hold.
	key, err := runner.LoadKey(e.KeyFile)
	var held runner.Held
	if err == nil {
		held, err = runner.ReadHeld(e.CredentialFile, cfg)
	}
	switch {
	case errors.Is(err, runner.ErrKeyGone):
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitJoinAgain
	case err != nil:
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
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
	endings := &runner.Endings{}
	d, err := openDriver(ctx, driver.Config{
		Socket:   socket,
		WorkRoot: cfg.WorkDir,
		Policy:   policy,
		Observer: endings,
		Logs:     runner.TaskLogs{},
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

	client, err := runner.NewClient(cfg.API, held.Credential, nil)
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}

	notify, _ := e.Lookup(runner.NotifySocket)
	err = runner.Serve(ctx, runner.Agent{
		Config: cfg, Capacity: capacity, Driver: d, Client: client, Endings: endings, Log: log,
		Ready: func() error { return runner.Notify(notify, runner.Ready) },
		Key:   key, Held: held, CredentialFile: e.CredentialFile,
	})
	switch {
	case errors.Is(err, runner.ErrCredentialRefused), errors.Is(err, runner.ErrKeyGone), errors.Is(err, runner.ErrRevoked):
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitJoinAgain
	case err != nil:
		fmt.Fprintln(e.Err, "agk-runner serve: "+err.Error())
		return exitRefused
	}
	return exitSucceeded
}

// joinPatience is how long serve waits for an API that does not answer its join, which is longer
// than an installation takes to come up beside it on a slow host, and short enough that a runner
// given an address nothing answers says so in its log within minutes. A variable so that a test
// need not wait that long.
var joinPatience = 5 * time.Minute

// joinFirst joins with the join token serve was given where this host has no identity yet, or has
// one its environment has moved on from, and says whether the start ends there, with its code.
//
// A runner given a join token is one its environment configures at every start, as a Compose file
// does, so the address and the labels it serves with are the environment's, never ones a first
// start wrote down and nothing changes afterwards. The identity cannot follow them, since the API
// checked what the runner claims against the token it joined with, so a runner whose environment
// says otherwise joins again, as join --replace does: a new runner with a new key, the one it was
// staying registered until an administrator revokes it. It joins as the account it serves as, so
// what it writes is that account's already.
func joinFirst(ctx context.Context, e env, token runner.Secret, socket string, log func(string)) (int, bool) {
	given := runner.JoinToken
	if v, _ := e.Lookup(runner.JoinTokenFile); v != "" {
		given = runner.JoinTokenFile
	}
	refuse := func(err error) (int, bool) {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(e.Err, "agk-runner serve: "+line)
		}
		return exitRefused, true
	}

	joined, err := runner.HasJoined(e.EnvFile, e.KeyFile)
	if err != nil {
		return refuse(err)
	}
	replace := false
	if joined {
		drifted, err := runner.Drifted(e.Lookup, e.EnvFile)
		switch {
		case err != nil:
			return refuse(err)
		case len(drifted) == 0:
			log("this host has joined already, and its environment claims what it joined with, so the join token in " + given + " is not used")
			return 0, false
		}
		log(strings.Join(drifted, ", ") + " in the environment says otherwise than " + e.EnvFile + ", which this runner joined with, so it joins again with the join token in " + given + " as a new runner, and the one it was stays registered until an administrator revokes it")
		replace = true
	}

	j, err := runner.JoinWhenReady(ctx, runner.Joining{
		Token:           token,
		Lookup:          e.Lookup,
		EnvironmentOnly: true,
		Replace:         replace,
		EnvPath:         e.EnvFile, KeyPath: e.KeyFile, MemInfo: e.MemInfo,
		CredentialPath: e.CredentialFile,
		Socket:         socket,
	}, joinPatience, log)
	switch {
	case ctx.Err() != nil:
		fmt.Fprintln(e.Err, "agk-runner serve: stopped while joining, before it was ready")
		return exitSucceeded, true
	case err != nil:
		return refuse(err)
	}
	claims := "no label"
	if len(j.Labels) > 0 {
		claims = "the labels " + strings.Join(j.Labels, ",")
	}
	log(fmt.Sprintf("this host joined pool %s as runner %s, claiming %s, with the join token in %s", j.Pool, j.Runner, claims, given))
	if len(j.Labels) == 0 && j.Pool != "default" {
		log("it claims no label, and pool " + j.Pool + " sends only steps that name one, so it will run none of them: set " + runner.Labels)
	}
	return 0, false
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
