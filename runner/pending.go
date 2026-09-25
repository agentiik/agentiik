package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
)

// Keeping a result until the bus has taken it.
//
// "The task message it answers was acknowledged long before, once its grant was redeemed, so a
// result that could not be published is the runner's to publish again": every runner but this one
// is refused the task at its redemption, so nothing on the bus recovers it. A result that did not go
// out is also the one thing the controller is waiting on, and the heartbeat is what keeps it
// waiting rather than declaring the task lost, so the key stays in the heartbeat until the result is
// out. A result held in memory alone would be lost with the agent, and the task with it: the record
// of the key says the key ended, a restarted agent names no ended key, and three heartbeat intervals
// later the controller declares lost a task whose container succeeded, and spends a requeue on it.
//
// So a result is written under the work root before it is published, and taken away once the bus
// has it. Written first rather than on a failure, because the agent can stop between Run returning
// and the publication, and a result written only once a publication failed is not there after that.
// A restarted agent publishes again whatever it finds. The result stream drops a copy of one that
// had gone out, from the same runner, dispatch and ending, for two minutes, and the controller reads
// one that arrives later still as the ending it already has, which is no news.
//
// A result is composed once Run has returned, and the driver writes the key's ending before that,
// which takes away the record's entry saying the key was taken. An agent that stops between the two
// would leave the key named nowhere: not by the record's taken keys, not by a kept result, and the
// task would be declared lost although its container ran and ended, its requeue spending one of
// max_requeues and, where another runner of the pool takes it, running the brick a second time. So
// before a task is run, the dispatch it owes a result to is written down beside the results, by its
// task_id, its key and the runner, which is what a result carries and the record does not: a restart
// reads the ending of each dispatch still owed from the record and keeps its result from there. The
// entry is taken away once the result it owes is kept, and never before.

// ResultsDir is where results the bus has not yet taken are kept under the work root.
//
// The dot keeps it apart from the task directories beside it, for the reason driver.KeysDir has
// one, and it is outside every one of them, since a task's directory goes with its container and a
// result is written after that.
const ResultsDir = ".results"

const (
	resultsMode    fs.FileMode = 0o700
	resultFileMode fs.FileMode = 0o600
)

// owedExt is the extension of an entry saying a result is owed to a dispatch, beside where that
// result goes once it is kept.
const owedExt = ".owed"

// Owed is a dispatch a result is owed to: what a result names that the record of its key does not.
// No grant and nothing else of the message, which the work root never keeps.
type Owed struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Runner         string `json:"runner"`
}

// Publisher is what takes a result to the bus, which is bus.Bus.
type Publisher interface {
	Report(ctx context.Context, r bus.TaskResult) error
}

// Results is the results this runner has to publish, kept under its work root until the bus has
// taken each one.
type Results struct {
	dir string
	bus Publisher

	// mu guards kept and the files under dir. A publication is made outside it, so that a bus
	// that answers slowly holds up the result being published and not every other one.
	mu   sync.Mutex
	kept map[string]bus.TaskResult
	owed map[string]Owed
}

// OpenResults opens the results kept under a work root, which is every one a previous agent on
// this host kept and never saw published. They are published by the next Flush, and named by Keys
// until they are. The dispatches a previous agent owed a result to and never kept one for are
// opened too, for Carrier.Recover to keep a result for from the record.
//
// A file that does not read as a result, or reads as one no publication would ever take, is taken
// away and said in the error, since keeping it would name its key in the heartbeat for ever and
// publish nothing. A result of another runner is one of them: a host joined again under a new
// name keeps its work root, and the old name's subject is one only the old credential may publish
// on, and a result naming a runner other than its subject's one the controller refuses. A file that could not be read at all is left where it is and said, since it may
// be a result the next agent can read. The rest are kept, and the results are opened all the same.
func OpenResults(workRoot, runner string, p Publisher) (*Results, error) {
	if workRoot == "" {
		return nil, errors.New("runner: no work root: results the bus has not taken are kept under one")
	}
	if p == nil {
		return nil, errors.New("runner: results with no bus to publish them on")
	}
	dir := filepath.Join(workRoot, ResultsDir)
	if err := os.MkdirAll(dir, resultsMode); err != nil {
		return nil, fmt.Errorf("runner: the results directory %s could not be created: %w", dir, err)
	}
	if err := os.Chmod(dir, resultsMode); err != nil {
		return nil, fmt.Errorf("runner: the results directory %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("runner: the results kept under %s could not be listed: %w", dir, err)
	}
	r := &Results{dir: dir, bus: p, kept: map[string]bus.TaskResult{}, owed: map[string]Owed{}}
	var dropped, owed []error
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") {
			// A write the previous agent never finished, which never became a result.
			os.Remove(path)
			continue
		}
		if id, ok := strings.CutSuffix(name, owedExt); ok && e.Type().IsRegular() {
			owed = append(owed, r.openOwed(path, id, runner))
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			// Unread is not unreadable: the file may be the one copy of an ending the
			// controller is waiting on, and the next agent may read it.
			dropped = append(dropped, fmt.Errorf("runner: the result kept at %s could not be read, and is left where it is: %w", path, err))
			continue
		}
		res, err := readKept(b)
		switch {
		case err != nil:
		case res.TaskID != id:
			err = fmt.Errorf("it is kept as %s and is the result of %s", id, res.TaskID)
		case res.Runner != runner:
			err = fmt.Errorf("it is %s's, and this runner is %s", res.Runner, runner)
		}
		if err != nil {
			os.Remove(path)
			dropped = append(dropped, fmt.Errorf("runner: the result kept at %s was taken away, since no publication would ever take it: %w", path, err))
			continue
		}
		r.kept[id] = res
	}
	// A dispatch whose result is kept is owed nothing more, which is where an agent that stopped
	// between the two writes left it.
	for id := range r.owed {
		if _, ok := r.kept[id]; ok {
			delete(r.owed, id)
			os.Remove(filepath.Join(dir, id+owedExt))
		}
	}
	return r, errors.Join(append(dropped, owed...)...)
}

// openOwed reads one entry saying a result is owed, as OpenResults reads a kept result: one that
// could not be read is left where it is, and one that no result would ever answer is taken away.
func (r *Results) openOwed(path, id, runner string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("runner: the dispatch owed a result at %s could not be read, and is left where it is: %w", path, err)
	}
	var o Owed
	switch err = json.Unmarshal(b, &o); {
	case err != nil:
	case o.TaskID != id:
		err = fmt.Errorf("it is kept as %s and names %s", id, o.TaskID)
	case o.Runner != runner:
		err = fmt.Errorf("it is %s's, and this runner is %s", o.Runner, runner)
	case bus.TaskResult{TaskID: o.TaskID, IdempotencyKey: o.IdempotencyKey, Runner: o.Runner, State: agk.TaskFailed}.Check() != nil:
		err = errors.New("it names no dispatch a result could answer")
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("runner: the dispatch owed a result at %s was taken away, since no result would ever answer it: %w", path, err)
	}
	r.owed[id] = o
	return nil
}

// readKept reads one kept result, held to the rules a publication holds it to.
func readKept(b []byte) (bus.TaskResult, error) {
	var res bus.TaskResult
	if err := json.Unmarshal(b, &res); err != nil {
		return bus.TaskResult{}, err
	}
	if err := res.Check(); err != nil {
		return bus.TaskResult{}, err
	}
	return res, nil
}

// owe writes down that runner owes a result to the dispatch m, before its task is run, and answers
// whether this call wrote it: a second delivery of one message owes what the first already does, and
// is not the one to take the entry away. An entry that could not be written is still held, and the
// error says what a crash would then lose.
func (r *Results) owe(m bus.TaskMessage, runner string) (bool, error) {
	o := Owed{TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey, Runner: runner}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.owed[o.TaskID]; ok {
		return false, nil
	}
	if _, ok := r.kept[o.TaskID]; ok {
		return false, nil
	}
	r.owed[o.TaskID] = o
	b, err := json.Marshal(o)
	if err == nil {
		err = r.write(o.TaskID+owedExt, b)
	}
	if err != nil {
		return true, fmt.Errorf("runner: the result owed to %s could not be written down, and an agent that stops before its result is kept leaves its key to be declared lost: %w", o.TaskID, err)
	}
	return true, nil
}

// settle takes away the entry saying a result is owed to a dispatch, which is what a delivery does
// that has nothing of its own to report.
func (r *Results) settle(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unowe(taskID)
}

// unowe takes one owed entry away. Called with mu held.
func (r *Results) unowe(taskID string) {
	if _, ok := r.owed[taskID]; !ok {
		return
	}
	delete(r.owed, taskID)
	os.Remove(filepath.Join(r.dir, taskID+owedExt))
}

// Owed are the dispatches still owed a result, by task_id.
func (r *Results) Owed() []Owed {
	r.mu.Lock()
	defer r.mu.Unlock()
	owed := make([]Owed, 0, len(r.owed))
	for _, o := range r.owed {
		owed = append(owed, o)
	}
	slices.SortFunc(owed, func(a, b Owed) int { return strings.Compare(a.TaskID, b.TaskID) })
	return owed
}

// Keep keeps a result without publishing it, for the next Flush to publish and Keys to name until
// then, which is how a result recovered while the bus is not open yet is kept.
func (r *Results) Keep(res bus.TaskResult) error {
	if err := res.Check(); err != nil {
		return fmt.Errorf("runner: a result that is not one: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keep(res)
}

// Report keeps a result and publishes it, and takes it away again once the bus has it.
//
// It answers nil once the result is published. A result the bus did not take is kept, and Report
// answers the error that says so: the result goes out with a later Flush, and its key is named by
// Keys meanwhile. A result that no publication would ever take is refused before it is kept. One
// that could not be written down is still held and published, since what could not be written is
// only the copy that outlives the agent, and once published it is no loss.
func (r *Results) Report(ctx context.Context, res bus.TaskResult) error {
	if err := res.Check(); err != nil {
		return fmt.Errorf("runner: a result that is not one: %w", err)
	}
	r.mu.Lock()
	kerr := r.keep(res)
	r.mu.Unlock()

	if err := r.bus.Report(ctx, res); err != nil {
		if kerr != nil {
			return fmt.Errorf("runner: the result of %s was neither kept nor published, and is lost with this agent: %w", res.TaskID, errors.Join(kerr, err))
		}
		return fmt.Errorf("runner: the result of %s is kept under %s and published again later: %w", res.TaskID, r.dir, err)
	}
	r.forget(res)
	return nil
}

// Flush publishes again every result kept, and answers with what the bus did not take. It is what
// the agent calls once it has a bus, and again for as long as anything is kept.
func (r *Results) Flush(ctx context.Context) error {
	r.mu.Lock()
	pending := make([]bus.TaskResult, 0, len(r.kept))
	for _, res := range r.kept {
		pending = append(pending, res)
	}
	r.mu.Unlock()
	slices.SortFunc(pending, func(a, b bus.TaskResult) int { return strings.Compare(a.TaskID, b.TaskID) })

	var failed []error
	for _, res := range pending {
		if ctx.Err() != nil {
			failed = append(failed, ctx.Err())
			break
		}
		if err := r.bus.Report(ctx, res); err != nil {
			failed = append(failed, err)
			continue
		}
		r.forget(res)
	}
	if len(failed) > 0 {
		return fmt.Errorf("runner: results kept under %s are still to be published: %w", r.dir, errors.Join(failed...))
	}
	return nil
}

// Keys are the idempotency keys of every result kept, which the heartbeat goes on naming until each
// is published: the controller waits on those results, and a key it stopped hearing of would be
// declared lost with its result on its way.
func (r *Results) Keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keys []string
	for _, res := range r.kept {
		if !slices.Contains(keys, res.IdempotencyKey) {
			keys = append(keys, res.IdempotencyKey)
		}
	}
	slices.Sort(keys)
	return keys
}

// keep writes one result under dir, whole or not at all, and holds it in memory, which it does
// whatever became of the write: a result held is one Flush publishes and Keys names for as long as
// this agent lives. Called with mu held.
//
// The dispatch is owed nothing once its result is kept, and only then: a result that could not be
// written leaves the entry saying it is owed, from which a restart keeps it again.
func (r *Results) keep(res bus.TaskResult) error {
	r.kept[res.TaskID] = res
	b, err := json.Marshal(res)
	if err == nil {
		err = r.write(res.TaskID+".json", b)
	}
	if err != nil {
		return fmt.Errorf("runner: the result of %s could not be kept: %w", res.TaskID, err)
	}
	r.unowe(res.TaskID)
	return nil
}

// write replaces one file under dir, whole or not at all, and synced, since what is kept there is
// read after the crash it is written for. Called with mu held.
func (r *Results) write(name string, b []byte) error {
	f, err := os.CreateTemp(r.dir, ".writing-*")
	if err != nil {
		return err
	}
	err = f.Chmod(resultFileMode)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(r.dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	// The rename is what makes the file survive a restart, and a directory never synced can come
	// back from a power cut without it.
	return syncDir(r.dir)
}

// forget takes away a result the bus has taken, unless a later one of the same dispatch replaced it
// meanwhile, which is still to go out.
func (r *Results) forget(res bus.TaskResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if had, ok := r.kept[res.TaskID]; !ok || !reflect.DeepEqual(had, res) {
		return
	}
	delete(r.kept, res.TaskID)
	os.Remove(filepath.Join(r.dir, res.TaskID+".json"))
}
