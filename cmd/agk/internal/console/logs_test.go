package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// stepLogs stands in for agk logs' follower: each step's lines and what is said about its log, a
// live one followed until it is no longer asked for, and a refused one stopped short of its end.
type stepLogs struct {
	lines   map[string][]string
	said    map[string][]string
	live    map[string]bool
	refused map[string]bool

	mu        sync.Mutex
	asked     []string
	cancelled map[string]chan struct{}
}

func (l *stepLogs) follow(ctx context.Context, run, step string, out, said io.Writer) error {
	l.mu.Lock()
	l.asked = append(l.asked, step)
	if l.cancelled == nil {
		l.cancelled = map[string]chan struct{}{}
	}
	gone := make(chan struct{})
	l.cancelled[step] = gone
	l.mu.Unlock()
	for _, line := range l.lines[step] {
		fmt.Fprintln(out, line)
	}
	for _, s := range l.said[step] {
		fmt.Fprintln(said, s)
	}
	switch {
	case l.live[step]:
		<-ctx.Done()
		close(gone)
		return ctx.Err()
	case l.refused[step]:
		return errors.New("refused")
	}
	return nil
}

// times is how often a step's log was asked for.
func (l *stepLogs) times(step string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, a := range l.asked {
		if a == step {
			n++
		}
	}
	return n
}

// stopped waits for the live log of a step last followed to be no longer asked for.
func (l *stepLogs) stopped(t *testing.T, step string) {
	t.Helper()
	l.mu.Lock()
	gone := l.cancelled[step]
	l.mu.Unlock()
	if gone == nil {
		t.Fatalf("the log of %s was never followed", step)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatalf("the log of %s is still followed", step)
	}
}

func invoiceLogs() *stepLogs {
	return &stepLogs{
		lines: map[string][]string{
			"normalize": {"normalize | read 3 orders", "normalize | wrote 3 items to ok"},
			"invoice":   {"invoice 2/3, attempt 4 | posting ORD-0002", "invoice 2/3, attempt 4 | the ledger answered 503"},
		},
		live: map[string]bool{"invoice": true},
	}
}

func followingLogs(t *testing.T, in *installation, logs *stepLogs, width, height int) Model {
	t.Helper()
	in.run = aFailedRun()
	return opened(t, in, Options{Run: failedRun, Follow: logs.follow}, width, height)
}

// The inspector follows the log of the step chosen beneath the run, live while it is followed;
// choosing another step stops following the first and follows the other, whose log read to its end
// is not asked for again, and a log stopped short of its end is followed again once chosen again.
func TestTheInspectorFollowsTheLogOfTheStepChosen(t *testing.T) {
	// Each waits on a live log a moment, and needs nothing of the others.
	t.Parallel()
	logs := invoiceLogs()
	m := followingLogs(t, &installation{}, logs, 100, 40)
	s := screen(m)
	for _, want := range []string{"LOG invoice  ● live", "invoice 2/3, attempt 4 | posting ORD-0002", "invoice 2/3, attempt 4 | the ledger answered 503"} {
		if !strings.Contains(s, want) {
			t.Errorf("the inspector does not show %q:\n%s", want, s)
		}
	}
	// Read again while it runs, the run follows nothing more.
	var tm tea.Model = m
	tm, cmd := tm.Update(again{view: runView, shown: m.shown})
	for _, msg := range run(cmd) {
		tm = send(t, tm, msg)
	}
	m = tm.(Model)
	if n := logs.times("invoice"); n != 1 {
		t.Errorf("the run read again asked for the log it follows %d times", n)
	}

	m = press(t, m, up)
	logs.stopped(t, "invoice")
	s = screen(m)
	if !strings.Contains(s, "LOG normalize  read to its end") || !strings.Contains(s, "normalize | wrote 3 items to ok") || strings.Contains(s, "ORD-0002") {
		t.Errorf("choosing normalize does not show its log alone:\n%s", s)
	}
	m = press(t, m, down, up)
	if logs.times("normalize") != 1 || logs.times("invoice") != 2 {
		t.Errorf("normalize's log was asked for %d times and invoice's %d", logs.times("normalize"), logs.times("invoice"))
	}
	if !strings.Contains(screen(m), "normalize | read 3 orders") {
		t.Errorf("normalize's log read to its end is not kept:\n%s", screen(m))
	}
}

// What is said about a log rather than in it is drawn beside its lines, and a log stopped short of
// its end is no longer said to be live. A step that wrote nothing says so.
func TestWhatIsSaidAboutALogIsDrawn(t *testing.T) {
	t.Parallel()
	logs := &stepLogs{
		lines:   map[string][]string{"invoice": {"invoice 2/3, attempt 4 | posting ORD-0002"}},
		said:    map[string][]string{"invoice": {"invoice: run 01RUNBBBBBBBBBBBBBBBBBBBBB has no such step, or is not there, or not yours"}},
		refused: map[string]bool{"invoice": true},
	}
	m := followingLogs(t, &installation{}, logs, 100, 40)
	s := screen(m)
	if !strings.Contains(s, "has no such step, or is not there, or not yours") || strings.Contains(s, "LOG invoice  ● live") || strings.Contains(s, "read to its end") {
		t.Errorf("a refused log is not said so:\n%s", s)
	}
	m = press(t, m, j)
	if s := screen(m); !strings.Contains(s, "LOG archive  read to its end") || !strings.Contains(s, "This step wrote no log.") {
		t.Errorf("a step that wrote nothing does not say so:\n%s", s)
	}
}

// The log takes what the run leaves, its last lines shown, each cut to the window rather than
// wrapped; at 80 columns it is under the panes as at 160.
func TestTheLogTakesWhatTheRunLeaves(t *testing.T) {
	t.Parallel()
	logs := invoiceLogs()
	logs.lines["invoice"] = nil
	for i := 1; i <= 100; i++ {
		logs.lines["invoice"] = append(logs.lines["invoice"], fmt.Sprintf("invoice 2/3, attempt 4 | line %03d %s", i, strings.Repeat("x", 200)))
	}
	for _, size := range [][2]int{{80, 24}, {160, 40}} {
		m := followingLogs(t, &installation{}, logs, size[0], size[1])
		s := screen(m)
		lines := strings.Split(s, "\n")
		if len(lines) != size[1] || !strings.Contains(s, "line 100") || strings.Contains(s, "line 001") || !strings.Contains(s, "LOG invoice") {
			t.Errorf("at %dx%d the log's last lines are not shown under the run:\n%s", size[0], size[1], s)
		}
		if !strings.Contains(s, "failed at invoice") {
			t.Errorf("at %dx%d the log has taken the run's header:\n%s", size[0], size[1], s)
		}
		for i, l := range lines {
			if w := len([]rune(l)); w != size[0] {
				t.Errorf("line %d is %d columns wide in a window of %d: %q", i, w, size[0], l)
			}
		}
		m.unfollow()
	}
}

// Leaving the run stops following its log.
func TestLeavingTheRunStopsFollowingItsLog(t *testing.T) {
	t.Parallel()
	logs := invoiceLogs()
	m := press(t, followingLogs(t, &installation{runs: someRuns()}, logs, 160, 40), esc)
	logs.stopped(t, "invoice")
	if m.follow != nil || strings.Contains(screen(m), "LOG") {
		t.Errorf("the runs view still follows a log:\n%s", screen(m))
	}
}
