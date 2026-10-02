package console

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

var (
	yes  = tea.KeyPressMsg{Code: 'y', Text: "y"}
	no   = tea.KeyPressMsg{Code: 'n', Text: "n"}
	keyC = tea.KeyPressMsg{Code: 'c', Text: "c"}
	keyP = tea.KeyPressMsg{Code: 'p', Text: "p"}
)

// mayRunIn is alice holding workflow:run in finance, which cancelling and replaying ask.
func mayRunIn() *principal {
	return &principal{Principal: "alice", Permissions: map[string][]string{"finance": {"run:read", "workflow:run"}}}
}

func openedOn(t *testing.T, in *installation, d db.RunDetail, width int) Model {
	t.Helper()
	in.run = d
	return opened(t, in, Options{Run: string(d.Run)}, width, 40)
}

// c asks before it cancels a run going on, naming the run; any key but y keeps it, and y sends the
// cancel, says it was asked, and reads the run again to show it ending.
func TestARunGoingOnIsCancelledOnceAskedY(t *testing.T) {
	in := &installation{me: mayRunIn()}
	d := aFailedRun()
	d.State = agk.Running
	m := openedOn(t, in, d, 160)
	if last := lastLine(m); !strings.Contains(last, "c Cancel run") || strings.Contains(last, "p Replay") {
		t.Errorf("a run going on offers %q", last)
	}
	m = press(t, m, keyC)
	s := screen(m)
	if !strings.Contains(s, "Cancel run "+failedRun+" of finance/monthly-invoicing? Its tasks in flight are stopped, and it ends cancelled.") || lastLine(m) != "y Cancel run   any other key Keep it" {
		t.Fatalf("c does not ask, naming the run:\n%s", s)
	}
	m = press(t, m, no)
	if m.asking != notAsking || len(in.sent) != 0 {
		t.Fatalf("n leaves the prompt %d, and sent %v", m.asking, in.sent)
	}
	m = press(t, m, keyC, enter)
	if len(in.sent) != 0 {
		t.Fatalf("enter, not y, sent %v", in.sent)
	}
	reads := len(in.asked)
	m = press(t, m, keyC, yes)
	if len(in.sent) != 1 || in.sent[0] != "POST /api/v1/runs/"+failedRun+"/cancel null" {
		t.Fatalf("y sent %v", in.sent)
	}
	if !strings.Contains(screen(m), "Cancelling was asked: the controller stops the tasks in flight, and the run ends cancelled.") {
		t.Errorf("the cancel asked is not said:\n%s", screen(m))
	}
	if !slices.Contains(in.asked[reads:], "/api/v1/runs/"+failedRun) {
		t.Errorf("the run is not read again once its cancel is asked: %v", in.asked[reads:])
	}
}

func lastLine(m Model) string {
	lines := strings.Split(screen(m), "\n")
	return strings.TrimRight(lines[len(lines)-1], " ")
}

// p asks before it replays a run over from the step chosen, naming both; y starts the replay and
// opens the run it started.
func TestARunOverIsReplayedFromTheStepChosen(t *testing.T) {
	in := &installation{me: mayRunIn()}
	m := openedOn(t, in, aFailedRun(), 160)
	if last := lastLine(m); !strings.Contains(last, "p Replay from step") || strings.Contains(last, "c Cancel") {
		t.Errorf("a run over offers %q", last)
	}
	m = press(t, m, keyP)
	if s := screen(m); !strings.Contains(s, "Replay run "+failedRun+" from invoice? A new run of a3f9c1e starts there, the steps above it reused.") || lastLine(m) != "y Replay from invoice   any other key Keep it" {
		t.Fatalf("p does not ask, naming the run and the step:\n%s", s)
	}
	m = press(t, m, yes)
	if len(in.sent) != 1 || in.sent[0] != `POST /api/v1/runs/`+failedRun+`/replay {"step":"invoice"}` {
		t.Fatalf("y sent %v", in.sent)
	}
	if m.view != runView || m.selected != replayRun || !slices.Contains(in.asked, "/api/v1/runs/"+replayRun) {
		t.Errorf("the run the replay started is not opened: %q, %v", m.selected, in.asked)
	}
}

// A replay refused is said as the installation says it, on the run that was to be replayed.
func TestARefusedReplayIsSaid(t *testing.T) {
	in := &installation{me: mayRunIn(), refusing: errors.New("a step above invoice was cancelled with its run, so the run replays from its start only")}
	m := press(t, openedOn(t, in, aFailedRun(), 160), keyP, yes)
	if s := screen(m); m.selected != failedRun || !strings.Contains(s, "The run could not be replayed from invoice: a step above invoice was cancelled with its run") {
		t.Errorf("a refused replay is not said:\n%s", s)
	}
}

// Who does not hold workflow:run is offered neither key, and a run replayable from its start alone
// is not offered p.
func TestCancelAndReplayAreWorkflowRunsAlone(t *testing.T) {
	in := &installation{}
	m := press(t, openedOn(t, in, aFailedRun(), 160), keyP, keyC)
	if m.asking != notAsking || len(in.sent) != 0 || strings.Contains(lastLine(m), "Replay") {
		t.Errorf("a principal without workflow:run is offered a replay: %q, sent %v", lastLine(m), in.sent)
	}
	if s := screen(press(t, m, help)); strings.Contains(s, "Cancel the run") || strings.Contains(s, "Replay the run") {
		t.Errorf("the list of keys offers what is not held:\n%s", s)
	}
	d := aFailedRun()
	d.ReplayFromStartOnly = true
	m = press(t, openedOn(t, &installation{me: mayRunIn()}, d, 160), keyP)
	if m.asking != notAsking || strings.Contains(lastLine(m), "Replay") {
		t.Errorf("a run replayable from its start alone is offered p: %q", lastLine(m))
	}
}

// A run that replays another says which, and from where.
func TestAReplaySaysWhatItReplays(t *testing.T) {
	d := aFailedRun()
	d.ReplayOf, d.ReplayFrom = "01RUNORIGINALORIGINALORIGI", "invoice"
	if s := screen(openedOn(t, &installation{}, d, 160)); !strings.Contains(s, "replays 01RUNORIGINALORIGINALORIGI from invoice") {
		t.Errorf("a replay does not say what it replays:\n%s", s)
	}
}
