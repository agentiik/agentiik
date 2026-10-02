package console

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// At full width each run carries a strip of its steps, one cell each in its verdict's colour, and
// its workflow's last twenty runs, read once for each workflow listed and again a minute later.
func TestAtFullWidthARunCarriesItsStepsAndItsWorkflowsLastRuns(t *testing.T) {
	runs := someRuns()
	runs[1].Steps = []db.StepStrip{{Step: "normalize", Verdict: agk.VerdictSucceeded}, {Step: "invoice", Verdict: agk.VerdictFailed}, {Step: "archive", Verdict: agk.VerdictSkipped}}
	in := &installation{runs: runs}
	m := opened(t, in, Options{}, 160, 24)
	s := screen(m)
	if !strings.Contains(s, "STEPS        LAST 20") || !strings.Contains(s, "alice          ■■□          ▁▂█") {
		t.Errorf("a run's steps and its workflow's last runs are not drawn at full width:\n%s", s)
	}
	for _, wf := range []string{"monthly-invoicing", "nightly-export"} {
		n := 0
		for _, a := range in.asked {
			if a == "/api/v1/runs?limit=20&namespace=finance&workflow="+wf {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the last runs of %s were read %d times", wf, n)
		}
	}

	// Read again within the minute, the last runs are not; a minute later, they are.
	asked := len(in.asked)
	reread := func(m Model) Model {
		var tm tea.Model = m
		tm, cmd := tm.Update(again{view: runsView, shown: m.shown})
		for _, msg := range run(cmd) {
			tm = send(t, tm, msg)
		}
		return tm.(Model)
	}
	m = reread(m)
	if len(in.asked) != asked+1 {
		t.Errorf("the runs read again within a minute asked %v", in.asked[asked:])
	}
	later := now.Add(61 * time.Second)
	m.o.Now = func() time.Time { return later }
	asked = len(in.asked)
	reread(m)
	if len(in.asked) != asked+3 {
		t.Errorf("the runs read again a minute later asked %v", in.asked[asked:])
	}

	narrow := opened(t, &installation{runs: runs}, Options{}, 159, 24)
	if strings.Contains(screen(narrow), "LAST 20") {
		t.Errorf("under 160 columns the last runs are drawn:\n%s", screen(narrow))
	}
}
