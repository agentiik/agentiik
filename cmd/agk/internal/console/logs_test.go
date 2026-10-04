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
	"charm.land/lipgloss/v2"
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

// soon is what f answers, the test failing where it has not answered within ten seconds: far more
// than drawing a line takes, and far less than a cut that measures again after each character
// takes on a line of a mebibyte, which is hours.
func soon[T any](t *testing.T, what string, f func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s took more than ten seconds, as a cut that measures again after each character does", what)
	}
	var none T
	return none
}

// A part of a mebibyte is cut to its width in one pass, a wide character left out whole rather
// than split, and ends in an ellipsis.
func TestALineOfAMebibyteIsCutInOnePass(t *testing.T) {
	cut := soon(t, "cutting a mebibyte", func() []part { return within([]part{{plain, strings.Repeat("x", 1<<20)}}, 120) })
	if w := widthOf(cut); w != 120 || !strings.HasSuffix(cut[len(cut)-1].text, "x…") {
		t.Errorf("a mebibyte cut to 120 columns is %d wide: %q", w, cut[len(cut)-1].text[max(0, len(cut[len(cut)-1].text)-20):])
	}
	wide := within([]part{{plain, "ab"}, {plain, strings.Repeat("漢", 1<<10)}}, 7)
	if got := wide[0].text + wide[1].text; got != "ab漢漢…" {
		t.Errorf("wide characters cut to 7 columns are %q", got)
	}
	// A keycap is an ASCII character, U+FE0F and U+20E3, two columns to Lip Gloss, which theme.line
	// pads by, and one to a cut that counts ASCII a byte at a time.
	if got := within([]part{{plain, strings.Repeat("1️⃣ ", 1<<10)}}, 40); widthOf(got) != 40 || got[0].text != strings.Repeat("1️⃣ ", 13)+"…" {
		t.Errorf("keycaps cut to 40 columns are %d wide: %q", widthOf(got), got[0].text)
	}
	// At every width, what is cut is no wider, and narrower by a column at most, where a cluster
	// two columns wide was left out whole.
	clusters := []part{{plain, "a 1️⃣ x️ 漢 é 👨‍👩‍👧 🇫🇷 ⚠️ "}, {muted, strings.Repeat("#️⃣ ナ 👍🏽 ", 8)}}
	for width := 1; width <= widthOf(clusters); width++ {
		if cut := within(clusters, width); widthOf(cut) > width || widthOf(cut) < width-1 {
			t.Errorf("cut to %d columns, the clusters are %d wide: %+v", width, widthOf(cut), cut)
		}
	}
}

// A log line of keycaps, which Lip Gloss measures two columns each, is cut inside its pane.
func TestALogLineOfKeycapsIsCutInsideItsPane(t *testing.T) {
	logs := &stepLogs{lines: map[string][]string{"invoice": {"invoice | answer: " + strings.Repeat("1️⃣ 2️⃣ 3️⃣ ", 30), "invoice | the end"}}}
	m := followingLogs(t, &installation{}, logs, 160, 40)
	s := screen(m)
	drawn := false
	for i, l := range strings.Split(s, "\n") {
		if w := lipgloss.Width(l); w != 160 {
			t.Errorf("line %d is %d columns wide in a window of 160: %q", i, w, l)
		}
		drawn = drawn || strings.Contains(l, "invoice | answer: 1️⃣ 2️⃣ 3️⃣") && strings.HasSuffix(l, "… │")
	}
	if !drawn {
		t.Errorf("the line of keycaps is not drawn cut inside its pane:\n%s", s)
	}
}

// A log line of a mebibyte, as a runner ships at most, is kept to its first lineKept bytes and an
// ellipsis, and drawn at once, cut inside its pane, at every size.
func TestALogLineOfAMebibyteIsKeptShortAndDrawnAtOnce(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{80, 24}, {160, 40}} {
		logs := &stepLogs{lines: map[string][]string{"invoice": {"invoice | " + strings.Repeat("x", 1<<20)}}, live: map[string]bool{"invoice": true}}
		m := followingLogs(t, &installation{}, logs, size[0], size[1])
		kept := m.logs[failedRun+"/invoice"].lines
		if len(kept) != 1 || len(kept[0].text) != lineKept+len("…") || !strings.HasSuffix(kept[0].text, "x…") {
			t.Fatalf("the inspector keeps %d lines, the first %d bytes long", len(kept), len(kept[0].text))
		}
		s := soon(t, "drawing a log line of a mebibyte", func() string { return screen(m) })
		drawn := false
		for i, l := range strings.Split(s, "\n") {
			if w := lipgloss.Width(l); w != size[0] {
				t.Errorf("at %d by %d, line %d is %d columns wide: %q", size[0], size[1], i, w, l)
			}
			drawn = drawn || strings.Contains(l, "invoice | xxx") && strings.HasSuffix(strings.TrimRight(l, " │"), "x…")
		}
		if !drawn {
			t.Errorf("at %d by %d, the line is not drawn cut with an ellipsis:\n%s", size[0], size[1], s)
		}
	}
}

// What the inspector keeps of a line is cut where a character starts, and made safe to draw before
// its ellipsis, so that a sequence the cut left unended takes nothing after it.
func TestALineIsCutWhereACharacterStarts(t *testing.T) {
	if got := clipped(strings.Repeat("x", lineKept)); got != strings.Repeat("x", lineKept) {
		t.Errorf("a line of lineKept bytes is cut: %d bytes", len(got))
	}
	if got := clipped(strings.Repeat("x", lineKept-1) + "漢"); got != strings.Repeat("x", lineKept-1)+"…" {
		t.Errorf("a character across the cut is split: %q", got[len(got)-8:])
	}
	if got := clipped(strings.Repeat("x", lineKept-8) + "\x1b]0;a window title"); got != strings.Repeat("x", lineKept-8)+"…" {
		t.Errorf("a sequence cut short takes the ellipsis: %q", got[len(got)-8:])
	}
}
