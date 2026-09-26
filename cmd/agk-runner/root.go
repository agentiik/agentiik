package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/runner"
)

// asRoot is serve started as root, as the image starts it: it prepares what the agent needs, takes
// the group that owns the daemon's socket, drops to the agent's account and starts itself again as
// that account, which serves. It never serves as root, and where any of that cannot be done it
// refuses the start rather than going on as root.
//
// It is how a Compose file runs the runner with nothing prepared on the host. The group that owns
// the socket differs from one host to the next, and a container given it with group_add needs its
// number written somewhere first; read here from the socket itself, it needs nothing. The work root
// is bound from the host at the same path, since the daemon resolves every bind source there, and a
// bind source Docker creates is root's; given to the agent here, it needs nothing either.
//
// It starts itself again rather than going on after the drop because a process that sets its user
// from root to another loses every capability it held, and what gives the agent back the three it
// needs, CAP_CHOWN, CAP_FOWNER and CAP_DAC_OVERRIDE, is the binary's file capabilities, which the
// kernel grants at an exec and at nothing else. The agent that comes of it holds those three and
// neither CAP_SETUID nor CAP_SETGID, so it cannot become root again.
func asRoot(e env, log func(string)) int {
	refuse := func(err error) int {
		fmt.Fprintln(e.Err, "agk-runner serve: started as root, it prepares the host for the account "+agentAccount+", takes the group that owns the daemon socket and serves as "+agentAccount+", and it refuses to serve as root, since a root agent is one whose every mistake is made as the host's root:")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(e.Err, "agk-runner serve: "+line)
		}
		return exitRefused
	}

	agent, aerr := e.Account(agentAccount)
	if aerr == nil && agent.UID == 0 {
		aerr = fmt.Errorf("the account %s is root, and the agent runs as an unprivileged account", agentAccount)
	}
	socket, serr := dockerHost(e)
	var group int
	if serr == nil {
		var path string
		if path, serr = docker.SocketPath(socket); serr == nil {
			group, serr = socketGroup(path)
		}
	}
	workRoot, werr := runner.WorkRoot(e.Lookup, e.EnvFile)
	// Everything is said on the one start, as serve says it, so that an operator fixing a
	// Compose file is told all that is wrong with it rather than one thing per restart.
	if err := errors.Join(aerr, serr, werr); err != nil {
		return refuse(err)
	}

	// The key and the credential are the agent's alone, and so is the work root; runner.env's
	// directory is given to it too, since serve joins as the agent where it is given a join token
	// and join writes runner.env beside where it goes. Each directory is given alone, never what
	// is in it: what the agent wrote there is its own already.
	for _, d := range []struct {
		dir  string
		mode fs.FileMode
	}{
		{filepath.Dir(e.KeyFile), 0o700},
		{filepath.Dir(e.CredentialFile), 0o700},
		{workRoot, 0o700},
		{filepath.Dir(e.EnvFile), 0o755},
	} {
		if err := giveDir(d.dir, d.mode, agent); err != nil {
			return refuse(err)
		}
	}

	groups := []int{agent.GID}
	if !slices.Contains(groups, group) {
		groups = append(groups, group)
	}
	become := e.Become
	if become == nil {
		become = becomeAgent
	}
	log(fmt.Sprintf("started as root: the daemon socket is group %d's, and the agent serves as account %d in groups %v, holding its three file capabilities", group, agent.UID, groups))
	if err := become(agent, groups); err != nil {
		return refuse(err)
	}
	// Only a test's Become returns without an error: the kernel's is replaced by the agent.
	return exitSucceeded
}
