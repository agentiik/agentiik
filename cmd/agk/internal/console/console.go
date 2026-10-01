// Package console is agk console: the web console's views in a terminal, drawn over /api/v1 as
// the principal agk is signed in as, with no server, route or permission of its own.
//
// It is the one package of the module that draws a screen, and the Charm toolkit it draws with,
// Bubble Tea and Lip Gloss, stays inside cmd/agk: the tests that hold the graph evaluator and the
// container driver to no server, no bus and no database refuse them any Charm package too, so that
// agk run --local stays the code path it was.
package console

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// Reader reads one route of the API into out, as agk's other verbs read it: the installation, the
// credential and the refusals are the command line's, and the console knows none of them.
type Reader func(ctx context.Context, path string, out any) error

// Options are what the command line opens the console with.
type Options struct {
	// Installation is what the top line names the installation by.
	Installation string
	// Namespace narrows the runs to one namespace, and is empty for every one the principal reads.
	Namespace string
	// Run is the run the console opens on, and is empty for the runs view.
	Run string

	Read Reader
	Now  func() time.Time

	// Follow follows a step's log as agk logs does, and is nil where no log is to be shown.
	Follow Follower

	// Every is how often what is shown is read again, so that a run going on is seen going: five
	// seconds where it is not set, as the web console reads its runs and a run until it ends.
	Every time.Duration

	// Getenv reads the variables the screen is drawn by: NO_COLOR, COLORTERM and TERM for how
	// many colours the terminal shows.
	Getenv func(string) string

	// Theme is AGENTIIK_THEME, light or dark, which settles the ground for a terminal that never
	// says what its background is, and is empty where the terminal is asked.
	Theme string
}

// Run draws the console until it is quit, and hands the screen back as it found it, an interrupt
// included.
func Run(ctx context.Context, o Options) error {
	m := New(ctx, o)
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithColorProfile(m.depth.profile())).Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// The size under which the console asks for more room: what a terminal opens at, and what every
// view is laid out to fit.
const (
	leastWidth  = 80
	leastHeight = 24
)

type view int

const (
	runsView view = iota
	runView
	runnersView
)

// Model is the console's state, as Bubble Tea holds it between one message and the next.
type Model struct {
	ctx context.Context
	o   Options

	width, height int

	me principal

	view     view
	runs     []db.ListedRun
	read     bool
	selected string
	top      int

	run       *db.RunDetail
	runRead   bool
	runFailed string

	// step and port are the inspector's choice: the step shown, and which of its ports.
	step     string
	port     int
	payloads map[string]payloadRead
	runners  map[string]db.Runner

	// logs are the logs of the steps chosen, by run and step, and follow the one followed now.
	logs   map[string]logBook
	follow *following

	// The runners view's: the pools and the runners as last read, and the runner chosen.
	pools       []api.Pool
	fleet       []db.Runner
	fleetRead   bool
	fleetFailed string
	runner      string

	// shown counts the views shown, so that a read again asked for by a view since left, and come
	// back to, is told from the one that view asks for now.
	shown int

	// unanswered is why the last read failed, said in the top line until a read succeeds.
	unanswered string
	listing    bool

	// tick is tea.Tick, which a test replaces with one that never fires, so that it reads what
	// the console shows without waiting for it to be read again.
	tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

	// depth is how many colours the terminal shows, and light whether the ground is the light
	// one, which settled says is final: once the terminal has answered, once a tenth of a second
	// has passed without an answer, or from the start where AGENTIIK_THEME names it.
	depth   depth
	light   bool
	settled bool
}

// New is the console as it opens: on the run Options names, or on the runs.
func New(ctx context.Context, o Options) Model {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Every <= 0 {
		o.Every = 5 * time.Second
	}
	m := Model{ctx: ctx, o: o, tick: tea.Tick, depth: depthOf(o.Getenv)}
	switch o.Theme {
	case "light":
		m.light, m.settled = true, true
	case "dark":
		m.settled = true
	}
	if !m.theme().painted() {
		// The ground is painted at 256 colours and above alone, so nothing else asks for it.
		m.settled = true
	}
	if o.Run != "" {
		m.view, m.selected = runView, o.Run
	}
	return m
}

// What a read brings back, each as one message.
type (
	meRead struct {
		me  principal
		err error
	}
	runsRead struct {
		runs []db.ListedRun
		err  error
	}
	runRead struct {
		run *db.RunDetail
		err error
	}
	// again asks for what is shown to be read again, once Every has passed since the last read.
	again struct {
		view  view
		shown int
	}
	// silent is the tenth of a second a terminal is given to say what its background is, passed.
	silent struct{}
)

// answerWithin is how long the terminal is given to say what its background is, after which it
// is taken as dark, as most are: long enough for one over SSH, short enough not to be seen.
const answerWithin = 100 * time.Millisecond

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.readMe(), m.readShown()}
	if !m.settled {
		// The OSC 11 query Lip Gloss sends, its answer a tea.BackgroundColorMsg.
		cmds = append(cmds, tea.RequestBackgroundColor, m.tick(answerWithin, func(time.Time) tea.Msg { return silent{} }))
	}
	return tea.Batch(cmds...)
}

func (m Model) readMe() tea.Cmd {
	return func() tea.Msg {
		var me principal
		err := m.o.Read(m.ctx, "/api/v1/me", &me)
		return meRead{me: me, err: err}
	}
}

func (m Model) readShown() tea.Cmd {
	if m.view == runnersView {
		return m.readFleet()
	}
	if m.view == runView {
		run := m.selected
		return func() tea.Msg {
			var d db.RunDetail
			if err := m.o.Read(m.ctx, "/api/v1/runs/"+url.PathEscape(run), &d); err != nil {
				return runRead{err: err}
			}
			return runRead{run: &d}
		}
	}
	path := "/api/v1/runs?limit=100"
	if m.o.Namespace != "" {
		path += "&namespace=" + url.QueryEscape(m.o.Namespace)
	}
	return func() tea.Msg {
		var listed struct {
			Runs []db.ListedRun `json:"runs"`
		}
		err := m.o.Read(m.ctx, path, &listed)
		return runsRead{runs: listed.Runs, err: err}
	}
}

// later reads the view shown again once Every has passed, and only that view: a message for one
// left meanwhile is dropped when it comes, even where that view has been come back to, since it
// is read again from the moment it is.
func (m Model) later() tea.Cmd {
	v, shown := m.view, m.shown
	return m.tick(m.o.Every, func(time.Time) tea.Msg { return again{view: v, shown: shown} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		// An answer after the tenth of a second is left: the screen is drawn on the dark ground
		// by then, and turning it light would flash.
		if !m.settled {
			m.light, m.settled = !msg.IsDark(), true
		}
	case silent:
		m.settled = true
	case meRead:
		if msg.err == nil {
			m.me = msg.me
			if m.me.Admin && m.view == runView && m.runners == nil {
				return m, tea.Batch(m.readRunners(), m.readChosen())
			}
			return m, m.readChosen()
		}
	case runnersRead:
		m.runners = msg.runners
	case payloadRead:
		if m.payloads == nil {
			m.payloads = map[string]payloadRead{}
		}
		m.payloads[msg.key] = msg
	case runsRead:
		if msg.err != nil {
			m.unanswered = said(msg.err)
		} else {
			m.unanswered, m.runs, m.read = "", msg.runs, true
			if m.selected == "" && len(m.runs) > 0 {
				m.selected = string(m.runs[0].Run)
			}
		}
		if m.view == runsView {
			return m, m.later()
		}
	case runRead:
		if msg.err != nil {
			if m.run == nil {
				m.runFailed = said(msg.err)
			} else {
				m.unanswered = said(msg.err)
			}
		} else {
			m.unanswered, m.runFailed, m.run, m.runRead = "", "", msg.run, true
			m.step = stepOf(m.run, m.step)
		}
		var follow tea.Cmd
		m, follow = m.followChosen(false)
		cmds := []tea.Cmd{follow}
		if m.view == runView && (m.run == nil || !m.run.State.Terminal()) {
			cmds = append(cmds, m.later())
		}
		if m.view == runView && m.me.Admin && m.runners == nil {
			cmds = append(cmds, m.readRunners())
		}
		cmds = append(cmds, m.readChosen())
		return m, tea.Batch(cmds...)
	case logRead:
		return m.kept(msg)
	case logEnded:
		return m.ended(msg), nil
	case fleetRead:
		if msg.err != nil {
			if m.fleetRead {
				m.unanswered = said(msg.err)
			} else {
				m.fleetFailed = said(msg.err)
			}
		} else {
			m.unanswered, m.fleetFailed, m.fleetRead, m.pools, m.fleet = "", "", true, msg.pools, msg.runners
			m.runner = movedRunner(m.fleet, m.runner, 0)
			m.runners = make(map[string]db.Runner, len(m.fleet))
			for _, r := range m.fleet {
				m.runners[r.ID] = r
			}
		}
		if m.view == runnersView {
			return m, m.later()
		}
	case again:
		if msg.view == m.view && msg.shown == m.shown {
			return m, m.readShown()
		}
	case tea.KeyPressMsg:
		return m.press(msg.String())
	}
	return m, nil
}

// readChosen reads the envelope of the port chosen, once, where the caller may read payloads.
func (m Model) readChosen() tea.Cmd {
	if m.run == nil || !m.mayReadData() {
		return nil
	}
	ports := portsOf(summaryOf(m.run, m.step))
	if len(ports) == 0 {
		return nil
	}
	port := ports[min(m.port, len(ports)-1)]
	if _, ok := m.payloads[m.step+"/"+port]; ok {
		return nil
	}
	return m.readPayload(string(m.run.Run), m.step, port)
}

// movedStep is the step before or after the one chosen, staying at either end.
func movedStep(run *db.RunDetail, step string, by int) string {
	for i, s := range run.Steps {
		if string(s.Step) == step {
			return string(run.Steps[min(len(run.Steps)-1, max(0, i+by))].Step)
		}
	}
	return stepOf(run, "")
}

// press does what a key names in the view shown.
func (m Model) press(key string) (tea.Model, tea.Cmd) {
	if m.listing {
		if key == "?" || key == "esc" {
			m.listing = false
		}
		if key == "q" || key == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.listing = true
		return m, nil
	}
	switch key {
	case "1":
		if m.view != runsView {
			return m.showing(runsView)
		}
		return m, nil
	case "4":
		// The runners are an administrator's alone, and the key is nobody else's either.
		if m.me.Admin && m.view != runnersView {
			return m.showing(runnersView)
		}
		return m, nil
	}
	if m.view == runnersView {
		switch key {
		case "esc":
			return m.showing(runsView)
		case "up", "k":
			m.runner = movedRunner(m.fleet, m.runner, -1)
		case "down", "j":
			m.runner = movedRunner(m.fleet, m.runner, 1)
		}
		return m, nil
	}
	if m.view == runView {
		switch key {
		case "esc":
			return m.showing(runsView)
		case "up", "k", "down", "j":
			if m.run != nil {
				by := 1
				if key == "up" || key == "k" {
					by = -1
				}
				m.step, m.port = movedStep(m.run, m.step, by), 0
				var follow tea.Cmd
				m, follow = m.followChosen(true)
				return m, tea.Batch(m.readChosen(), follow)
			}
		case "[", "]":
			if m.run != nil {
				n := len(portsOf(summaryOf(m.run, m.step)))
				if key == "[" {
					m.port = max(0, m.port-1)
				} else {
					m.port = min(max(0, n-1), m.port+1)
				}
				return m, m.readChosen()
			}
		}
		return m, nil
	}
	switch key {
	case "up", "k":
		m.selected = moved(m.runs, m.selected, -1)
	case "down", "j":
		m.selected = moved(m.runs, m.selected, 1)
	case "enter":
		if m.selected != "" {
			return m.showing(runView)
		}
	}
	return m, nil
}

// showing is the console turned to another view, which is read at once: a run left behind is
// forgotten, and opened again from the runs.
func (m Model) showing(v view) (tea.Model, tea.Cmd) {
	if m.view == runView {
		m = m.unfollow()
		m.run, m.runRead, m.runFailed, m.step, m.port, m.payloads = nil, false, "", "", 0, nil
	}
	m.view = v
	m.shown++
	return m, m.readShown()
}

// moved is the run before or after the one selected, the first where none is, and the one
// selected again at either end of the list.
func moved(runs []db.ListedRun, selected string, by int) string {
	if len(runs) == 0 {
		return ""
	}
	for i, r := range runs {
		if string(r.Run) == selected {
			return string(runs[min(len(runs)-1, max(0, i+by))].Run)
		}
	}
	return string(runs[0].Run)
}

// said is a refusal or an unreachable installation as the top line says it, on one line.
func said(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
