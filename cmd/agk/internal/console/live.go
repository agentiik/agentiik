package console

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// What changes while the console is open is shown changing, and nothing moves that is not live: an
// animation on something settled would teach the eye to ignore the ones that matter. A spinner
// turns beside every running run, step and shard, in the accent's hue, beside the word running; a
// fan-out is drawn one cell per shard in its state, with a bar of those done; and a run's workflow
// is drawn as the durations of its last twenty runs beside their median.

// newSpinner is the spinner every running thing turns, one character wide so that a column of
// states keeps its width: Bubbles' MiniDot.
func newSpinner() spinner.Model { return spinner.New(spinner.WithSpinner(spinner.MiniDot)) }

// mark is the sign beside a state's word: the spinner's frame for what is going on, and a dot for
// what is settled.
func (m Model) mark(going bool) string {
	if going {
		return m.spinner.View()
	}
	return "●"
}

// live says whether anything shown is going on, which is what the spinner turns for.
func (m Model) live() bool {
	switch m.view {
	case runsView:
		return slices.ContainsFunc(m.shownRuns(), func(r db.ListedRun) bool { return r.State == agk.Running })
	case runView, graphView:
		return m.run != nil && m.run.State == agk.Running
	}
	return false
}

// nextFrame is the spinner's next frame, on the console's own clock rather than the spinner's, so
// that it stops once nothing is going on and a test that never waits draws the first frame alone.
func (m Model) nextFrame() tea.Cmd {
	id := m.spinner.ID()
	return m.tick(m.spinner.Spinner.FPS, func(t time.Time) tea.Msg { return spinner.TickMsg{ID: id, Time: t} })
}

// turned is the spinner a frame on, and the next frame asked for while anything is still going on.
func (m Model) turned(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	// The spinner's own next tick is left: nextFrame asks for it, or does not.
	m.spinner, _ = m.spinner.Update(msg)
	if !m.live() {
		m.spinning = false
		return m, nil
	}
	return m, m.nextFrame()
}

// shardCells is a fan-out as one cell per shard in its state, a shard not dispatched yet an empty
// one; past the room given, a cell stands for a run of shards and takes the state that matters
// most among them, a failure before a shard going on before one waiting before one done.
func shardCells(tasks []db.TaskSummary, of, room int) []part {
	if of <= 1 || room <= 0 {
		return nil
	}
	byIndex := map[int]db.TaskSummary{}
	for _, t := range tasks {
		if t.Shard != nil {
			byIndex[t.Shard.Index] = t
		}
	}
	per := (of + room - 1) / room
	var cells []part
	for first := 1; first <= of; first += per {
		worst, rank := part{quiet, "□"}, -1
		for i := first; i < first+per && i <= of; i++ {
			c, r := part{quiet, "□"}, 1
			if t, ok := byIndex[i]; ok {
				c = part{taskRole(t.State), "■"}
				switch taskRole(t.State) {
				case failedText:
					r = 3
				case runningText:
					r = 2
				case succeededText:
					r = 0
				}
			}
			if r > rank {
				worst, rank = c, r
			}
		}
		cells = append(cells, worst)
	}
	return cells
}

// shardsOf is how many shards a step fans out to, as its tasks say it, and 0 where it does not.
func shardsOf(tasks []db.TaskSummary) int {
	for _, t := range tasks {
		if t.Shard != nil && t.Shard.Of > 1 {
			return t.Shard.Of
		}
	}
	return 0
}

// doneOf counts the shards that ended, however they did.
func doneOf(tasks []db.TaskSummary) int {
	done := 0
	for _, t := range tasks {
		switch t.State {
		case agk.TaskSucceeded, agk.TaskFailed, agk.TaskLost, agk.TaskTimedOut, agk.TaskCancelled:
			done++
		}
	}
	return done
}

// doneBar is a bar of the shards done, ten cells wide, and how many of how many.
func doneBar(tasks []db.TaskSummary, of int) []part {
	done := doneOf(tasks)
	filled := done * 10 / of
	return []part{{plain, strings.Repeat("█", filled)}, {quiet, strings.Repeat("░", 10-filled)}, {muted, fmt.Sprintf(" %d/%d done", done, of)}}
}

// recentRead is the last twenty runs of the workflow of the run shown.
type recentRead struct {
	run  string
	runs []db.ListedRun
}

// readRecent reads the last twenty runs of the run's workflow, for its sparkline.
func (m Model) readRecent() tea.Cmd {
	if m.run == nil {
		return nil
	}
	run := string(m.run.Run)
	path := "/api/v1/runs?limit=20&namespace=" + url.QueryEscape(m.run.Namespace) + "&workflow=" + url.QueryEscape(m.run.Workflow)
	return func() tea.Msg {
		var listed struct {
			Runs []db.ListedRun `json:"runs"`
		}
		if m.o.Read(m.ctx, path, &listed) != nil {
			// A sparkline is a glance, and one that cannot be read is left out.
			return recentRead{run: run}
		}
		return recentRead{run: run, runs: listed.Runs}
	}
}

// sparkline is the durations of a workflow's last twenty runs, one bar each, oldest first: a
// failure in red, the run in progress in the accent, the rest muted, beside their median. A run
// not started yet has taken nothing to draw, and is left out.
func sparkline(runs []db.ListedRun, now time.Time) []part {
	bars := []rune("▁▂▃▄▅▆▇█")
	type bar struct {
		took  time.Duration
		state agk.RunState
	}
	var drawn []bar
	var settled []time.Duration
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.StartedAt.IsZero() {
			continue
		}
		end := r.FinishedAt
		if end.IsZero() {
			end = now
		}
		drawn = append(drawn, bar{end.Sub(r.StartedAt), r.State})
		if r.State.Terminal() {
			settled = append(settled, end.Sub(r.StartedAt))
		}
	}
	if len(drawn) == 0 {
		return nil
	}
	longest := slices.MaxFunc(drawn, func(a, b bar) int { return int(a.took - b.took) }).took
	parts := []part{{quiet, "last " + fmt.Sprint(len(drawn)) + " "}}
	for _, b := range drawn {
		i := 0
		if longest > 0 {
			i = int(b.took * time.Duration(len(bars)-1) / longest)
		}
		r := muted
		switch {
		case failing(b.state):
			r = failedText
		case b.state == agk.Running:
			r = runningText
		}
		parts = append(parts, part{r, string(bars[i])})
	}
	if len(settled) > 0 {
		slices.Sort(settled)
		median := settled[len(settled)/2]
		if len(settled)%2 == 0 {
			median = (settled[len(settled)/2-1] + settled[len(settled)/2]) / 2
		}
		parts = append(parts, part{muted, "  median " + Took(median)})
	}
	return parts
}
