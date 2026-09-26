//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/agentiik/agentiik/runner"
)

// socketGroup is the group that owns the daemon's socket, which is what gives an account the
// daemon on a host where the socket is not the account's own.
func socketGroup(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("the daemon socket %s cannot be read, and the agent takes the group that owns it: %s", path, reasonOf(err))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case info.Mode()&fs.ModeSocket == 0:
		return 0, fmt.Errorf("%s is not a socket, and it is where the daemon is reached: mount the host's /var/run/docker.sock there, or name another with DOCKER_HOST", path)
	case !ok:
		return 0, fmt.Errorf("the group that owns %s cannot be read on this platform", path)
	}
	return int(st.Gid), nil
}

// giveDir makes dir where it is not, with mode, its parents as a directory a host is given, and
// gives dir alone to agent.
//
// The directory is opened without following a link at its last step and given through what was
// opened, so that a link put in its place, by an agent of an earlier start that owned its parent,
// is refused rather than followed to a file root would then give away.
func giveDir(dir string, mode fs.FileMode, agent runner.Owner) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("%s cannot be created: %s", filepath.Dir(dir), reasonOf(err))
	}
	if err := os.Mkdir(dir, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s cannot be created: %s", dir, reasonOf(err))
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("%s is not a directory that can be given to the agent's account: %s", dir, reasonOf(err))
	}
	defer f.Close()
	if err := f.Chown(agent.UID, agent.GID); err != nil {
		return fmt.Errorf("%s cannot be given to account %d, which takes CAP_CHOWN: %s", dir, agent.UID, reasonOf(err))
	}
	return nil
}

// becomeAgent is the kernel's Become: this program, started again as agent in groups.
func becomeAgent(agent runner.Owner, groups []int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("the path of this program cannot be read, and it starts itself again as the agent's account: %w", err)
	}
	return become(agent, groups, self, os.Args, os.Environ())
}

// become sets this process's groups, group and user, in that order since each after the first
// takes a privilege the next gives up, then replaces it with path.
//
// Go sets them on every thread of the process, and the user is checked after, so that a process
// that is still root in any way never reaches the exec.
func become(agent runner.Owner, groups []int, path string, argv, envv []string) error {
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("the groups %v cannot be taken, which takes CAP_SETGID, cap_add SETGID in Compose: %w", groups, err)
	}
	if err := syscall.Setgid(agent.GID); err != nil {
		return fmt.Errorf("group %d cannot be taken, which takes CAP_SETGID, cap_add SETGID in Compose: %w", agent.GID, err)
	}
	if err := syscall.Setuid(agent.UID); err != nil {
		return fmt.Errorf("account %d cannot be taken, which takes CAP_SETUID, cap_add SETUID in Compose: %w", agent.UID, err)
	}
	if os.Getuid() != agent.UID || os.Geteuid() != agent.UID || os.Getgid() != agent.GID || os.Getegid() != agent.GID {
		return fmt.Errorf("this process is still not account %d in group %d after taking them", agent.UID, agent.GID)
	}
	err := syscall.Exec(path, argv, envv)
	return fmt.Errorf("%s could not be started again as account %d, which it needs to be to hold its file capabilities; the kernel refuses such an exec where a capability the file holds, CAP_CHOWN, CAP_FOWNER or CAP_DAC_OVERRIDE, is not in the container's bounding set: %w", path, agent.UID, err)
}

// reasonOf is an error without the path it repeats, where it is one about a path.
func reasonOf(err error) string {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}
