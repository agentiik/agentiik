package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
)

// Error is what the daemon answered when it refused. The Engine API reports every
// refusal the same way, a status and a JSON object carrying one message, so one type
// covers all of them and a caller reads the status rather than the prose.
type Error struct {
	Status  int
	Message string
}

// Error writes the status and what the daemon said, in that order, because the status is
// what a caller branches on and the message is what a person reads.
func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the daemon answered %d", e.Status)
	}
	return fmt.Sprintf("the daemon answered %d: %s", e.Status, e.Message)
}

// unreachable is the daemon not being there at all, which is a different thing from the
// daemon refusing. A refusal is an answer and carries a status; this carries none,
// because nothing answered.
type unreachable struct {
	socket string
	err    error
}

func (u *unreachable) Error() string {
	return fmt.Sprintf("the Docker daemon at %s could not be reached: %v", u.socket, u.err)
}

func (u *unreachable) Unwrap() error { return u.err }

// IsNotFound says the daemon answered 404. It is asked of a container, a network or an
// image that is gone, which is ordinary rather than exceptional: a stop arriving for a
// container somebody already removed is the case at-least-once delivery produces.
func IsNotFound(err error) bool { return status(err) == 404 }

// IsConflict says the daemon answered 409, which is the state the request asked for
// being the state the object is already in: starting a container that is running,
// removing one that is not stopped, creating a name that exists.
func IsConflict(err error) bool { return status(err) == 409 }

// IsUnreachable says nothing answered. It is the question a driver asks before charging
// a failure to a brick, because a daemon that is not there failed nobody's step.
func IsUnreachable(err error) bool {
	var u *unreachable
	if errors.As(err, &u) {
		return true
	}
	// A dial onto a socket that is not there, or that this process may not open,
	// arrives as one of these three whether it came through the pooled transport or
	// through the raw connection the attach is dialed on.
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, fs.ErrNotExist) ||
		isNetOpError(err)
}

// isNetOpError catches the dial failures that arrive with no errno of their own.
func isNetOpError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// status is the status the daemon answered with, or zero where the error is not one of
// its refusals.
func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}
