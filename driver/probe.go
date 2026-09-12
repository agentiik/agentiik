package driver

import (
	"context"
	"fmt"

	"github.com/agentiik/agentiik/internal/docker"
)

// Daemon is what one daemon says it is: where it was found, what is being spoken to it, what
// it runs, and whether it remaps user namespaces.
//
// It is the answer to the questions a caller has to settle before a driver exists. Which
// architecture a container runs natively decides which static helper may be bound into one,
// and a binary for the wrong machine gives a script an exec format error rather than a
// program. Whether the daemon remaps decides nothing here, because the floor is the Policy's
// and readUsernsFloor applies it, but a command line that is about to lift the floor can say
// what this machine is before it does.
type Daemon struct {
	// Socket is the path that answered, which is what the caller asked for or what the
	// resolution found: DOCKER_HOST, then the per-user path Docker Desktop uses, then the
	// system path.
	Socket string

	// APIVersion is what is being spoken, which is min(the ceiling, what the daemon
	// offers) and not what was compiled in.
	APIVersion string

	// OSType and Architecture are the platform a container runs on natively, in the
	// daemon's own spelling: linux and arm64, amd64, and so on.
	OSType       string
	Architecture string

	// UsernsRemapped is read off the daemon's own security options and not off a setting
	// somebody wrote down about it.
	UsernsRemapped bool
}

// Probe asks one daemon what it is, and closes.
//
// A package-level function rather than a method, because the whole point of it is to be
// asked before New: Policy.Helper has to be set when the configuration is built, the helper
// to name depends on the architecture a container runs on, and New keeps the Info it fetches
// to itself. The second Info call per run is the honest price of that ordering, and it is one
// round trip over a unix socket.
//
// It is also what keeps cmd/agk off the Engine API. internal/docker is module-wide, so the
// command line could reach a daemon directly; asking for the facts here leaves this package
// the only one in the module that does, which is the rule the root doc.go states.
//
// Nothing is created, started or pulled. A caller that only wants to know whether a daemon is
// there gets an error wrapping ErrDaemonUnreachable where it is not, which is the same
// sentinel New answers with and the same exit code the command line gives it.
func Probe(ctx context.Context, socket string) (Daemon, error) {
	cli, err := docker.Dial(socket)
	if err != nil {
		if docker.IsUnreachable(err) {
			return Daemon{}, fmt.Errorf("driver: %w: %v", ErrDaemonUnreachable, err)
		}
		return Daemon{}, fmt.Errorf("driver: %w", err)
	}
	defer cli.Close()

	info, err := cli.Info(ctx)
	if err != nil {
		if docker.IsUnreachable(err) {
			return Daemon{}, fmt.Errorf("driver: %w: %v", ErrDaemonUnreachable, err)
		}
		return Daemon{}, fmt.Errorf("driver: the daemon at %s could not be asked what it is: %w", cli.Socket(), err)
	}
	return Daemon{
		Socket:         cli.Socket(),
		APIVersion:     cli.APIVersion(),
		OSType:         info.OSType,
		Architecture:   info.Architecture,
		UsernsRemapped: info.UsernsRemapped(),
	}, nil
}
