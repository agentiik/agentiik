package console

import (
	"context"
	"net/url"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
)

// Cancelling a run and replaying it from a step are the two things the inspector does rather than
// reads, each workflow:run's on the run's workflow, and each asked first: a prompt names what it
// does to which run, y does it, and any other key keeps the run as it is, since a key is pressed by
// mistake more often than a run is stopped on purpose.

// Sender sends one request that changes something, as agk's other verbs send it, and decodes the
// answer into out where out is not nil: the installation, the credential and the refusals are the
// command line's.
type Sender func(ctx context.Context, method, path string, body, out any) error

// asking is the prompt open over the inspector, if any.
type asking int

const (
	notAsking asking = iota
	askingCancel
	askingReplay
)

type (
	// cancelled is the cancel asked for, or why it was refused.
	cancelled struct {
		run string
		err error
	}
	// replayed is the run a replay started, or why it was refused.
	replayed struct {
		from, run string
		err       error
	}
)

// mayRun says whether the caller holds workflow:run on the run shown, which cancelling and
// replaying ask; the keys of whoever does not are left out rather than refused.
func (m Model) mayRun() bool {
	return m.o.Send != nil && m.run != nil && m.me.holds("workflow:run", m.run.Namespace, m.run.Workflow)
}

// mayCancel says whether c is offered: a run going on, to whoever may run its workflow.
func (m Model) mayCancel() bool { return m.mayRun() && !m.run.State.Terminal() }

// mayReplay says whether p is offered: a run over, replayable from a step, to whoever may run its
// workflow. A run whose purge took an input a step would restart from replays from its start alone,
// which the web console offers and p does not.
func (m Model) mayReplay() bool {
	return m.mayRun() && m.run.State.Terminal() && !m.run.ReplayFromStartOnly && stepOf(m.run, m.step) != ""
}

// question is the prompt open, naming what it does to which run.
func (m Model) question() string {
	r := m.run
	switch m.asking {
	case askingCancel:
		return "Cancel run " + string(r.Run) + " of " + r.Namespace + "/" + r.Workflow + "? Its tasks in flight are stopped, and it ends cancelled."
	case askingReplay:
		return "Replay run " + string(r.Run) + " from " + stepOf(r, m.step) + "? A new run of " + short(r.Commit) + " starts there, the steps above it reused."
	}
	return ""
}

// answer does what the prompt open asks where the key is y, and closes it whatever the key.
func (m Model) answer(key string) (tea.Model, tea.Cmd) {
	was := m.asking
	m.asking = notAsking
	if key != "y" || m.run == nil {
		return m, nil
	}
	run, send, ctx := string(m.run.Run), m.o.Send, m.ctx
	m.acted, m.problem = "", ""
	switch was {
	case askingCancel:
		return m, func() tea.Msg {
			return cancelled{run: run, err: send(ctx, "POST", "/api/v1/runs/"+url.PathEscape(run)+"/cancel", nil, nil)}
		}
	case askingReplay:
		from := stepOf(m.run, m.step)
		return m, func() tea.Msg {
			var started struct {
				Run agk.RunID `json:"run"`
			}
			err := send(ctx, "POST", "/api/v1/runs/"+url.PathEscape(run)+"/replay", map[string]string{"step": from}, &started)
			return replayed{from: from, run: string(started.Run), err: err}
		}
	}
	return m, nil
}

// cancelledRun says the cancel was asked, or why it was refused, and reads the run again to show
// it ending.
func (m Model) cancelledRun(msg cancelled) (tea.Model, tea.Cmd) {
	if m.view != runView || m.run == nil || string(m.run.Run) != msg.run {
		return m, nil
	}
	if msg.err != nil {
		m.problem = "The run could not be cancelled: " + said(msg.err)
		return m, nil
	}
	m.acted = "Cancelling was asked: the controller stops the tasks in flight, and the run ends cancelled."
	return m, m.readShown()
}

// replayedRun opens the run a replay started, or says why the replay was refused.
func (m Model) replayedRun(msg replayed) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.problem = "The run could not be replayed from " + msg.from + ": " + said(msg.err)
		return m, nil
	}
	m = m.unfollow()
	m.run, m.runRead, m.runFailed, m.step, m.port, m.payloads = nil, false, "", "", 0, nil
	m.acted, m.problem = "", ""
	m.recent, m.recentFor = nil, ""
	m.selected, m.view = msg.run, runView
	m.shown++
	return m, m.readShown()
}
