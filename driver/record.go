package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// The record this host keeps of the keys it has taken and of the ones it has carried to
// an ending.
//
// "JetStream guarantees at-least-once delivery, so every task is replayable: it carries
// the idempotency key run_id/step/attempt/shard, and the runner refuses to start a
// container for a key that has already completed." The container is no record of that. It
// is removed once its logs and exit code are collected, and adoption by label reaches
// only a container that is still there. A key can come back after it has gone, in a
// message a runner took and never acknowledged, or in the requeue of a task the heartbeat
// declared lost while its host was only cut off, and without a record the brick would run
// a second time on the host that already ran it.
//
// So the record is on disk, because the redelivery it guards against is what follows a
// restart, and under the work root, because that is the one directory a runner is given
// to write in. It sits beside the task directories and in none of them: a task's
// directory is removed with its container, and the record is what has to outlive it. It
// holds the key, the state and the moment, and never a payload. A working directory is
// "removed with the container, so no residue of one namespace survives into the next task
// on that host", and a record that kept the envelopes would be exactly that residue.

// KeysDir is where the record sits under the work root.
//
// The dot is what keeps it apart from the task directories beside it, whose first segment
// is a run identifier: agk.NewRunID mints a ULID, which never begins with one.
const KeysDir = ".keys"

// KeysKept is how long a key is remembered after it was last written.
//
// Seven days, which is the task stream's MaxAge: how long a message may wait for a runner
// before the stream discards it. A key comes back in a message, and the requeue that
// publishes one follows within minutes of the host last speaking of the key, so a key
// this host has not written for longer than the stream keeps a message is one whose
// redelivery has had every chance to arrive. The number is written here rather than read
// from the bus because this package reaches no bus, and it is the stream's number this
// one follows.
const KeysKept = 7 * 24 * time.Hour

// pruneEvery is how often what is older than KeysKept is taken away. Hourly, when an
// ending is written, rather than on every one: a busy host writes thousands a day, and a
// record a few minutes past its week guards against nothing and costs nothing either.
const pruneEvery = time.Hour

// ErrCompleted is a key this host has already carried to an ending.
//
// Any ending counts, whichever state it was. The key names one attempt of one shard of
// one step of one run, so a second container for it is never new work: it is the same
// attempt run twice, which is what the idempotency key exists to prevent. A retry is
// another attempt and so another key.
var ErrCompleted = errors.New("the runner refuses to start a container for a key that has already completed")

// keyEntry is what the record says about one key: taken, or how it ended.
type keyEntry struct {
	Key   agk.TaskID    `json:"idempotency_key"`
	State agk.TaskState `json:"state"`

	// ExitCode is written for the two states that carry one, as graph.Result carries it.
	ExitCode int `json:"exit_code,omitempty"`

	At time.Time `json:"at"`
}

// keys is the record of one work root.
//
// Every read and write goes through mu, and so does the prune. Hold reads a key and
// writes it in one step, and an ending written between the two would be overwritten by
// the hold and the key forgotten; the prune takes away directories a write may be about
// to use.
type keys struct {
	root string

	mu     sync.Mutex
	pruned time.Time
}

// path is where one key's entry is, spelled as a task's working directory is spelled, so
// that a person who found the one finds the other.
func (k *keys) path(id agk.TaskID) (string, error) {
	if k.root == "" {
		return "", errors.New("driver: no work root: the record of the keys this host has taken and completed is kept under one")
	}
	rel, err := taskPath(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(k.root, KeysDir, rel) + ".json", nil
}

// read answers with what the record says about one key, and false where it says nothing.
func (k *keys) read(id agk.TaskID) (keyEntry, bool, error) {
	path, err := k.path(id)
	if err != nil {
		return keyEntry{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return keyEntry{}, false, nil
	}
	if err != nil {
		return keyEntry{}, false, fmt.Errorf("driver: task %s: the record of its key could not be read, and a key this host cannot say it has not completed is not started: %w", id, err)
	}
	var e keyEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return keyEntry{}, false, fmt.Errorf("driver: task %s: the record of its key at %s does not read, and a key this host cannot say it has not completed is not started: %w", id, path, err)
	}
	if e.Key != id {
		return keyEntry{}, false, fmt.Errorf("driver: task %s: the record of its key at %s names %s, and a key this host cannot say it has not completed is not started", id, path, e.Key)
	}
	return e, true, nil
}

// write replaces one key's entry, whole or not at all.
//
// Whole, because the entry is read after the crash it is written for: a file renamed into
// place is either the old entry or the new one, where a file written in place can be half
// of the new one. Synced before the rename, because the host that restarts is the host
// the entry was written for, and an entry a power cut took back is a brick run twice.
func (k *keys) write(e keyEntry) error {
	path, err := k.path(e.Key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("driver: task %s: the record of its key could not be written: %w", e.Key, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, workdirMode); err != nil {
		return fmt.Errorf("driver: task %s: the record of its key could not be written: %w", e.Key, err)
	}
	f, err := os.CreateTemp(dir, ".writing-*")
	if err != nil {
		return fmt.Errorf("driver: task %s: the record of its key could not be written: %w", e.Key, err)
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("driver: task %s: the record of its key could not be written: %w", e.Key, err)
	}
	// The directory holds the name the rename wrote, and a directory that was never synced
	// can come back from a power cut without it. A failure here is not reported: the entry
	// is in place, and what is left is a guarantee this platform may not offer.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// prune takes away every entry last written before the record's retention, and the
// directories that leaves empty.
//
// The moment is the one written inside the entry, on the same clock the cutoff is read
// from. A file that does not read is judged by its own modification time instead, so that
// it is taken away in its turn rather than refusing its key for ever. The record's own
// directory stays, and so does anything under it that is not a file or a directory.
func (k *keys) prune(now time.Time) {
	top := filepath.Join(k.root, KeysDir)
	cutoff := now.Add(-KeysKept)
	var dirs []string
	filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != top {
				dirs = append(dirs, path)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if at, known := written(path, d); known && at.Before(cutoff) {
			os.Remove(path)
		}
		return nil
	})
	// Deepest first, and os.Remove rather than os.RemoveAll: a directory still holding a key
	// stays.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
	k.pruned = now
}

// written is when one entry was last written: the moment inside it, or the file's own
// where there is none to read. A file that cannot even be asked is left alone, being the
// one thing nothing here can judge.
func written(path string, d fs.DirEntry) (time.Time, bool) {
	if b, err := os.ReadFile(path); err == nil {
		var e keyEntry
		if json.Unmarshal(b, &e) == nil && !e.At.IsZero() {
			return e.At, true
		}
	}
	info, err := d.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// Hold records that this host has taken a task, which is what a runner does before it
// acknowledges the task message.
//
// A runner acknowledges on take, so from the acknowledgement on the bus never delivers
// that message again and the host is what answers for the key. Writing the key down first
// is what makes that true, and package bus says why the acknowledgement is not left to
// the end.
//
// A key this host has already carried to an ending is refused here with ErrCompleted,
// before anything is redeemed, pulled or created. The message is not put back for that:
// another runner of the pool has no record of the key and would start it, which is the
// second run the refusal exists to prevent.
func (d *Docker) Hold(id agk.TaskID) error {
	d.keys.mu.Lock()
	defer d.keys.mu.Unlock()
	e, found, err := d.keys.read(id)
	if err != nil {
		return err
	}
	if found && e.State.Terminal() {
		return completed(id, e)
	}
	return d.keys.write(keyEntry{Key: id, State: agk.TaskDispatched, At: d.now().UTC()})
}

// refuseCompleted is the refusal Run makes of a key this host has already carried to an
// ending, before it pulls, creates or redeems anything.
//
// What such a key left behind is taken away on the way out. An ending is written before
// the container and the working directory are removed, so that no moment passes in which
// the work is done and the record does not say so, and a runner that dies between the two
// leaves them behind with the key recorded. The refusal is then the only delivery that
// will ever reach them, and a working directory left in place is a directory of secret
// values nothing removes.
func (d *Docker) refuseCompleted(ctx context.Context, t graph.Task) error {
	d.keys.mu.Lock()
	e, found, err := d.keys.read(t.ID)
	d.keys.mu.Unlock()
	if err != nil {
		return err
	}
	if !found || !e.State.Terminal() {
		return nil
	}

	tidy, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalGrace)
	defer cancel()
	if left, err := d.containerOf(tidy, t.ID); err == nil && left != "" {
		d.cli.ContainerRemove(tidy, left, true)
	}
	if w, err := workdirFor(d.cfg.WorkRoot, t.ID, d.cfg.Policy.SecretsDir); err == nil {
		w.remove()
	}
	return completed(t.ID, e)
}

// ended writes down the ending Run is about to return, and returns it.
//
// It takes Run's own return values so that it sits on the return itself, which is what
// places it before the defers that remove the container and the working directory.
//
// An error is an ending too where it came after the container had run to its end, which
// afterExit marks. The brick ran, the removal on the way out takes what it left whether
// or not it was collected, and a key left unrecorded there is a key whose next delivery
// runs the brick from the beginning. It is written failed, which is how a caller records
// a Run that answered an error, and with no exit code, since none reached a Result. Only
// an error from before the container ran leaves the key as it was: nothing ran, so there
// is nothing a second delivery would run twice.
//
// An entry that could not be written does not turn the result into an error. The
// container ran and this is what became of it, and an error here would say that no
// outcome could be determined at all, which is the one thing that is not true. It is said
// instead, because what is lost is the refusal of a later delivery of this key on this
// host.
func (d *Docker) ended(r graph.Result, err error) (graph.Result, error) {
	e := keyEntry{Key: r.Task, State: r.State}
	if r.State == agk.TaskSucceeded || r.State == agk.TaskFailed {
		e.ExitCode = r.ExitCode
	}
	var late *afterExit
	switch {
	case errors.As(err, &late):
		e = keyEntry{Key: late.task, State: agk.TaskFailed}
		r, err = graph.Result{}, late.err
	case err != nil || !r.State.Terminal():
		return r, err
	}
	d.keys.mu.Lock()
	defer d.keys.mu.Unlock()

	now := d.now().UTC()
	e.At = now
	if werr := d.keys.write(e); werr != nil {
		d.say(werr.Error() + ": a later delivery of this key on this host will not be refused")
	}
	if now.Sub(d.keys.pruned) >= pruneEvery {
		d.keys.prune(now)
	}
	return r, err
}

// afterExit is an error met once the container of a task had run to its end: an output
// that is not an envelope, a store that refused the upload, a container an adoption found
// exited and could not collect. Run still answers with the error it carries, and ended
// takes the mark off before it does, so it never leaves this package.
type afterExit struct {
	task agk.TaskID
	err  error
}

func (e *afterExit) Error() string { return e.err.Error() }

func (e *afterExit) Unwrap() error { return e.err }

// exited marks err as met after the container of task had run to its end.
func exited(task agk.TaskID, err error) error {
	return &afterExit{task: task, err: err}
}

// completed is the refusal of one key, naming how and when it ended.
func completed(id agk.TaskID, e keyEntry) error {
	_, step, _, _, _ := agk.ParseTaskID(string(id))
	return fault(step, ErrCompleted, ChargePlatform, "task %s ended %s on this host at %s", id, e.State, e.At.UTC().Format(time.RFC3339))
}
