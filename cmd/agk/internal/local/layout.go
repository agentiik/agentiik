package local

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/agentiik/agentiik/agk"
)

// DefaultDir is where a local run keeps what it produces, beside the workflow it ran.
//
// Inside the tree rather than in a cache directory somewhere, which is a wart said out
// loud in doc.go rather than hidden: the tree is bound read-only at /agk/repo, so a
// container can see what previous runs wrote. At three in the morning discoverability is
// worth more than tidiness, and a flag moves it for anyone who minds.
const DefaultDir = ".agk"

// The modes the layout is laid down with.
//
// Everything but the work root is a run's own record, readable by the person who started
// it. The work root is private, because a platform with no tmpfs has the driver write a
// task's secret values into that task's working directory, and the mode of the parents is
// the whole of what protects a value that is readable by design. That mode is only the
// whole of it because the work root sits outside the tree, which WorkRoot says why of: a
// bind mount is not obliged to carry a host mode into a container, and on Docker Desktop
// it does not.
const (
	recordMode = 0o755
	workMode   = 0o700
)

// workPrefix names the work root, which is the one directory of a local run that is not
// under the working directory. The digest of the working directory follows it, so that two
// processes over one working directory name one work root.
const workPrefix = "agk-work-"

// Layout is the working directory of a local run: one shared object store, one directory
// per run, and the work root the driver prepares a task's directory under.
//
// It is a value and not a handle. Every path is arithmetic on Root, so a caller can name
// a file without creating anything, which is what lets a report name a log that a failed
// task wrote and a test read a state file without a session in hand.
type Layout struct {
	Root string
}

// NewLayout names the working directory and creates what a run writes into.
//
// An empty root is DefaultDir, resolved against the process's working directory, which is
// what agk run --local passes when nobody said otherwise. The root is made absolute here
// because every path it produces reaches a daemon: a bind mount source is a path on the
// host and a relative one would be resolved against whatever directory the daemon happens
// to run in.
func NewLayout(root string) (Layout, error) {
	if root == "" {
		root = DefaultDir
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("local: the working directory %s could not be resolved: %w", root, err)
	}
	l := Layout{Root: abs}
	// The object store and the work root are made here and not per run, because both
	// are shared by every run this directory holds: two runs over the same inputs
	// write the same objects, which is the property that makes content addressing
	// show.
	if err := os.MkdirAll(l.Objects(), recordMode); err != nil {
		return Layout{}, fmt.Errorf("local: the object store %s could not be prepared: %w", l.Objects(), err)
	}
	if err := os.MkdirAll(l.WorkRoot(), workMode); err != nil {
		return Layout{}, fmt.Errorf("local: the work root %s could not be prepared: %w", l.WorkRoot(), err)
	}
	return l, nil
}

// Objects is the artifact.Dir root, shared by every run, so that two runs producing
// identical bytes store one copy.
func (l Layout) Objects() string { return filepath.Join(l.Root, "objects") }

// Bin is where the static helper is laid down, once, for every run to bind.
func (l Layout) Bin() string { return filepath.Join(l.Root, "bin") }

// WorkRoot is driver.Config.WorkRoot: the directory a task's working directory is created
// fresh under and removed from with its container.
//
// It is the one path of this layout that is not under Root, and that is containment rather
// than tidiness. Root defaults to sitting inside the tree the workflow sits in, and that
// tree is bound read-only at /agk/repo in every container of the run, so everything under
// Root is readable by every task while it runs. A task's working directory is where the
// driver writes that task's secret values on a platform with no tmpfs, announced when the
// session opens. Under Root, a step that declares no secret could therefore read a value
// another step was given, at /agk/repo/.agk/work/<run>/<step>/<attempt>/secrets/<name>,
// whatever the mode of the parents says: a bind mount is not obliged to carry a host mode
// into a container, and on Docker Desktop a container reads a 0700 directory of the host
// as its own. The work root is also the one thing here nobody is meant to read, being
// "created fresh, owned by an unprivileged account, and removed with the container", so
// moving it out costs none of the discoverability .agk is inside the tree for.
//
// It is still arithmetic on Root, so a Layout stays a value: the name carries a digest of
// Root rather than the path, because a path is not a directory name, and two processes
// over one working directory name one work root.
//
// It is not per run, and that is the driver's own doing rather than a choice taken here:
// the driver names a task's directory run/step/attempt beneath its work root, so the run
// is already the first segment of every path below this one. A work root per run would
// spell the run twice.
func (l Layout) WorkRoot() string {
	sum := sha256.Sum256([]byte(l.Root))
	return filepath.Join(workParent(), workPrefix+hex.EncodeToString(sum[:8]))
}

// workParent is the directory the work root sits in: this user's own cache directory, and the
// temporary directory on a machine with no answer for one.
//
// This user's own and not the shared temporary directory, which is the other obvious place for
// something nobody is meant to read. The name below it is derived from the working directory,
// so it is a path anybody on the host can work out, and /tmp is writable by every account on
// it: a symlink planted at that name before the first run would have the driver prepare a
// task's inputs, parameters and secret values wherever the planter chose. os.UserCacheDir is
// per user on both platforms this runs on, which is what closes that.
//
// A task's directory is bound into a container, so it has to be a path the daemon can bind.
// On Linux that is any path. On macOS the cache directory is under /Users, which is in the
// default file sharing of Docker Desktop, as is the temporary directory it falls back to.
func workParent() string {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "agentiik")
	}
	return os.TempDir()
}

// Work is where one run's task directories land, each removed with the container that read
// it.
func (l Layout) Work(run agk.RunID) string { return filepath.Join(l.WorkRoot(), string(run)) }

// pruneWork takes away what an ended run left under the work root.
//
// The driver removes a task's directory with its container, which is run/step/attempt and not
// the run and the step above it, so what an ended run leaves behind is a skeleton of empty
// directories. That skeleton was somebody's to find while the work root sat under .agk beside
// everything else they read; outside the tree it is in a directory nobody thinks to look in,
// so it is this package's to clear.
//
// os.Remove and never os.RemoveAll, deepest first. A directory with anything in it stays: a
// task whose directory the driver could not remove is a thing worth finding, a run of the same
// working directory that is still going keeps its own, and the difference between the two is
// not this function's to guess at. An error is not reported for the same reason the driver
// does not report one: a directory that is already gone is the outcome asked for.
//
// The work root itself is the one directory this never removes, and that is not tidiness
// either. A directory removed and created again at one path is a path Docker Desktop's file
// sharing no longer resolves inside the VM: with the work root taken away at the end of a run,
// the next run over the same working directory was refused every container it created, "invalid
// mount config for type bind: bind source path does not exist", and every step of it failed
// 125. Emptying it costs nothing and removing it costs every run after the first.
func (l Layout) pruneWork() {
	root := l.WorkRoot()
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}

// Runs is the directory every run's record sits under.
func (l Layout) Runs() string { return filepath.Join(l.Root, "runs") }

// Dir is one run's own directory.
func (l Layout) Dir(run agk.RunID) string { return filepath.Join(l.Runs(), string(run)) }

// RunFile is the local run record: the agk.Run the containers were told about, with
// local: true beside it.
func (l Layout) RunFile(run agk.RunID) string { return filepath.Join(l.Dir(run), "run.json") }

// State is graph.State, written after every Record so that a run that dies at three in
// the morning leaves the thing a second process would resume from.
func (l Layout) State(run agk.RunID) string { return filepath.Join(l.Dir(run), "state.json") }

// Outputs is where one envelope per declared workflow output is written.
func (l Layout) Outputs(run agk.RunID) string { return filepath.Join(l.Dir(run), "outputs") }

// Logs is where the log of every task of one run sits.
func (l Layout) Logs(run agk.RunID) string { return filepath.Join(l.Dir(run), "logs") }

// Log is one task's log file, derived from the task identifier and never minted, so that
// the report of a failure and the file on disk name the same path.
//
// The shard is spelled index-of on one segment, which is the spelling the driver already
// gives a task's working directory, so that a person reading one and then the other does
// not have to hold two conventions.
func (l Layout) Log(task agk.TaskID) (string, error) {
	run, step, attempt, shard, err := agk.ParseTaskID(string(task))
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	name := strconv.Itoa(attempt)
	if !shard.IsZero() {
		name += "-" + strconv.Itoa(shard.Index) + "-" + strconv.Itoa(shard.Of)
	}
	return filepath.Join(l.Logs(run), string(step), name+".log"), nil
}

// prepare creates the directories one run writes into. The logs are not created here,
// because a log is created with the task that writes it and a run with no failures has
// directories for every step whether or not anything opened one.
func (l Layout) prepare(run agk.RunID) error {
	if err := run.Validate(); err != nil {
		return fmt.Errorf("local: %w", err)
	}
	for _, dir := range []string{l.Dir(run), l.Outputs(run)} {
		if err := os.MkdirAll(dir, recordMode); err != nil {
			return fmt.Errorf("local: the run directory %s could not be prepared: %w", dir, err)
		}
	}
	return nil
}
