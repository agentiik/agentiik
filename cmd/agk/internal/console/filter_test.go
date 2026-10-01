package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

func typed(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m = press(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

var (
	slash     = tea.KeyPressMsg{Code: '/', Text: "/"}
	backspace = tea.KeyPressMsg{Code: tea.KeyBackspace}
)

// manyRuns is the runs of three workflows by two people, so that a word has something to tell
// apart.
func manyRuns() []db.ListedRun {
	runs := someRuns()
	extra := someRuns()[2]
	extra.Run, extra.Workflow, extra.TriggeredBy, extra.Trigger = "01RUNDDDDDDDDDDDDDDDDDDDDD", "invoice-archive", "bob", agk.TriggerSchedule
	return append(runs, extra)
}

// A word typed after / narrows the runs at once, its letters matched in order against a run's
// identifier, workflow, state, trigger and principal, the closest first; enter keeps the filter and
// esc clears it.
func TestWordsNarrowTheRunsAsTheyAreTyped(t *testing.T) {
	in := &installation{runs: manyRuns()}
	m := press(t, opened(t, in, Options{}, 120, 24), slash)
	if !m.filtering || !strings.Contains(screen(m), "/ filter as you type, or state=failed since=24h") || lastLine(m) != "type Filter   ↑↓ Move   enter Keep   esc Clear" {
		t.Fatalf("/ does not open the filter:\n%s", screen(m))
	}
	m = typed(t, m, "inv fail")
	s := screen(m)
	if !strings.Contains(s, "/ inv fail▏") || !strings.Contains(s, "1 of 4 runs") || !strings.Contains(s, "01RUNBBBBBBBBBBBBBBBBBBBBB") || strings.Contains(s, "01RUNAAAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("inv fail does not leave the failed invoicing run alone:\n%s", s)
	}
	if m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("the selection is left on %q, a run the filter hides", m.selected)
	}
	// q is a letter typed, not quit, while the line is open.
	if next, cmd := m.Update(quit); next.(Model).filter != "inv failq" || cmd != nil {
		t.Errorf("q typed into the filter leaves %q", next.(Model).filter)
	}
	m = press(t, m, backspace, backspace, backspace, backspace)
	m = typed(t, m, "bob")
	if s := screen(m); !strings.Contains(s, "01RUNDDDDDDDDDDDDDDDDDDDDD") || strings.Contains(s, "01RUNBBBBBBBBBBBBBBBBBBBBB") {
		t.Errorf("inv bob does not leave bob's invoice-archive run:\n%s", s)
	}
	m = press(t, m, enter)
	if m.filtering || !strings.Contains(screen(m), "/ inv bob") || !strings.Contains(lastLine(m), "esc Clear") {
		t.Errorf("enter does not keep the filter and close its line:\n%s", screen(m))
	}
	m = press(t, m, esc)
	if m.filter != "" || strings.Count(screen(m), "01RUN") < 4 {
		t.Errorf("esc does not clear the filter:\n%s", screen(m))
	}
}

// The closest match comes first: a word whole at the start of a word of the workflow before one
// whose letters are apart.
func TestTheClosestMatchesComeFirst(t *testing.T) {
	if a, b := matched("inv", "finance/invoice-archive"), matched("inv", "finance/monthly-invoicing"); a <= 0 || b <= 0 {
		t.Errorf("inv matches neither invoicing workflow: %d, %d", a, b)
	}
	if whole, apart := matched("inv", "finance/invoice-archive"), matched("inv", "finance/nightly-export-v"); whole <= apart {
		t.Errorf("inv whole scores %d, inv apart %d", whole, apart)
	}
	if matched("xyz", "finance/monthly-invoicing") != -1 {
		t.Error("a word whose letters are not there matches")
	}
	// ni is whole at the start of nightly-export, whole inside running, and apart in the others.
	in := &installation{runs: manyRuns()}
	m := typed(t, press(t, opened(t, in, Options{}, 120, 24), slash), "ni")
	shown := m.shownRuns()
	var order []string
	for _, r := range shown {
		order = append(order, string(r.Run)[5:6])
	}
	if strings.Join(order, "") != "CABD" {
		t.Errorf("ni orders the runs %v", order)
	}
}

// A name=value term is asked of the installation once typing pauses, since also taking a duration
// back from now, and the runs it answers are what the words then narrow; a term the API refuses is
// said on the filter's line.
func TestTermsAreAskedOfTheInstallationOnceTypingPauses(t *testing.T) {
	in := &installation{runs: manyRuns()}
	m := opened(t, in, Options{}, 120, 24)
	var tm tea.Model = press(t, m, slash)
	for _, r := range "state=failed since=24h" {
		tm, _ = tm.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m = tm.(Model)
	asked := len(in.asked)
	// A pause passed since an earlier key is not the last one's.
	if _, cmd := m.Update(typedPaused{typed: m.keys - 1}); cmd != nil {
		t.Error("a pause passed since an earlier key asked the installation")
	}
	m = send(t, m, typedPaused{typed: m.keys}).(Model)
	want := "/api/v1/runs?limit=100&state=failed&since=" + url.QueryEscape(now.Add(-24*time.Hour).UTC().Format(time.RFC3339))
	if len(in.asked) != asked+1 || in.asked[asked] != want {
		t.Fatalf("the terms asked for %v, not %s", in.asked[asked:], want)
	}
	// Read again while kept live, the terms are asked again, since back from now.
	tm, cmd := m.Update(again{view: runsView, shown: m.shown})
	for _, msg := range run(cmd) {
		tm = send(t, tm, msg)
	}
	if last := in.asked[len(in.asked)-1]; last != want {
		t.Errorf("the runs read again asked %s", last)
	}

	in.failing = errSaid("state: bogus is no run state")
	m = typed(t, press(t, tm.(Model), backspace, backspace, backspace), "x")
	m = send(t, m, typedPaused{typed: m.keys}).(Model)
	if s := screen(m); !strings.Contains(s, "The runs could not be read with these terms: state: bogus is no run state") || strings.Contains(strings.Split(s, "\n")[0], "not answering") {
		t.Errorf("a refused term is not said on the filter's line:\n%s", s)
	}
}

type errSaid string

func (e errSaid) Error() string { return string(e) }

// A name the API does not take is said, and sent nowhere.
func TestAnUnknownTermIsSaid(t *testing.T) {
	in := &installation{runs: manyRuns()}
	m := typed(t, press(t, opened(t, in, Options{}, 120, 24), slash), "colour=blue")
	m = send(t, m, typedPaused{typed: m.keys}).(Model)
	if s := screen(m); !strings.Contains(s, "colour: a term is namespace, workflow, state, since or until") {
		t.Errorf("an unknown term is not said:\n%s", s)
	}
	for _, a := range in.asked {
		if strings.Contains(a, "colour") {
			t.Errorf("an unknown term was sent: %s", a)
		}
	}
}

// A namespace the filter names is asked for in place of the one the console opened on.
func TestTheFiltersNamespaceReplacesTheOneOpenedOn(t *testing.T) {
	in := &installation{runs: manyRuns()}
	m := typed(t, press(t, opened(t, in, Options{Namespace: "finance"}, 120, 24), slash), "namespace=ops")
	send(t, m, typedPaused{typed: m.keys})
	if last := in.asked[len(in.asked)-1]; last != "/api/v1/runs?limit=100&namespace=ops" {
		t.Errorf("the filter's namespace asked %s", last)
	}
}

func TestADurationBackIsReadAsGoOrDaysWriteIt(t *testing.T) {
	for s, want := range map[string]time.Duration{"24h": 24 * time.Hour, "30m": 30 * time.Minute, "7d": 7 * 24 * time.Hour} {
		if got, ok := durationBack(s); !ok || got != want {
			t.Errorf("%s reads as %s, %v", s, got, ok)
		}
	}
	for _, s := range []string{"2026-09-24T00:00:00Z", "-1h", "yesterday"} {
		if _, ok := durationBack(s); ok {
			t.Errorf("%s reads as a duration", s)
		}
	}
	if q := parse("since=2026-09-24T00:00:00Z").query(now); q != "since=2026-09-24T00%3A00%3A00Z" {
		t.Errorf("an instant is sent as %s", q)
	}
}
