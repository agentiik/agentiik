package local

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/stoptest"
)

// playing is a driver that plays one history: every task exits with the code the history gives
// it as soon as it starts, except the late one, which exits 0 once its stop has been sent, as a
// container that finished in the moment before the stop reached it.
type playing struct {
	h stoptest.History

	// refusals is how many stops of each task the driver refuses before it takes one.
	refusals int

	mu      sync.Mutex
	stopped map[agk.TaskID]chan struct{}
	asked   map[agk.TaskID]int
	waited  bool
}

func newPlaying(h stoptest.History, refusals int) *playing {
	return &playing{h: h, refusals: refusals, stopped: map[agk.TaskID]chan struct{}{}, asked: map[agk.TaskID]int{}}
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
	if p.asked[s.Task] <= p.refusals {
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
			s := session(t, newPlaying(h, 0))
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
				if sh.Task != want.State || sh.ExitCode != want.ExitCode || sh.NoExitCode {
					t.Errorf("%s shard %d ended %s, exit %d, no exit code %t, want %s, exit %d", task.Step, task.Shard, sh.Task, sh.ExitCode, sh.NoExitCode, want.State, want.ExitCode)
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

// The evaluator names a stop sent while the run goes on once, so a stop the driver refused is the
// loop's to send again, on the next pass, until one is taken. Here the pass that stops archive
// starts pick, whose ending is that next pass.
func TestAStopTheDriverRefusedIsSentAgain(t *testing.T) {
	h := stoptest.Histories[slices.IndexFunc(stoptest.Histories, func(h stoptest.History) bool { return h.Name == "merge first" })]
	p := newPlaying(h, 1)
	out, err := session(t, p).Run(t.Context(), Request{Graph: built(t, h.Workflow), Tree: t.TempDir(), Inputs: h.Inputs})
	if err != nil {
		t.Fatal(err)
	}
	var archive agk.TaskID
	for id := range p.asked {
		archive = id
	}
	if p.waited || p.asked[archive] != 2 {
		t.Errorf("the driver was asked to stop %s %d times, and it came back on its own %t", archive, p.asked[archive], p.waited)
	}
	if out.Run.State != h.Run {
		t.Errorf("the run is %s, want %s", out.Run.State, h.Run)
	}
}
