package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// joinTokenMaxBytes is the most the file AGK_RUNNER_JOIN_TOKEN_FILE names is read to, which is what
// every AGK_*_FILE is held to. A join token is some fifty bytes.
const joinTokenMaxBytes = 64 << 10

// ReadJoinToken is the join token serve was given to join with, from AGK_RUNNER_JOIN_TOKEN or the
// file AGK_RUNNER_JOIN_TOKEN_FILE names, and empty where neither is set.
//
// The value is taken as well as the file, where a server program takes a secret from a file alone,
// because a join token is spent by the one join it is for and refused ever after: a runner on
// another machine is then one Compose file and one variable, with nothing to write on the host
// first. The file is for a token another program writes, as init writes the local runner's, and it
// is held to what every AGK_*_FILE is held to. Neither a value nor a path is ever repeated.
func ReadJoinToken(lookup Lookup) (Secret, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, byValue := lookup(JoinToken)
	path, byFile := lookup(JoinTokenFile)
	byValue, byFile = byValue && value != "", byFile && path != ""
	switch {
	case byValue && byFile:
		return "", &Error{Variable: JoinToken, Reason: "and " + JoinTokenFile + " are both set, and a runner given two join tokens would be guessing which one was meant: set one of them"}
	case byValue:
		return joinTokenOf(JoinToken, value)
	case !byFile:
		return "", nil
	case !filepath.IsAbs(path):
		return "", &Error{Variable: JoinTokenFile, Reason: "is not an absolute path, and it is one, so that the file read does not depend on the directory the agent was started from"}
	}

	// Opened without blocking, so that a FIFO in its place cannot hold the start for ever, and
	// checked on the descriptor, so that what is read is what was checked.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", &Error{Variable: JoinTokenFile, Reason: "names a file that cannot be read by the account the agent runs as: " + reasonOf(err)}
	}
	defer f.Close()
	info, err := f.Stat()
	switch {
	case err != nil:
		return "", &Error{Variable: JoinTokenFile, Reason: "names a file that cannot be read: " + reasonOf(err)}
	case !info.Mode().IsRegular():
		return "", &Error{Variable: JoinTokenFile, Reason: "names something that is not a file, and it is the file the join token is in"}
	case info.Mode().Perm()&0o077 != 0:
		return "", &Error{Variable: JoinTokenFile, Reason: fmt.Sprintf("names a file of mode %#o, and it holds a join token, which is readable by its owner alone: chmod 600 it, because a token anybody on the host can read is one anybody on the host can join with", info.Mode().Perm())}
	}
	text, err := io.ReadAll(io.LimitReader(f, joinTokenMaxBytes+1))
	switch {
	case err != nil:
		return "", &Error{Variable: JoinTokenFile, Reason: "names a file that cannot be read: " + reasonOf(err)}
	case len(text) > joinTokenMaxBytes:
		return "", &Error{Variable: JoinTokenFile, Reason: fmt.Sprintf("names a file of more than %d bytes, and a join token is some fifty", joinTokenMaxBytes)}
	}
	return joinTokenOf(JoinTokenFile, strings.TrimSpace(string(text)))
}

// joinTokenOf holds a join token to its form, naming the variable it came from.
func joinTokenOf(name, s string) (Secret, error) {
	switch {
	case s == "":
		return "", &Error{Variable: name, Reason: "names an empty file, and it is the file the join token an administrator issued is in"}
	case !isJoinToken(Secret(s)):
		return "", &Error{Variable: name, Reason: "is not a join token, which is written agkjoin_ followed by its secret"}
	}
	return Secret(s), nil
}

// HasJoined says whether this host has an identity already: a runner.env holding one, or the key a
// join writes before it.
//
// A key alone is an identity too, since join puts the key in place before runner.env and a host
// holding one has spent a token already: joining again would only be refused, or orphan the runner
// the key belongs to.
func HasJoined(envPath, keyPath string) (bool, error) {
	r := &reader{path: envPath, file: map[string]string{}, written: map[string]bool{}}
	_, identity := Joining{EnvPath: envPath}.existing(r)
	if err := r.err(); err != nil {
		return false, err
	}
	if identity {
		return true, nil
	}
	_, err := os.Lstat(keyPath)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("runner: %s cannot be read, so whether this host has already joined cannot be told: %s", keyPath, reasonOf(err))
}

// JoinWhenReady is Join, asked again while the API does not answer, for at most within.
//
// A runner started beside its installation, as Compose starts one, is often up before the API is,
// and an API that does not answer yet is one to wait for rather than a start to refuse. The wait
// doubles from a second to thirty, as every retry of the agent's does. It is bounded so that a
// runner pointed at an address nothing will ever answer says so and exits, for its service manager
// to start again, rather than waiting in silence. Every other refusal is the same on every try and
// ends the join at once. say is told of every attempt that got no answer.
//
// A join that got no answer may still have reached the API, which then spent the token, so an
// attempt refused after one that got none says the token may have been spent by it.
func JoinWhenReady(ctx context.Context, j Joining, within time.Duration, say func(string)) (Joined, error) {
	until := time.Now().Add(within)
	wait := retryFirst
	unanswered := false
	for {
		joined, err := Join(ctx, j)
		switch {
		case err == nil:
			return joined, nil
		case ctx.Err() != nil:
			return Joined{}, ctx.Err()
		case !errors.Is(err, ErrUnavailable):
			if unanswered {
				return Joined{}, fmt.Errorf("%w. An earlier attempt got no answer, and it may have reached the API and spent the token", err)
			}
			return Joined{}, err
		}
		unanswered = true
		left := time.Until(until)
		if left <= 0 {
			return Joined{}, fmt.Errorf("runner: the API has not answered a join for %s, so this runner stops asking and exits, for its service manager to start it again: check that %s is the API's address and that the API is up. The last attempt: %w", within, API, err)
		}
		wait = min(wait, left)
		say(fmt.Sprintf("the API did not answer the join, which is asked again in %s: %s", wait, err))
		if !sleep(ctx, wait) {
			return Joined{}, ctx.Err()
		}
		wait = min(2*wait, retryMost)
	}
}

// Drifted names the settings a runner claims at join, AGK_API, AGK_RUNNER_LABELS and
// AGK_RUNNER_NAMESPACES, that the environment says otherwise of than the runner.env this host
// joined with, the environment read as the whole of what is claimed: one unset there claims none.
//
// It is for a runner given a join token, whose environment is where it is configured at every
// start, as a Compose file configures it. The identity join wrote cannot follow a change to any of
// the three, since the API checked each against the token when it was claimed, so a runner whose
// environment moved on joins again rather than serving as the runner it was.
func Drifted(lookup Lookup, path string) ([]string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &reader{path: path, file: map[string]string{}, written: map[string]bool{}}
	there, _ := Joining{EnvPath: path}.existing(r)
	if err := r.err(); err != nil {
		return nil, err
	}
	var drifted []string
	for _, name := range joinWrites {
		now, _ := lookup(name)
		was := there[name]
		// The address is compared as the client reaches it, without its trailing slashes; a
		// list is compared as written, since its grammar allows it one spelling.
		if name == API {
			now, was = strings.TrimRight(now, "/"), strings.TrimRight(was, "/")
		}
		if now != was {
			drifted = append(drifted, name)
		}
	}
	return drifted, nil
}
