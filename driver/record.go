package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// directory is removed with its container, and the record is what has to outlive it.
//
// Refusing the key is not the whole answer, because the requeue is a task the run is
// waiting on. The heartbeat declares a task lost when its host stops reporting, and a host
// only cut off may well have run it to its end and reported that ending into the same
// silence. The requeue that follows is likeliest to come back to that very host, and
// certain to where it is its pool's only runner, and refused there and nothing more, it
// would leave the run waiting on an ending nobody gives. So the record keeps, beside the
// key, the state and the moment, what the ending left, by reference: each port's envelope
// by digest and count, each artifact by digest and size, and where the log went and how
// long it is. The host answers the requeue with that, and the brick never runs twice.
//
// By reference and never a payload. The envelopes and the artifacts are in the object
// store before the ending is written down, since conclude writes them there first and
// names each by the digest the store answered, so a reference is all a second report
// needs. And a working directory is "removed with the container, so no residue of one
// namespace survives into the next task on that host": a record that kept the envelopes
// would be exactly that residue.

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

// Completed is the refusal of a key this host has already carried to an ending, holding
// what the record says of that ending.
//
// errors.Is with ErrCompleted is what says a key was refused for having ended, and
// errors.As with a *Completed is what hands over how it ended. The runner that took the
// key reports that ending under the task_id of the message it took, which is how the
// requeue of a task lost while its host was only cut off is answered without the brick
// running again.
//
// The Ending is the whole of it, so a Completed written as a literal, which is how a
// runner's tests fake this package, is the same refusal as one Hold answered.
type Completed struct {
	Ending Ending
}

// Error is the refusal, naming the key, how and when it ended, and the rule.
func (c *Completed) Error() string { return c.fault().Error() }

// Unwrap gives up the fault, through which errors.Is reaches ErrCompleted and Charged reads
// whose it is.
func (c *Completed) Unwrap() error { return c.fault() }

// fault is the refusal written as every other refusal of this package is, composed from the
// ending each time rather than kept beside it, since a field only this package could fill
// would leave every Completed made anywhere else pointing at nothing.
func (c *Completed) fault() *Fault {
	_, step, _, _, _ := agk.ParseTaskID(string(c.Ending.Key))
	return fault(step, ErrCompleted, ChargePlatform, "task %s ended %s on this host at %s", c.Ending.Key, c.Ending.State, c.Ending.At.UTC().Format(time.RFC3339))
}

// Ending is what the record says about one key: how it ended, when, and what it left, by
// reference.
//
// The references are spelled as a task result spells them, so that a runner answering a
// requeue from the record forwards them rather than composes them. A key taken and not
// yet ended is written in the same shape, dispatched, and says nothing else.
type Ending struct {
	Key   agk.TaskID    `json:"idempotency_key"`
	State agk.TaskState `json:"state"`

	// ExitCode is there wherever a container exited and its code was read, as a result
	// carries one: a success, a failure, and a container stopped at its deadline or
	// cancelled, with the code the stop left. Outputs the collection refused are written
	// with ExitContractBroken, since the container ran. A failure met after the exit that
	// is the platform's, an upload that did not go through, is written with none.
	ExitCode *int `json:"exit_code,omitempty"`

	// StartedAt and FinishedAt are the daemon's own, as the Result carried them, or where the
	// daemon could not say after the exit, the dispatch and the moment the exit was read.
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`

	// Outputs names the envelope of every port the Result carried, as the observer was
	// told them, and of a success it is never absent, the empty list included: that is how
	// a result tells a step that published nothing from a runner that said nothing.
	Outputs []EndedPort `json:"outputs,omitzero"`

	// Artifacts are the objects the task put in the store, as the observer was told them.
	Artifacts []EndedArtifact `json:"artifacts,omitzero"`

	// Log is where the task's log went, for a runner that keeps one.
	Log *EndedLog `json:"log,omitempty"`

	// At is when the entry was written, on this host's clock, which is what the record is
	// pruned by.
	At time.Time `json:"at"`
}

// EndedPort is the envelope one port published, named rather than kept.
type EndedPort struct {
	Port agk.Port `json:"port"`

	// Digest is sha256: and sixty-four lowercase hexadecimal characters, as a result writes
	// an envelope's, and it is the digest the store answered when the envelope was written
	// to it, before the ending was.
	Digest string `json:"digest"`
	Items  int    `json:"items"`
}

// EndedArtifact is one object the task put in the store, by digest and size.
type EndedArtifact struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// EndedLog is where a task's log is addressed from, how many lines it holds and whether a
// cap cut it short.
type EndedLog struct {
	URI       agk.LogURI `json:"uri"`
	Lines     int        `json:"lines"`
	Truncated bool       `json:"truncated"`
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

// takenExt is the extension of an entry saying a key was taken and nothing more, beside
// where its ending goes.
//
// Apart from the ending, so that listing what this host took and never ended costs a
// reading of the directories and of those entries alone, rather than of every ending kept
// for a week, which on a busy host is tens of thousands of files read while a restarted
// runner's first heartbeat waits.
const takenExt = ".taken"

// takenPath is where the entry saying one key was taken goes.
func (k *keys) takenPath(id agk.TaskID) (string, error) {
	path, err := k.path(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".json") + takenExt, nil
}

// read answers with what the record says about one key, and false where it says nothing:
// its ending, or where it has none, that it was taken.
//
// An ending that does not read refuses the key, since it may be the ending of a key that
// ran. An entry saying only that the key was taken is passed over where it does not read,
// since it refuses nothing either way.
func (k *keys) read(id agk.TaskID) (Ending, bool, error) {
	path, err := k.path(id)
	if err != nil {
		return Ending{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return k.taken(id)
	}
	if err != nil {
		return Ending{}, false, fmt.Errorf("driver: task %s: the record of its key could not be read, and a key this host cannot say it has not completed is not started: %w", id, err)
	}
	var e Ending
	if err := json.Unmarshal(b, &e); err != nil {
		return Ending{}, false, fmt.Errorf("driver: task %s: the record of its key at %s does not read, and a key this host cannot say it has not completed is not started: %w", id, path, err)
	}
	if e.Key != id {
		return Ending{}, false, fmt.Errorf("driver: task %s: the record of its key at %s names %s, and a key this host cannot say it has not completed is not started", id, path, e.Key)
	}
	return e, true, nil
}

// taken answers with the entry saying one key was taken, where there is one that reads.
func (k *keys) taken(id agk.TaskID) (Ending, bool, error) {
	path, err := k.takenPath(id)
	if err != nil {
		return Ending{}, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Ending{}, false, nil
	}
	var e Ending
	if json.Unmarshal(b, &e) != nil || e.Key != id || e.State.Terminal() {
		return Ending{}, false, nil
	}
	return e, true, nil
}

// write replaces one key's entry, whole or not at all: an ending where the key ended, which
// then takes away the entry saying it was taken, and that entry otherwise.
//
// Whole, because the entry is read after the crash it is written for: a file renamed into
// place is either the old entry or the new one, where a file written in place can be half
// of the new one. Synced before the rename, because the host that restarts is the host
// the entry was written for, and an entry a power cut took back is a brick run twice.
func (k *keys) write(e Ending) error {
	path, err := k.path(e.Key)
	if !e.State.Terminal() {
		path, err = k.takenPath(e.Key)
	}
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
	// Once the ending is in place and not before, so that no moment passes in which the
	// record says nothing of a key that was taken. A failure leaves an entry Dispatched
	// passes over, since it finds the ending beside it.
	if e.State.Terminal() {
		if taken, err := k.takenPath(e.Key); err == nil {
			os.Remove(taken)
		}
	}
	return nil
}

// forget takes away the entry saying one key was taken. An ending is kept, being what
// refuses the key. A failure is not said: an entry left behind refuses nothing, and is only
// listed by Dispatched until the prune takes it.
func (k *keys) forget(id agk.TaskID) {
	if path, err := k.takenPath(id); err == nil {
		os.Remove(path)
	}
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
		var e Ending
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

// Hold records that this host has taken a task, which is the first thing a runner does with
// a message it took: before it redeems the grant, before it acknowledges the message, and
// before it pulls or creates anything. Package bus says why the redemption comes before the
// acknowledgement. This comes before both because of what it refuses.
//
// A key this host has already carried to an ending is refused here with a *Completed, which
// errors.Is reads as ErrCompleted, before anything is redeemed, pulled or created. Not
// redeemed, because the redemption would bind this runner to the dispatch and read its
// secret values for a brick that is not going to run. The message is not put back for that
// either: another runner of the pool has no record of the key and would start it, which is
// the second run the refusal exists to prevent. It is answered instead. The refusal carries
// the ending the record holds, and the runner reports that ending under the message's own
// task_id and then acknowledges the message: a key comes back to the host that ended it as
// the requeue of a task declared lost, and the run is waiting on the requeue's answer.
//
// A key this host still has in flight, a Run carrying it or another delivery holding it, is
// refused as well, with ErrTaskInFlight, and before anything is redeemed for the same reason.
// It comes back while the host still has it as the requeue of a task the heartbeat declared
// lost while its host was only cut off and its container ran on. Redeemed, that requeue would
// be bound to this runner, and Run would refuse it as a second delivery of the key, so the one
// ending the container produces would be reported under the dispatch that was lost, which the
// controller reads as no news, and the requeue, bound and never answered, would be declared
// lost in its turn once the key was let go of: one cut costing the key two of max_requeues, and
// a step whose brick succeeded failing on it where the bound had no second to spend. Refused
// here, the requeue binds nobody, and nothing sweeps a dispatch nobody redeemed. The runner
// says nothing and the message comes round once AckWait has passed, by when the key has ended
// and Hold answers with its *Completed, or is still running and refused again. Another runner
// of the pool may take it meanwhile, and redeem and run it, which is what a requeue is for.
//
// A key written down is also held in memory, as Run holds the task it runs, so that a stop
// is kept from here on. The redemption that follows binds the task to this runner, and from
// then a cancel names it and the controller sends its one stop, which can arrive while the
// runner is still acknowledging and before Run has begun: answered nil and forgotten, it
// would leave Run to start a brick for a run already called off. Recorded here, it is what
// Run finds, and the container is never started. A runner that does not go on to Run the
// task, its redemption refused or the message put back, lets go of it with Release, and a
// delivery Hold refused holds nothing and lets go of nothing. A redemption that got no
// answer is neither: it may have bound the task, so the runner keeps the key, which its
// heartbeat goes on naming, and redeems again, as package bus says.
func (d *Docker) Hold(id agk.TaskID) error {
	d.keys.mu.Lock()
	defer d.keys.mu.Unlock()
	e, found, err := d.keys.read(id)
	if err != nil {
		return err
	}
	// The ending first: a key whose ending is written and whose Run is still removing
	// what it left is answered from the record, and waiting for the removal would gain
	// nothing.
	if found && e.State.Terminal() {
		return &Completed{Ending: e}
	}
	if !d.hold(id) {
		_, step, _, _, _ := agk.ParseTaskID(string(id))
		return fault(step, ErrTaskInFlight, ChargePlatform, "task %s", id)
	}
	if err := d.keys.write(Ending{Key: id, State: agk.TaskDispatched, At: d.now().UTC()}); err != nil {
		d.release(id)
		return err
	}
	return nil
}

// Dispatched are the keys the record says this host took and never carried to an ending,
// the most recently taken first.
//
// A runner calls it as it starts, before it takes anything, and what it answers then is
// what an earlier process on this host held when it stopped: written down by Hold and never
// ended, since Release forgets a key let go of and an ending replaces the entry. Some of
// those were redeemed and are bound to this runner, their containers perhaps still running
// on the daemon, and the controller counts a bound task as held only while its runner goes
// on naming it. So the heartbeat names these from the first, for as long as the message of
// one may still come round to be taken again here.
//
// An entry that does not read is passed over rather than refusing the rest, since this is a
// list of what to name and one key left out of it is one task the sweep may declare lost,
// which is what happens to all of them where nothing is listed.
func (d *Docker) Dispatched() ([]agk.TaskID, error) {
	d.keys.mu.Lock()
	defer d.keys.mu.Unlock()
	if d.keys.root == "" {
		return nil, errors.New("driver: no work root: the record of the keys this host has taken is kept under one")
	}
	type taken struct {
		key agk.TaskID
		at  time.Time
	}
	var found []taken
	top := filepath.Join(d.keys.root, KeysDir)
	err := filepath.WalkDir(top, func(path string, e fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist) && path == top:
			return fs.SkipAll
		case err != nil:
			return err
		case !e.Type().IsRegular() || filepath.Ext(path) != takenExt:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var entry Ending
		if json.Unmarshal(b, &entry) != nil || entry.State.Terminal() || entry.Key.Validate() != nil {
			return nil
		}
		// Only an entry kept where its key's entry is kept, so that a file copied or
		// renamed under the record does not name a key it is not the entry of.
		if want, err := d.keys.takenPath(entry.Key); err != nil || want != path {
			return nil
		}
		// An ending written after it, whose writer stopped before taking this away.
		if ended, found, _ := d.keys.read(entry.Key); found && ended.State.Terminal() {
			return nil
		}
		found = append(found, taken{entry.Key, entry.At})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("driver: the record of the keys this host has taken could not be read: %w", err)
	}
	slices.SortFunc(found, func(a, b taken) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return strings.Compare(string(a.key), string(b.key))
	})
	keys := make([]agk.TaskID, len(found))
	for i, f := range found {
		keys[i] = f.key
	}
	return keys, nil
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
		d.tidy(t, w)
	}
	return &Completed{Ending: e}
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
// a Run that answered an error. Where the exit was read before the error, which is a
// collection that failed, it is written with the exit code and the span refused reports,
// since a result that says a container ran carries both; otherwise with neither. Only an
// error from before the container ran leaves the key as it was: nothing ran, so there is
// nothing a second delivery would run twice.
//
// An entry that could not be written does not turn the result into an error. The
// container ran and this is what became of it, and an error here would say that no
// outcome could be determined at all, which is the one thing that is not true. It is said
// instead, because what is lost is the refusal of a later delivery of this key on this
// host.
func (d *Docker) ended(r graph.Result, err error) (graph.Result, error) {
	var e Ending
	var late *afterExit
	switch {
	case errors.As(err, &late) && late.ran != nil:
		e = d.ending(*late.ran)
		r, err = graph.Result{}, late.err
	case errors.As(err, &late):
		e = Ending{Key: late.task, State: agk.TaskFailed}
		r, err = graph.Result{}, late.err
	case err != nil || !r.State.Terminal():
		return r, err
	default:
		e = d.ending(r)
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

// ending is what the record keeps of a Result: how it ended and when, and by reference
// what it left, which is what the observer was told of it beside the Result: the digests
// its ports were published under, its artifacts and its log.
//
// The ports are named as the collection published them, by the digest the store answered
// for each write, and never worked out again here. A report made from the record is read
// back by that digest, so the one worth recording is the one the store holds the envelope
// under, and only the write can say that.
//
// The log is addressed as agk.NewLogURI addresses a task's log, where this runner keeps
// logs at all. The sink knows where its bytes went; the address a result carries is the
// task's, whatever the sink.
func (d *Docker) ending(r graph.Result) Ending {
	e := Ending{Key: r.Task, State: r.State, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	if r.State == agk.TaskSucceeded || r.State == agk.TaskFailed {
		code := r.ExitCode
		e.ExitCode = &code
	}

	h := d.lookup(r.Task)
	if h == nil {
		return e
	}
	told := h.ended()
	// A container stopped at its deadline or cancelled exited too, and the code the stop
	// left is on what the observer was told, since a Result reads one for succeeded and
	// failed alone. It is kept where the container is known to have started, as a result
	// carries a code only beside the span.
	if e.ExitCode == nil && told.ExitCode != nil && !e.StartedAt.IsZero() {
		code := *told.ExitCode
		e.ExitCode = &code
	}
	e.Outputs = told.Outputs
	for _, f := range told.Artifacts {
		e.Artifacts = append(e.Artifacts, EndedArtifact{SHA256: f.SHA256, Bytes: f.Size})
	}
	if d.cfg.Logs != nil {
		if uri, err := agk.NewLogURI(r.Task); err == nil {
			e.Log = &EndedLog{URI: uri, Lines: told.Log.Lines, Truncated: told.Log.Truncated}
		}
	}
	return e
}

// afterExit is an error met once the container of a task had run to its end: an output
// that is not an envelope, a store that refused the upload, a container an adoption found
// exited and could not collect. Run still answers with the error it carries, and ended
// takes the mark off before it does, so it never leaves this package.
type afterExit struct {
	task agk.TaskID
	err  error

	// ran is the ending the error came after, where the exit had been read: failed, with
	// the code and the span the key is written down with.
	ran *graph.Result
}

// Error is the error it carries, word for word.
func (e *afterExit) Error() string { return e.err.Error() }

// Unwrap gives that error up, so that errors.Is and Charged read through the mark.
func (e *afterExit) Unwrap() error { return e.err }

// exited marks err as met after the container of task had run to its end.
func exited(task agk.TaskID, err error) error {
	return &afterExit{task: task, err: err}
}

// exitedWith marks err as met after the container had run to the ending r, which is what
// the key is written down with.
func exitedWith(r graph.Result, err error) error {
	return &afterExit{task: r.Task, err: err, ran: &r}
}
