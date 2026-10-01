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
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/access"
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

	// Send sends what cancels or replays a run, and is nil where the console only reads.
	Send Sender

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
	graphView
	workflowsView
	sharingView
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

	// The runs' filter: whether its line is open, what was typed, how many keys were, the filter
	// as it stood when typing last paused, whose terms the runs are asked with, and why the
	// installation refused them.
	filtering     bool
	filter        string
	keys          int
	terms         string
	filterRefused string

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

	// asking is the prompt open over the run, and acted and problem what the last thing asked of
	// it came to: done, or refused and why.
	asking  asking
	acted   string
	problem string

	// spinner turns beside what is going on, while spinning says its next frame is asked for;
	// recent is the last twenty runs of the run's workflow, read for recentFor.
	spinner   spinner.Model
	spinning  bool
	recent    []db.ListedRun
	recentFor string

	// sparks are the last runs of each workflow the runs view lists, by namespace/workflow.
	sparks map[string]sparkRead

	// seen are the notifications GET /api/v1/me has held since the console opened, and toasts
	// those shown now.
	seen   map[string]bool
	toasts []toast

	// palette is the command palette open, if any, and used the commands chosen in it, the last
	// first, which it opens on.
	palette *palette
	used    []string

	// clicked is what the last click landed on and clickedAt when, which tell a second click on
	// the same from a first.
	clicked   pick
	clickedAt time.Time

	// The panes': the one in focus, where a border was dragged and the one being dragged, and how
	// far the log of logBackOf is scrolled back from its end.
	focus     pane
	split     split
	dragging  string
	logBack   int
	logBackOf string

	// framed is how many columns the frame of the pane a view is drawn in takes, which the view's
	// widths count back: four inside a pane, none for a view that is the whole window.
	framed int

	// The workflows view's: the workflows the runs read name, the one chosen, and what was read of
	// each beyond its row. graphOf is the workflow whose graph is shown with no run laid over it.
	flows       []flowRow
	flowsRead   bool
	flowsFailed string
	flow        string
	flowDetails map[string]flowDetailRead
	flowStatsOf map[string]flowStatsRead
	graphOf     string

	// The graph view's: the graph of the run's workflow, read for graphFor, or why it could not be;
	// the view it was opened from, which esc goes back to; and whether it is written as a list.
	graph       *flowGraph
	graphFor    string
	graphFailed string
	graphFrom   view
	asList      bool

	// The sharing view's: the scope whose grants are listed, the grants as last read and the scope
	// they were read for, why they could not be, the members of every group where an administrator
	// reads them, the grant chosen, the principal they are resolved for, and the filter over them
	// and whether its line is open.
	scope           string
	grants          []access.Grant
	grantsFor       string
	grantsFailed    string
	members         map[string][]string
	grant           string
	whom            string
	grantsFilter    string
	grantsFiltering bool

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
	m := Model{ctx: ctx, o: o, tick: tea.Tick, depth: depthOf(o.Getenv), spinner: newSpinner()}
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
		m.view, m.selected, m.focus = runView, o.Run, runPane
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
		runs  []db.ListedRun
		err   error
		shown int
	}
	runRead struct {
		run *db.RunDetail
		err error
		// want is the run asked for, so that a read of a run since left for another is dropped.
		want string
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
	if m.view == workflowsView {
		return m.readFlows()
	}
	if m.view == sharingView {
		return m.readSharing()
	}
	if m.view == graphView && m.graphOf != "" {
		return m.readWorkflowNamed(m.graphOf)
	}
	if m.view == runView || m.view == graphView {
		// The run open, which the runs' pane beside it moves the selection away from.
		run := m.openRun()
		read := func() tea.Msg {
			var d db.RunDetail
			if err := m.o.Read(m.ctx, "/api/v1/runs/"+url.PathEscape(run), &d); err != nil {
				return runRead{err: err, want: run}
			}
			return runRead{run: &d, want: run}
		}
		if m.view == runView {
			// The runs' pane is beside the run, or folded above it, and kept live as it is alone.
			return tea.Batch(read, m.readRuns())
		}
		return read
	}
	return m.readRuns()
}

// readRuns reads the runs the runs' pane lists, with the filter's terms.
func (m Model) readRuns() tea.Cmd {
	path := "/api/v1/runs?limit=100"
	// A namespace the filter names is the one asked for, in place of the one the console opened on.
	terms := parse(m.terms)
	if _, named := terms.terms["namespace"]; m.o.Namespace != "" && !named {
		path += "&namespace=" + url.QueryEscape(m.o.Namespace)
	}
	// since=24h is read back from now at each read, so that the runs kept live keep the last day.
	if q := terms.query(m.o.Now()); q != "" {
		path += "&" + q
	}
	shown := m.shown
	return func() tea.Msg {
		var listed struct {
			Runs []db.ListedRun `json:"runs"`
		}
		err := m.o.Read(m.ctx, path, &listed)
		return runsRead{runs: listed.Runs, err: err, shown: shown}
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
	if frame, ok := msg.(spinner.TickMsg); ok {
		return m.turned(frame)
	}
	next, cmd := m.update(msg)
	n := next.(Model)
	if !n.spinning && n.live() {
		n.spinning = true
		cmd = tea.Batch(cmd, n.nextFrame())
	}
	return n, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		if msg.err != nil {
			// Asked again half a minute later, as the runs are.
			return m, m.tick(meEvery, func(time.Time) tea.Msg { return meAgain{} })
		}
		m.me = msg.me
		var cmds []tea.Cmd
		m, cmds = m.noticed(msg.me)
		if m.me.Admin && m.view == runView && m.runners == nil {
			cmds = append(cmds, m.readRunners())
		}
		if m.view == sharingView && m.scope == "" {
			// Opened before the principal was read, the view learns only now what it may list.
			if m.scope = m.sharingScope(); m.scope != "" {
				cmds = append(cmds, m.readSharing())
			}
		}
		return m, tea.Batch(append(cmds, m.readChosen())...)
	case meAgain:
		return m, m.readMe()
	case toastGone:
		return m.gone(msg.id), nil
	case runnersRead:
		m.runners = msg.runners
	case payloadRead:
		if m.payloads == nil {
			m.payloads = map[string]payloadRead{}
		}
		m.payloads[msg.key] = msg
	case runsRead:
		if msg.shown != m.shown {
			// Asked for with terms since changed, or by a view since left.
			return m, nil
		}
		var toasts []tea.Cmd
		switch {
		case msg.err != nil && len(parse(m.terms).terms) > 0:
			m.filterRefused = "The runs could not be read with these terms: " + said(msg.err)
		case msg.err != nil:
			m.unanswered = said(msg.err)
		default:
			m, toasts = m.runsEnded(m.runs, msg.runs)
			m.unanswered, m.filterRefused, m.runs, m.read = "", "", msg.runs, true
			// A run being opened is named by the selection until it is read, which a list that
			// does not hold it leaves alone.
			opening := m.view == runView && m.run == nil
			if shown := m.shownRuns(); !opening && !slices.ContainsFunc(shown, func(r db.ListedRun) bool { return string(r.Run) == m.selected }) && len(shown) > 0 {
				m.selected = string(shown[0].Run)
			}
		}
		if m.view == runsView {
			toasts = append(append(toasts, m.readSparks()...), m.later())
		}
		return m, tea.Batch(toasts...)
	case sparkRead:
		if m.sparks == nil {
			m.sparks = map[string]sparkRead{}
		}
		m.sparks[msg.key] = msg
	case runRead:
		if msg.want != "" && msg.want != m.openRun() {
			return m, nil
		}
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
		if m.run != nil && m.recentFor != string(m.run.Run) {
			m.recentFor = string(m.run.Run)
			cmds = append(cmds, m.readRecent())
		}
		if m.view == graphView && m.run != nil && m.graphFor != workflowKey(m.run) {
			m.graphFor, m.graph, m.graphFailed = workflowKey(m.run), nil, ""
			cmds = append(cmds, m.readWorkflow())
		}
		// A run is read again while it goes on, and the runs beside it always.
		if m.view == runView || m.view == graphView && (m.run == nil || !m.run.State.Terminal()) {
			cmds = append(cmds, m.later())
		}
		if m.view == runView && m.me.Admin && m.runners == nil {
			cmds = append(cmds, m.readRunners())
		}
		cmds = append(cmds, m.readChosen())
		return m, tea.Batch(cmds...)
	case flowsRead:
		if msg.shown != m.shown {
			return m, nil
		}
		if msg.err != nil {
			if m.flowsRead {
				m.unanswered = said(msg.err)
			} else {
				m.flowsFailed = said(msg.err)
			}
		} else {
			m.unanswered, m.flowsFailed, m.flowsRead, m.flows = "", "", true, flowRows(msg.runs)
			if f, ok := m.chosenFlow(); ok {
				m.flow = f.key()
			}
		}
		if m.view == workflowsView {
			return m, tea.Batch(append(m.readChosenFlow(), m.later())...)
		}
	case flowDetailRead:
		if m.flowDetails == nil {
			m.flowDetails = map[string]flowDetailRead{}
		}
		m.flowDetails[msg.key] = msg
	case flowStatsRead:
		if m.flowStatsOf == nil {
			m.flowStatsOf = map[string]flowStatsRead{}
		}
		m.flowStatsOf[msg.key] = msg
	case workflowRead:
		if msg.key == m.graphFor {
			if msg.err != nil {
				m.graphFailed = said(msg.err)
			} else {
				m.graph, m.graphFailed = msg.graph, ""
			}
		}
	case recentRead:
		if m.run != nil && msg.run == string(m.run.Run) {
			m.recent = msg.runs
		}
	case cancelled:
		return m.cancelledRun(msg)
	case replayed:
		return m.replayedRun(msg)
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
	case sharingRead:
		if msg.scope != m.scope {
			// Read for a scope since left.
			return m, nil
		}
		if msg.err != nil {
			if m.grantsFor == m.scope {
				m.unanswered = said(msg.err)
			} else {
				m.grantsFailed = said(msg.err)
			}
		} else {
			m.unanswered, m.grantsFailed, m.grantsFor, m.grants = "", "", msg.scope, msg.grants
			if msg.members != nil {
				m.members = msg.members
			}
		}
		if m.view == sharingView {
			return m, m.later()
		}
	case again:
		if msg.view == m.view && msg.shown == m.shown {
			return m, m.readShown()
		}
	case typedPaused:
		return m.paused(msg)
	case tea.MouseMsg:
		return m.mouse(msg)
	case tea.KeyPressMsg:
		if m.palette != nil && m.asking == notAsking {
			return m.typingCommand(msg)
		}
		if m.filtering && m.view == runsView && m.asking == notAsking && !m.listing {
			return m.typing(msg)
		}
		if m.grantsFiltering && m.view == sharingView && !m.listing {
			return m.typingGrants(msg)
		}
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
	if m.asking != notAsking {
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		return m.answer(key)
	}
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
	case ":":
		m.palette = &palette{}
		return m, nil
	}
	switch key {
	case "1":
		if m.view != runsView {
			return m.showing(runsView)
		}
		return m, nil
	case "2":
		if m.view != workflowsView {
			return m.showing(workflowsView)
		}
		return m, nil
	case "3":
		if m.view != sharingView {
			m.scope = m.sharingScope()
			return m.showing(sharingView)
		}
		return m, nil
	case "4":
		// The runners are an administrator's alone, and the key is nobody else's either.
		if m.me.Admin && m.view != runnersView {
			return m.showing(runnersView)
		}
		return m, nil
	}
	if m.view == sharingView {
		return m.sharingPress(key)
	}
	if m.view == workflowsView {
		switch key {
		case "esc":
			return m.showing(runsView)
		case "up", "k", "down", "j":
			if f, ok := m.chosenFlow(); ok {
				i := slices.IndexFunc(m.flows, func(r flowRow) bool { return r.key() == f.key() })
				if key == "up" || key == "k" {
					i = max(0, i-1)
				} else {
					i = min(len(m.flows)-1, i+1)
				}
				m.flow = m.flows[i].key()
				return m, tea.Batch(m.readChosenFlow()...)
			}
		case "enter", "g":
			if f, ok := m.chosenFlow(); ok {
				m.graphOf, m.graphFrom, m.step = f.key(), workflowsView, ""
				return m.showing(graphView)
			}
		}
		return m, nil
	}
	if m.view == graphView {
		switch key {
		case "esc":
			return m.showing(m.graphFrom)
		case "g":
			m.asList = !m.asList
		case "up", "k", "down", "j":
			if m.graph != nil && len(m.graph.Order) > 0 {
				i := slices.Index(m.graph.Order, m.graphStep())
				if key == "up" || key == "k" {
					i = max(0, i-1)
				} else {
					i = min(len(m.graph.Order)-1, i+1)
				}
				m.step, m.port = m.graph.Order[i], 0
			}
		case "left", "h", "right", "l":
			if m.graph != nil && len(m.graph.Order) > 0 {
				if next := m.graph.along(m.graphStep(), key == "right" || key == "l"); next != "" {
					m.step, m.port = next, 0
				}
			}
		case "enter":
			if m.run != nil {
				m.step = m.graphStep()
				return m.showing(runView)
			}
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
		if next, cmd, done := m.panePress(key); done {
			return next, cmd
		}
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
		case "g":
			if m.run != nil {
				m.graphFrom = runView
				return m.showing(graphView)
			}
		case "c":
			if m.mayCancel() {
				m.asking = askingCancel
			}
		case "p":
			if m.mayReplay() {
				m.asking = askingReplay
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
		m.selected = moved(m.shownRuns(), m.selected, -1)
	case "down", "j":
		m.selected = moved(m.shownRuns(), m.selected, 1)
	case "/":
		m.filtering = true
	case "g":
		if m.selected != "" {
			m.graphFrom = runsView
			return m.showing(graphView)
		}
	case "esc":
		if m.filter != "" {
			return m.filtered("")
		}
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
	// The run and its graph are one run seen two ways, and turning between them keeps it; any
	// other view forgets it, and it is read again once opened again.
	if m.view == runView {
		m = m.unfollow()
	}
	ofRun := func(v view) bool { return v == runView || v == graphView }
	if ofRun(m.view) && !ofRun(v) {
		m = m.forgetRun()
	}
	if v == runView && m.view != runView {
		m.focus = runPane
	}
	if v != graphView {
		m.graphOf = ""
	}
	if v == sharingView && m.grantsFor != m.scope {
		m.grants, m.grantsFor, m.grantsFailed = nil, "", ""
	}
	if m.graphOf != "" && m.graphFor != m.graphOf {
		m.graph, m.graphFor, m.graphFailed = nil, m.graphOf, ""
	}
	m.view = v
	m.shown++
	if v == runsView {
		// A filter changed and left before typing paused is asked for on coming back.
		m.terms = m.filter
	}
	return m, m.readShown()
}

// openRun is the run open, or being opened: the one read, and the selection until it is.
func (m Model) openRun() string {
	if m.run != nil {
		return string(m.run.Run)
	}
	return m.selected
}

// forgetRun forgets the run open, which is read again once opened again.
func (m Model) forgetRun() Model {
	m.run, m.runRead, m.runFailed, m.step, m.port, m.payloads = nil, false, "", "", 0, nil
	m.asking, m.acted, m.problem = notAsking, "", ""
	m.recent, m.recentFor = nil, ""
	return m
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
