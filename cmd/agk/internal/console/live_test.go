package console

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// frames records the frames the console asks for, on a clock that never fires.
type frames struct{ asked []time.Duration }

func (f *frames) tick(d time.Duration, _ func(time.Time) tea.Msg) tea.Cmd {
	f.asked = append(f.asked, d)
	return nil
}

// A spinner turns beside a run going on, a frame at a time, and stops once nothing shown is going
// on: an animation on something settled would teach the eye to ignore the ones that matter.
func TestTheSpinnerTurnsWhileARunGoesOnAlone(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 24)
	if !m.spinning || !strings.Contains(screen(m), "⠋ running   01RUNAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatalf("no spinner turns beside the run going on:\n%s", screen(m))
	}
	if !strings.Contains(screen(m), "● failed    01RUNBBBBBBBBBBBBBBBBBBBBB") {
		t.Errorf("a settled run turns too:\n%s", screen(m))
	}
	f := &frames{}
	m.tick = f.tick
	next, _ := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	m = next.(Model)
	if !strings.Contains(screen(m), "⠙ running") || len(f.asked) != 1 || f.asked[0] != spinner.MiniDot.FPS {
		t.Errorf("a frame does not turn the spinner and ask for the next: %v\n%s", f.asked, screen(m))
	}

	// Filtered to the settled runs, nothing is going on, and the next frame is not asked for.
	m.filter = "succeeded"
	next, _ = m.Update(spinner.TickMsg{ID: m.spinner.ID()})
	m = next.(Model)
	if m.spinning || len(f.asked) != 1 {
		t.Errorf("the spinner still turns over runs that are settled: %v", f.asked)
	}
	settled := opened(t, &installation{runs: someRuns()[1:]}, Options{}, 120, 24)
	if settled.spinning {
		t.Error("a view where nothing is going on starts the spinner")
	}
}

// A fan-out is drawn one cell per shard in its state beside its step, and a bar of those done in
// the step chosen, a shard not dispatched yet an empty cell.
func TestAFanOutIsDrawnShardByShard(t *testing.T) {
	m := inspecting(t, &installation{}, 160)
	s := screen(m)
	if !strings.Contains(s, "■■■ 3 shards · 1 failed") || !strings.Contains(s, "shards  ██████████ 3/3 done") {
		t.Errorf("invoice's three shards are not drawn:\n%s", s)
	}

	d := aFailedRun()
	d.State = agk.Running
	shard := func(i int) *agk.Shard { return &agk.Shard{Index: i, Of: 8} }
	d.Steps[1].Verdict = agk.VerdictRunning
	d.Tasks = []db.TaskSummary{
		{Step: "invoice", State: agk.TaskSucceeded, Attempt: 1, Shard: shard(1)},
		{Step: "invoice", State: agk.TaskSucceeded, Attempt: 1, Shard: shard(2)},
		{Step: "invoice", State: agk.TaskRunning, Attempt: 1, Shard: shard(3)},
	}
	in := &installation{}
	in.run = d
	m = opened(t, in, Options{Run: failedRun}, 160, 40)
	s = screen(m)
	if !strings.Contains(s, "⠋ running    invoice") || !strings.Contains(s, "■■■□□□□□ 3 shards") || !strings.Contains(s, "shards  ██░░░░░░░░ 2/8 done") {
		t.Errorf("a fan-out under way is not drawn shard by shard:\n%s", s)
	}
	if !strings.Contains(s, "3/8    ⠋ running") {
		t.Errorf("no spinner turns beside the shard going on:\n%s", s)
	}
}

// Past the room given, a cell stands for a run of shards, and takes a failure among them.
func TestACellStandsForSeveralShardsPastTheRoom(t *testing.T) {
	var tasks []db.TaskSummary
	for i := 1; i <= 40; i++ {
		state := agk.TaskSucceeded
		if i == 20 {
			state = agk.TaskFailed
		}
		tasks = append(tasks, db.TaskSummary{Step: "s", State: state, Shard: &agk.Shard{Index: i, Of: 40}})
	}
	cells := shardCells(tasks, 40, 16)
	if len(cells) != 14 {
		t.Fatalf("40 shards in the room of 16 are %d cells", len(cells))
	}
	failed := 0
	for _, c := range cells {
		if c.role == failedText {
			failed++
		}
	}
	if failed != 1 || cells[6].role != failedText {
		t.Errorf("the cell of shards 19 to 21 does not take the failure of shard 20: %v", cells)
	}
}

// The run's header draws its workflow's last runs, oldest first, beside their median.
func TestARunIsDrawnBesideItsWorkflowsLastRuns(t *testing.T) {
	m := inspecting(t, &installation{runs: someRuns()}, 160)
	if s := screen(m); !strings.Contains(s, "last 3 ▁▂█  median 1m 07s") {
		t.Errorf("the run's workflow's last runs are not drawn:\n%s", s)
	}
	parts := sparkline(someRuns(), now)
	roles := []role{}
	for _, p := range parts[1:4] {
		roles = append(roles, p.role)
	}
	if roles[0] != muted || roles[1] != failedText || roles[2] != runningText {
		t.Errorf("the bars are drawn %v, where a success is muted, a failure red and the run going on in the accent", roles)
	}
}
