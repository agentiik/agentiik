package console

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
)

func withNotices(ns ...notice) *principal {
	return &principal{Principal: "alice", Permissions: map[string][]string{"finance": {"run:read"}}, Notifications: ns}
}

var (
	widened  = notice{ID: "01M2AF6V8X0Z2B4D6F8H0K2M4P", Kind: "admin_access_widened", Act: "granted", By: "carol", Namespace: "finance"}
	recovery = notice{ID: "01M2AH3S5V7X9Z1B3D5F7H9K1M", Kind: "break_glass_recovery", Login: "alice"}
	counter  = notice{ID: "01M2AG2R4T6W8Y0A2C4E6G8J0M", Kind: "passkey_counter_refused"}
)

func readAgain(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	tm, cmd := m.Update(msg)
	for _, next := range run(cmd) {
		tm = send(t, tm, next)
	}
	return tm.(Model)
}

// Notifications waiting at start are counted in the top line and not toasted, so that opening the
// console never buries the screen; one arriving while it is open, read half a minute later, is
// toasted in the corner, and goes eight seconds after.
func TestANotificationArrivingWhileOpenIsToasted(t *testing.T) {
	in := &installation{runs: someRuns(), me: withNotices(widened)}
	m := opened(t, in, Options{}, 120, 24)
	if top := strings.Split(screen(m), "\n")[0]; !strings.Contains(top, "1 notification  ● live") || len(m.toasts) != 0 {
		t.Fatalf("a notification waiting at start is toasted, or not counted: %q, %v", top, m.toasts)
	}
	in.me = withNotices(recovery, widened)
	asked := len(in.asked)
	m = readAgain(t, m, meAgain{})
	if in.asked[asked] != "/api/v1/me" {
		t.Fatalf("half a minute later /me is not read again: %v", in.asked[asked:])
	}
	s := screen(m)
	if !strings.Contains(s, "2 notifications") || !strings.Contains(s, "A recovery code was issued to alice from the") || !strings.Contains(s, "╭") || strings.Contains(s, "carol wrote") {
		t.Errorf("the notification arrived is not toasted alone:\n%s", s)
	}
	for i, l := range strings.Split(s, "\n") {
		if w := len([]rune(l)); w != 120 {
			t.Errorf("line %d is %d columns wide under a toast: %q", i, w, l)
		}
	}
	// A key pressed while a toast shows goes where it was going.
	if m = press(t, m, down); m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" || len(m.toasts) != 1 {
		t.Errorf("a key pressed under a toast went to it: %q", m.selected)
	}
	m = readAgain(t, m, toastGone{id: recovery.ID})
	if len(m.toasts) != 0 || strings.Contains(screen(m), "recovery code") {
		t.Errorf("the toast stays past its eight seconds:\n%s", screen(m))
	}
}

// The end of a run the principal started, seen going and then over in the runs the console reads,
// is toasted; one somebody else started, or one already over when first read, is not.
func TestTheEndOfYourRunIsToasted(t *testing.T) {
	runs := someRuns()
	runs[2].State, runs[2].FinishedAt = agk.Running, time.Time{}
	runs[2].TriggeredBy = "bob"
	in := &installation{runs: runs}
	m := opened(t, in, Options{}, 120, 24)
	ended := someRuns()
	ended[0].State, ended[0].FinishedAt = agk.Succeeded, now
	ended[2].TriggeredBy = "bob"
	in.runs = ended
	m = readAgain(t, m, again{view: runsView, shown: m.shown})
	if len(m.toasts) != 1 || m.toasts[0].text != "Your run 01RUNAAAAAAAAAAAAAAAAAAAAA of finance/monthly-invoicing ended succeeded." {
		t.Errorf("the runs ended are toasted %v", m.toasts)
	}
}

func TestANotificationSaysWhatTheWebConsoleSays(t *testing.T) {
	for n, want := range map[notice]string{
		widened:                        "carol wrote a grant by the installation's power in finance.",
		recovery:                       "A recovery code was issued to alice from the installation's host.",
		counter:                        "A sign-in with one of your passkeys was refused: its signature counter did not move forward, as a copied authenticator's does.",
		{Kind: "admin_access_widened"}: "An administrator widened access in a namespace.",
	} {
		if got := n.sentence(); got != want {
			t.Errorf("%s says %q, where the web console says %q", n.Kind, got, want)
		}
	}
}
