package driver

import (
	"context"
	"io"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// Config is everything a container needs that a Task deliberately does not carry.
//
// That split is the reason graph.Driver needs no widening. A Task is the whole of the
// per-task argument and nothing in it is looked up; everything that is about this
// machine rather than about this task, the socket, the store, the repository tree, the
// secret source, the log sink, the policy and the work root, arrives once, here. A wider
// Run would be the evaluator learning about grants, trees and sockets, which is the
// thing graph/driver.go exists to prevent.
//
// Three of those are about the task after all where a server runs it: Store, Secrets and
// Repo, which a runner has from the redemption of one task's grant. A runner gives them
// per task, as Sources on the context of that task's Run through WithSources, and each
// one given there answers in place of the hook of the same name here. agk run --local
// gives none and is answered from these. Every other field is the machine's, and is
// never overridden per task.
type Config struct {
	// Socket is the daemon to talk to, and empty means wherever one is:
	// DOCKER_HOST, then the per-user path Docker Desktop uses, then the system
	// path.
	Socket string

	// Store opens the artifact store of one namespace. It is per namespace because
	// a Store is opened for one, and because an artifact "never crosses a namespace
	// boundary": a driver holding one store for every tenant would be the place that
	// boundary stopped being true. A runner gives the store of each task in
	// Sources.Store instead, opened on that task's presigned URLs and upload policy.
	Store func(namespace string) (*artifact.Store, error)

	// Repo answers with the path of the workflow repository tree at one commit,
	// which is what gets bound read-only at /agk/repo. A server runner lays that
	// directory out itself from the files its grant redemption names, one
	// content-addressed object per file, holds no checkout and no credential for
	// the repository, and names the directory in Sources.Repo; agk run --local has
	// the working tree.
	Repo func(ctx context.Context, namespace, workflow, commit string) (string, error)

	// Runs answers with the run a task belongs to, which is what /agk/run.json
	// carries. It is asked rather than carried on the Task because a run is one
	// value shared by every task of it.
	Runs func(ctx context.Context, run agk.RunID) (agk.Run, error)

	// Secrets is where a value comes from when the container is prepared. A server
	// runner answers from its redemption of the per-task grant the controller issued,
	// which it made before the pull, since it redeems before it acknowledges the task
	// message, and gives it in Sources.Secrets, because two tasks it holds at once may
	// name one secret and be owed two values; agk run --local reads the command line.
	Secrets Secrets

	// Logs is where a task's log is written. A nil Logs is a runner that keeps
	// none, which is what agk brick test is.
	Logs Logs

	// Observer is what graph.Result has nowhere to carry: the digests the ports were
	// published under, the log reference, the artifacts, the usage block, and the
	// dispatched, running and publishing transitions a heartbeat needs while a task is
	// still in flight.
	Observer Observer

	// Policy is the runner's own configuration, and its zero value is every floor in
	// place. New never reads a file: reading /etc/agentiik/runner.toml belongs where
	// a runner is configured, and doing it here would make this package refuse to be
	// a library on a machine with no such file. LoadPolicy is the reader.
	Policy Policy

	// WorkRoot is where a task's working directory is created, fresh, and removed
	// with its container.
	WorkRoot string

	// Limits are the size rules the collection is held to. Its zero value is agk's
	// own defaults.
	Limits agk.Limits

	// Now is the clock, so that a test can hold one still. The two moments a result
	// reports are not taken from it: they come from the daemon's own State.StartedAt
	// and State.FinishedAt, so that the same task read twice reports the same
	// moments.
	Now func() time.Time

	// Announce is where the driver says, once, what this machine gives up. It is a
	// function rather than a logger because what it has to say is two sentences, and
	// where they go is the caller's: a terminal for agk run --local, the runner's
	// own log for a server.
	Announce func(string)

	// Host answers what the floors ask of this machine rather than of the daemon: the
	// capabilities this process holds, which a remapped daemon needs three of, and
	// what the secrets directory is mounted as. Nil is the kernel's own answers, and
	// only a test gives another.
	Host Host
}

// Logs is where a task's log is written, opened by whoever knows where it lands.
//
// It is one method for the same reason Secrets is: the driver asks one question, where
// does this task's log go, and the answer is a store key on a server and a file on a
// laptop. The agk.URI of the result is filled in by the caller that opened the sink,
// because the sink knows where it went and this package does not.
type Logs interface {
	OpenLog(ctx context.Context, task agk.TaskID) (io.WriteCloser, error)
}

// now is the clock, or the real one.
func (d *Docker) now() time.Time {
	if d.cfg.Now != nil {
		return d.cfg.Now()
	}
	return time.Now()
}

// say announces one sentence, where anybody is listening.
func (d *Docker) say(s string) {
	if d.cfg.Announce != nil {
		d.cfg.Announce(s)
	}
}

// limits are the size rules in force, or agk's own.
func (d *Docker) limits() agk.Limits {
	if d.cfg.Limits == (agk.Limits{}) {
		return agk.DefaultLimits()
	}
	return d.cfg.Limits
}
