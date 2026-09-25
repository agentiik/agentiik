package local

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/stoptest"
)

// playing is a driver that plays one history: every task exits with the code the history gives
// it as soon as it starts, except the late one, which exits 0 once its stop has been sent, as a
// container that finished in the moment before the stop reached it.
type playing struct {
	h stoptest.History

	// missed is how many stops of each task the driver answers and does not act on before it
	// takes one, as a driver asked to stop a task whose container it has not reached yet, and
	// refused how many it refuses outright.
	missed, refused int

	// observe, where it is set, is how the late task's container says it is running once
	// its stop has been sent, before it exits.
	observe func(agk.TaskID)

	mu      sync.Mutex
	stopped map[agk.TaskID]chan struct{}
	asked   map[agk.TaskID]int
	ran     map[stoptest.Task]bool
	waited  bool
}

func newPlaying(h stoptest.History, missed, refused int) *playing {
	return &playing{h: h, missed: missed, refused: refused, stopped: map[agk.TaskID]chan struct{}{}, asked: map[agk.TaskID]int{}, ran: map[stoptest.Task]bool{}}
}

func (p *playing) stop(id agk.TaskID) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped[id] == nil {
		p.stopped[id] = make(chan struct{})
	}
	return p.stopped[id]
}

func (p *playing) Run(ctx context.Context, t graph.Task) (graph.Result, error) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := stoptest.Task{Step: t.Step, Shard: t.Shard.Index}
	p.mu.Lock()
	p.ran[task] = true
	p.mu.Unlock()
	if task == p.h.Late {
		// A driver that is never asked to stop it reports it all the same, so that the
		// run ends and says what it made of it rather than hanging.
		select {
		case <-p.stop(t.ID):
		case <-time.After(10 * time.Second):
			p.mu.Lock()
			p.waited = true
			p.mu.Unlock()
		}
		if p.observe != nil {
			p.observe(t.ID)
		}
	}
	if code := p.h.Exits[task]; code != 0 {
		return graph.Result{Task: t.ID, State: agk.TaskFailed, ExitCode: code, StartedAt: started, FinishedAt: started}, nil
	}
	r := published(t)
	r.StartedAt, r.FinishedAt = started, started
	return r, nil
}

func (p *playing) Stop(ctx context.Context, s graph.Stop) error {
	ch := p.stop(s.Task)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked[s.Task]++
	switch {
	case p.asked[s.Task] <= p.missed:
		return nil
	case p.asked[s.Task] <= p.missed+p.refused:
		return errors.New("the daemon did not answer")
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
	return nil
}

// A task stopped while the run goes on ends as the stop goes out, and the report of a container
// that exited 0 just before the stop reached it adds its code and nothing else: the shard reads
// cancelled, as it does on a server playing the same history.
func TestALocalRunEndsAStoppedShardAsAServerDoes(t *testing.T) {
	for _, h := range stoptest.Histories {
		t.Run(h.Name, func(t *testing.T) {
			p := newPlaying(h, 0, 0)
			s := session(t, p)
			out, err := s.Run(t.Context(), Request{Graph: built(t, h.Workflow), Tree: t.TempDir(), Inputs: h.Inputs})
			if err != nil {
				t.Fatal(err)
			}
			for task, want := range h.Want {
				var sh graph.ShardState
				found := false
				for _, s := range out.State.Steps[task.Step].Shards {
					if s.Shard.Index == task.Shard {
						sh, found = s, true
					}
				}
				if !found {
					t.Errorf("%s shard %d never ran", task.Step, task.Shard)
					continue
				}
				// Every container here that ran started, and the start is the driver's, which
				// reaches a stopped shard only through the report the evaluator takes its code
				// from. One that never started has neither a start nor a code, and the driver
				// was never asked to run it.
				ran := !want.NeverStarted
				if sh.Task != want.State || sh.ExitCode != want.ExitCode || sh.NoExitCode == ran || sh.StartedAt.IsZero() == ran || p.ran[task] != ran {
					t.Errorf("%s shard %d ended %s, exit %d, no exit code %t, started at %s, run by the driver %t, want %s, exit %d, run %t", task.Step, task.Shard, sh.Task, sh.ExitCode, sh.NoExitCode, sh.StartedAt, p.ran[task], want.State, want.ExitCode, ran)
				}
			}
			for step, want := range h.Steps {
				if got := out.State.Steps[step].Verdict; got != want {
					t.Errorf("%s is %s, want %s", step, got, want)
				}
			}
			if out.Run.State != h.Run {
				t.Errorf("the run is %s, want %s", out.Run.State, h.Run)
			}
		})
	}
}

// The evaluator names a stop sent while the run goes on once, so the loop sends it again on every
// pass until its task comes back: the driver may have answered nil for a container it had not
// reached yet, or refused the stop. Here the pass that stops archive starts pick, whose ending is
// the next pass.
func TestAStopIsSentAgainUntilItsTaskComesBack(t *testing.T) {
	h := stoptest.Histories[slices.IndexFunc(stoptest.Histories, func(h stoptest.History) bool { return h.Name == "merge first" })]
	for name, p := range map[string]*playing{
		"missed":  newPlaying(h, 1, 0),
		"refused": newPlaying(h, 0, 1),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := session(t, p).Run(t.Context(), Request{Graph: built(t, h.Workflow), Tree: t.TempDir(), Inputs: h.Inputs})
			if err != nil {
				t.Fatal(err)
			}
			// The late shard of archive, the one that was started: the other was never
			// handed out, and its one stop is not sent again.
			var archive agk.TaskID
			for id := range p.asked {
				if _, step, _, shard, err := agk.ParseTaskID(string(id)); err == nil && (stoptest.Task{Step: step, Shard: shard.Index}) == h.Late {
					archive = id
				}
			}
			if p.waited || p.asked[archive] < 2 {
				t.Errorf("the driver was asked to stop %s %d times, and it came back on its own %t", archive, p.asked[archive], p.waited)
			}
			for id, n := range p.asked {
				if id != archive && n != 1 {
					t.Errorf("the driver was asked to stop %s %d times, and it never ran it", id, n)
				}
			}
			if out.Run.State != h.Run {
				t.Errorf("the run is %s, want %s", out.Run.State, h.Run)
			}
		})
	}
}

// The container of a task the evaluator stopped may say it is running after the stop went out.
// The narration has already said cancelled, and does not say running after it.
func TestAStoppedTaskIsNotNarratedRunningAgain(t *testing.T) {
	h := stoptest.Histories[slices.IndexFunc(stoptest.Histories, func(h stoptest.History) bool { return h.Name == "merge first" })]
	p := newPlaying(h, 0, 0)
	s := session(t, p)
	p.observe = func(id agk.TaskID) { s.observations <- driver.Event{Task: id, State: agk.TaskRunning} }
	var archive []agk.TaskState
	if _, err := s.Run(t.Context(), Request{Graph: built(t, h.Workflow), Tree: t.TempDir(), Inputs: h.Inputs, Events: func(e Event) {
		if e.Step == h.Late.Step && e.Shard.Index == h.Late.Shard && e.Attempt > 0 {
			archive = append(archive, e.State)
		}
	}}); err != nil {
		t.Fatal(err)
	}
	if at := slices.Index(archive, agk.TaskCancelled); at < 0 || slices.Contains(archive[at:], agk.TaskRunning) {
		t.Errorf("archive was narrated %v", archive)
	}
}
