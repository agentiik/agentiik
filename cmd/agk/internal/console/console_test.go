package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func TestTookIsReadAtAGlance(t *testing.T) {
	for d, want := range map[time.Duration]string{
		820 * time.Millisecond:          "820ms",
		22 * time.Second:                "22s",
		112 * time.Second:               "1m 52s",
		64 * time.Minute:                "1h 04m",
		(3*24 + 2) * time.Hour:          "3d 02h",
		-time.Second:                    "",
		59*time.Minute + 59*time.Second: "59m 59s",
	} {
		if got := Took(d); got != want {
			t.Errorf("Took(%s) is %q, where it reads %q", d, got, want)
		}
	}
}

// A run not started yet is dated by its creation and has taken nothing; one going on has lasted
// until now.
func TestLinesDateARunNotStartedByItsCreation(t *testing.T) {
	created := now.Add(-time.Minute)
	runs := []db.ListedRun{
		{RunSummary: db.RunSummary{Namespace: "ops", Run: "01RUNQUEUED00000000000000A", Workflow: "nightly", State: agk.Queued, Trigger: agk.TriggerSchedule, CreatedAt: created}},
		{RunSummary: db.RunSummary{Namespace: "ops", Run: "01RUNGOING000000000000000A", Workflow: "nightly", State: agk.Running, Trigger: agk.TriggerManual, TriggeredBy: "alice", CreatedAt: created, StartedAt: now.Add(-90 * time.Second)}},
	}
	var b strings.Builder
	if err := Lines(&b, runs, now); err != nil {
		t.Fatal(err)
	}
	want := "queued\t01RUNQUEUED00000000000000A\tops/nightly\tschedule\t2026-09-25T11:59:00Z\t\t\n" +
		"running\t01RUNGOING000000000000000A\tops/nightly\tmanual\t2026-09-25T11:58:30Z\t1m 30s\talice\n"
	if b.String() != want {
		t.Errorf("Lines wrote\n%q\nwhere\n%q", b.String(), want)
	}
}

// installation is what the console reads, standing in for the command line's Reader.
type installation struct {
	runs      []db.ListedRun
	run       db.RunDetail
	me        *principal
	runners   []db.Runner
	pools     []api.Pool
	envelopes map[string]any
	failing   error
	asked     []string

	// sent is what was sent, as method, path and body, and refusing why a send is refused.
	sent     []string
	refusing error
}

// replayRun is the run a replay starts.
const replayRun = "01RUNREPLAYREPLAYREPLAYREP"

func (in *installation) send(_ context.Context, method, path string, body, out any) error {
	b, _ := json.Marshal(body)
	in.sent = append(in.sent, method+" "+path+" "+string(b))
	if in.refusing != nil {
		return in.refusing
	}
	if strings.HasSuffix(path, "/replay") && out != nil {
		return json.Unmarshal([]byte(`{"run":"`+replayRun+`","state":"queued","commit":"a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f","replay_of":"`+failedRun+`"}`), out)
	}
	return nil
}

func (in *installation) read(_ context.Context, path string, out any) error {
	in.asked = append(in.asked, path)
	if in.failing != nil {
		return in.failing
	}
	var answer any
	switch {
	case path == "/api/v1/me":
		answer = principal{Principal: "alice", Permissions: map[string][]string{"finance": {"run:read"}}}
		if in.me != nil {
			answer = *in.me
		}
	case path == "/api/v1/runners":
		if in.me == nil || !in.me.Admin {
			return errors.New("no such thing, or not yours")
		}
		answer = map[string]any{"runners": in.runners}
	case path == "/api/v1/runner-pools":
		if in.me == nil || !in.me.Admin {
			return errors.New("no such thing, or not yours")
		}
		listed := []api.RunnerPool{}
		for _, p := range in.pools {
			listed = append(listed, api.RunnerPool{Pool: p})
		}
		answer = map[string]any{"runner_pools": listed}
	case strings.HasPrefix(path, "/api/v1/runs?"):
		answer = map[string]any{"runs": in.runs}
	case strings.Contains(path, "/steps/"):
		e, ok := in.envelopes[path]
		if !ok {
			return errors.New("no such thing, or not yours")
		}
		answer = e
	case strings.HasPrefix(path, "/api/v1/runs/"):
		answer = in.run
	default:
		return fmt.Errorf("no route %s", path)
	}
	b, _ := json.Marshal(answer)
	return json.Unmarshal(b, out)
}

func someRuns() []db.ListedRun {
	at := now.Add(-10 * time.Minute)
	run := func(id, workflow string, state agk.RunState, took time.Duration) db.ListedRun {
		r := db.ListedRun{RunSummary: db.RunSummary{Namespace: "finance", Run: agk.RunID(id), Workflow: workflow, State: state, Trigger: agk.TriggerManual, TriggeredBy: "alice", CreatedAt: at, StartedAt: at}}
		if took > 0 {
			r.FinishedAt = at.Add(took)
		}
		return r
	}
	return []db.ListedRun{
		run("01RUNAAAAAAAAAAAAAAAAAAAAA", "monthly-invoicing", agk.Running, 0),
		run("01RUNBBBBBBBBBBBBBBBBBBBBB", "monthly-invoicing", agk.Failed, 112*time.Second),
		run("01RUNCCCCCCCCCCCCCCCCCCCCC", "nightly-export", agk.Succeeded, 22*time.Second),
	}
}

// opened is a console as Bubble Tea would hold it once it has started in a window of the size
// given and its first reads have come back.
func opened(t *testing.T, in *installation, o Options, width, height int) Model {
	t.Helper()
	o.Read, o.Send, o.Now = in.read, in.send, func() time.Time { return now }
	if o.Installation == "" {
		o.Installation = "agentiik.example.com"
	}
	n := New(t.Context(), o)
	n.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	var m tea.Model = n
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	for _, msg := range run(m.Init()) {
		m = send(t, m, msg)
	}
	return m.(Model)
}

// send hands the model one message, and the messages the reads it asks for bring back, but not a
// tick, which would wait.
func send(t *testing.T, m tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	m, cmd := m.Update(msg)
	for _, next := range run(cmd) {
		m = send(t, m, next)
	}
	return m
}

// run carries out a command and the batches it holds, leaving out the quit, which the test asks
// about itself, and a command still waiting after a moment, as one waiting for a live log's next
// line does, which is left to wait.
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(waited):
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, run(c)...)
		}
		return msgs
	}
	if _, ok := msg.(tea.QuitMsg); ok {
		return nil
	}
	return []tea.Msg{msg}
}

// waited is how long a test waits on a command before leaving it waiting.
const waited = 200 * time.Millisecond

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// screen is what the window shows, without the escapes that style it.
func screen(m Model) string { return ansi.ReplaceAllString(m.screen(), "") }

func press(t *testing.T, m Model, keys ...tea.KeyPressMsg) Model {
	t.Helper()
	var tm tea.Model = m
	for _, k := range keys {
		tm = send(t, tm, k)
	}
	return tm.(Model)
}

var (
	down  = tea.KeyPressMsg{Code: tea.KeyDown}
	up    = tea.KeyPressMsg{Code: tea.KeyUp}
	enter = tea.KeyPressMsg{Code: tea.KeyEnter}
	esc   = tea.KeyPressMsg{Code: tea.KeyEscape}
	j     = tea.KeyPressMsg{Code: 'j', Text: "j"}
	help  = tea.KeyPressMsg{Code: '?', Text: "?"}
	quit  = tea.KeyPressMsg{Code: 'q', Text: "q"}
)

// The runs view names the installation, every namespace and the principal on its top line, lifts
// the failed run into a band above the list, selects the newest run, and names its keys by their
// effect on the bottom line.
func TestTheRunsViewListsTheRunsWithTheFailedAbove(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 24)
	s := screen(m)
	lines := strings.Split(s, "\n")
	if len(lines) != 24 {
		t.Fatalf("the screen is %d lines high in a window of 24:\n%s", len(lines), s)
	}
	for _, w := range []string{"agentiik  agentiik.example.com  every namespace  alice", "live"} {
		if !strings.Contains(lines[0], w) {
			t.Errorf("the top line does not say %q: %s", w, lines[0])
		}
	}
	if !strings.HasPrefix(lines[1], "Failed, 1 of the last 3") || !strings.HasPrefix(lines[2], "● failed    01RUNBBBBBBBBBBBBBBBBBBBBB") {
		t.Errorf("the failed run is not lifted above the list:\n%s", s)
	}
	if !strings.HasPrefix(lines[4], "STATE       RUN") {
		t.Errorf("the list has no header where it starts:\n%s", s)
	}
	for _, w := range []string{"● running   01RUNAAAAAAAAAAAAAAAAAAAAA finance/monthly-invoicing", "1m 52s", "● succeeded 01RUNCCCCCCCCCCCCCCCCCCCCC finance/nightly-export"} {
		if !strings.Contains(s, w) {
			t.Errorf("the runs view does not show %q:\n%s", w, s)
		}
	}
	if m.selected != "01RUNAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("the runs view opens with %q selected, not the newest run", m.selected)
	}
	if last := strings.TrimRight(lines[23], " "); last != "↑↓ Move   enter Open   q Quit   ? Every key" {
		t.Errorf("the key line is %q", last)
	}
	// Every line is as wide as the window, so that the ground is painted under all of it.
	for i, l := range lines {
		if w := len([]rune(l)); w != 120 {
			t.Errorf("line %d is %d columns wide in a window of 120: %q", i, w, l)
		}
	}
}

// A window under 80 by 24 draws nothing cut: it is asked to grow, naming the size it needs.
func TestATooSmallWindowIsAskedToGrow(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 79, 24)
	if s := screen(m); !strings.Contains(s, "The window is 79 by 24: agk console needs 80 by 24.") || strings.Contains(s, "01RUN") {
		t.Errorf("a window of 79 by 24 shows:\n%s", s)
	}
	m = press(t, m, tea.KeyPressMsg{}) // nothing to do, but the model must keep
	if _, cmd := m.Update(quit); cmd == nil {
		t.Error("q does not quit a window too small to draw in")
	}
}

// ↓ and j move the selection, which stops at either end; enter opens the run selected, which is
// read and described; esc goes back to the runs.
func TestTheKeysMoveOpenAndGoBack(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := opened(t, in, Options{}, 100, 30)
	m = press(t, m, down, j, j)
	if m.selected != "01RUNCCCCCCCCCCCCCCCCCCCCC" {
		t.Fatalf("↓ then j twice selects %q, not the last run", m.selected)
	}
	m = press(t, m, up)
	if m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" {
		t.Fatalf("↑ selects %q", m.selected)
	}
	in.run = db.RunDetail{RunSummary: in.runs[1].RunSummary}
	m = press(t, m, enter)
	if m.view != runView || !strings.Contains(screen(m), "Run 01RUNBBBBBBBBBBBBBBBBBBBBB  finance/monthly-invoicing@") {
		t.Fatalf("enter does not open the run selected:\n%s", screen(m))
	}
	if in.asked[len(in.asked)-1] != "/api/v1/runs/01RUNBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("opening a run asked for %s", in.asked[len(in.asked)-1])
	}
	if !strings.HasSuffix(strings.TrimRight(screen(m), " "), "↑↓ Step   [] Port   esc Runs   q Quit   ? Every key") {
		t.Errorf("the run view's key line is wrong:\n%s", screen(m))
	}
	m = press(t, m, esc)
	if m.view != runsView || m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("esc leaves the console in view %d with %q selected", m.view, m.selected)
	}
}

// ? lists every key of the view over it, and esc closes the list rather than leaving the view.
func TestQuestionMarkListsEveryKey(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 100, 30)
	m = press(t, m, help)
	s := screen(m)
	for _, w := range []string{"Every key of this view", "q, ctrl+c", "enter        Open the run selected"} {
		if !strings.Contains(s, w) {
			t.Errorf("the list of keys does not say %q:\n%s", w, s)
		}
	}
	m = press(t, m, esc)
	if m.listing || m.view != runsView {
		t.Errorf("esc over the list of keys leaves listing %v in view %d", m.listing, m.view)
	}
}

// Opened on a run, the console reads that run alone and starts on it.
func TestAConsoleOpenedOnARunStartsOnIt(t *testing.T) {
	in := &installation{run: db.RunDetail{RunSummary: db.RunSummary{Namespace: "finance", Workflow: "monthly-invoicing", Run: "01RUNBBBBBBBBBBBBBBBBBBBBB", State: agk.Failed}}}
	m := opened(t, in, Options{Run: "01RUNBBBBBBBBBBBBBBBBBBBBB", Namespace: "finance"}, 100, 30)
	if m.view != runView || !strings.Contains(screen(m), "Run 01RUNBBBBBBBBBBBBBBBBBBBBB  finance/monthly-invoicing@") {
		t.Errorf("a console opened on a run shows:\n%s", screen(m))
	}
	if !strings.Contains(strings.Split(screen(m), "\n")[0], "finance") {
		t.Errorf("the top line does not name the namespace: %s", strings.Split(screen(m), "\n")[0])
	}
	for _, a := range in.asked {
		if strings.HasPrefix(a, "/api/v1/runs?") {
			t.Errorf("a console opened on a run read the runs too: %v", in.asked)
		}
	}
}

// An installation that stops answering is said so on the top line and asked again, and what was
// read before stays on the screen.
func TestAnInstallationThatStopsAnsweringIsSaidSo(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := opened(t, in, Options{}, 120, 24)
	in.failing = errors.New("the installation could not be reached")
	var tm tea.Model = m
	tm, cmd := tm.Update(again{view: runsView})
	tm = send(t, tm, cmd())
	m = tm.(Model)
	top := strings.Split(screen(m), "\n")[0]
	if !strings.Contains(top, "not answering, asked again: the installation could not be") {
		t.Errorf("the top line says %q", top)
	}
	if !strings.Contains(screen(m), "01RUNAAAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("the runs read before are gone:\n%s", screen(m))
	}
}

// A narrow window shows the first twelve characters of a run, which still tell two apart, and
// cuts what is too long with an ellipsis rather than wrapping it.
func TestANarrowWindowCutsRatherThanWraps(t *testing.T) {
	runs := someRuns()
	runs[0].Workflow = "a-workflow-whose-name-runs-on-and-on-past-any-column"
	m := opened(t, &installation{runs: runs}, Options{}, 80, 24)
	s := screen(m)
	if !strings.Contains(s, "01RUNAAAAAA… ") || strings.Contains(s, "01RUNAAAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("a run's identifier at 80 columns is not cut to twelve:\n%s", s)
	}
	if !strings.Contains(s, "finance/a-workflow-whose-n… ") || strings.Contains(s, "past-any-column") {
		t.Errorf("a long workflow is not cut:\n%s", s)
	}
	for i, l := range strings.Split(s, "\n") {
		if w := len([]rune(l)); w != 80 {
			t.Errorf("line %d is %d columns wide in a window of 80: %q", i, w, l)
		}
	}
}
