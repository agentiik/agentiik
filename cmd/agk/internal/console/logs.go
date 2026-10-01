package console

import (
	"bytes"
	"context"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// The inspector follows the log of the step chosen, history then live, as agk logs follows it: the
// command line hands the console its own follower, so that a log read on a screen is resumed after
// a cut, given up on and refused exactly as one agk logs prints, rather than read by a second
// implementation of the stream that would drift from the first.

// Follower follows one step's log until it is over or ctx is done, writing each line to out as agk
// logs prints it, and what is said about the log rather than in it to said: a gap, a log the
// runner cut, a stream being asked again, a refusal. It answers nil where it read the log to its
// end, and an error where it stopped short of it, which said has explained.
type Follower func(ctx context.Context, run, step string, out, said io.Writer) error

// logKept is how many lines of a step's log the inspector keeps: the end is what a screen shows,
// and the whole log is agk logs' to print.
const logKept = 2000

// logLine is one line of a log, or one said about it.
type logLine struct {
	text string
	said bool
}

// logBook is what the inspector holds of one step's log, and whether it was read to its end, after
// which it is not asked for again.
type logBook struct {
	lines []logLine
	over  bool
}

// following is the log being followed now, of one step of one run.
type following struct {
	key    string
	cancel context.CancelFunc
	lines  chan tea.Msg
	start  sync.Once
	follow func()
}

type (
	// logRead is one line of the log followed.
	logRead struct {
		key  string
		line logLine
	}
	// logEnded is the follower returned, the log read to its end where over says so.
	logEnded struct {
		key  string
		over bool
	}
)

// next waits for the next line of the log, the follower started with the first wait so that a
// command never begun leaves nothing running.
func (f *following) next() tea.Cmd {
	return func() tea.Msg {
		f.start.Do(func() { go f.follow() })
		msg, ok := <-f.lines
		if !ok {
			return logEnded{key: f.key}
		}
		return msg
	}
}

// followChosen follows the log of the step chosen, unless it is followed already or was read to
// its end, and stops following any other. A log stopped short of its end is followed again where
// the step is chosen again, and not each time the run is read again, which would ask a refused
// log again every few seconds.
func (m Model) followChosen(again bool) (Model, tea.Cmd) {
	if m.o.Follow == nil || m.run == nil || m.view != runView {
		return m, nil
	}
	run, step := string(m.run.Run), stepOf(m.run, m.step)
	key := run + "/" + step
	if m.follow != nil && m.follow.key == key {
		return m, nil
	}
	m = m.unfollow()
	if b, ok := m.logs[key]; ok && (b.over || !again) {
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	f := &following{key: key, cancel: cancel, lines: make(chan tea.Msg, 64)}
	follow := m.o.Follow
	f.follow = func() {
		err := follow(ctx, run, step, &lineWriter{ctx: ctx, key: key, to: f.lines}, &lineWriter{ctx: ctx, key: key, said: true, to: f.lines})
		if ctx.Err() == nil {
			select {
			case f.lines <- logEnded{key: key, over: err == nil}:
			case <-ctx.Done():
			}
		}
		close(f.lines)
	}
	if m.logs == nil {
		m.logs = map[string]logBook{}
	}
	// A log followed again is sent again from its start, its history first.
	m.logs[key] = logBook{}
	m.follow = f
	return m, f.next()
}

// unfollow stops following the log followed, whose follower returns once its context is done.
func (m Model) unfollow() Model {
	if m.follow != nil {
		m.follow.cancel()
		m.follow = nil
	}
	return m
}

// kept keeps a line of the log followed, and waits for the next; a line of a log no longer
// followed is left.
func (m Model) kept(msg logRead) (Model, tea.Cmd) {
	if m.follow == nil || msg.key != m.follow.key {
		return m, nil
	}
	b := m.logs[msg.key]
	b.lines = append(b.lines, msg.line)
	if len(b.lines) > logKept {
		b.lines = append([]logLine(nil), b.lines[len(b.lines)-logKept:]...)
	}
	m.logs[msg.key] = b
	return m, m.follow.next()
}

// ended is the follower returned: the log is no longer followed, and is not asked for again where
// it was read to its end. One stopped short of it is followed again when its step is chosen again.
func (m Model) ended(msg logEnded) Model {
	if m.follow == nil || msg.key != m.follow.key {
		return m
	}
	b := m.logs[msg.key]
	b.over = msg.over
	m.logs[msg.key] = b
	m.follow.cancel()
	m.follow = nil
	return m
}

// lineWriter hands the console each line written to it, whole, as the follower writes them.
type lineWriter struct {
	ctx  context.Context
	key  string
	said bool
	to   chan<- tea.Msg
	rest []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.rest = append(w.rest, p...)
	for {
		i := bytes.IndexByte(w.rest, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := logRead{key: w.key, line: logLine{text: string(w.rest[:i]), said: w.said}}
		w.rest = w.rest[i+1:]
		select {
		case w.to <- line:
		case <-w.ctx.Done():
			return len(p), w.ctx.Err()
		}
	}
}

// logLines is the log of the step chosen as the inspector draws it, its header and then as many of
// its last lines as height holds.
func (m Model) logLines(t theme, step string, height int) []string {
	if height <= 0 {
		return nil
	}
	key := string(m.run.Run) + "/" + step
	b := m.logs[key]
	header := []part{{quiet, "LOG"}, {plain, " " + step}}
	switch {
	case m.follow != nil && m.follow.key == key:
		header = append(header, part{plain, "  "}, part{succeededText, "●"}, part{plain, " live"})
	case b.over:
		header = append(header, part{muted, "  read to its end"})
	}
	lines := []string{t.line(false, m.width, within(header, m.width)...)}
	if len(b.lines) == 0 {
		said := "Waiting for the log."
		if b.over {
			said = "This step wrote no log."
		}
		return append(lines, t.line(false, m.width, part{quiet, said}))
	}
	shown := b.lines[max(0, len(b.lines)-(height-1)):]
	for _, l := range shown {
		r := plain
		if l.said {
			r = quiet
		}
		lines = append(lines, t.line(false, m.width, within([]part{{r, l.text}}, m.width)...))
	}
	return lines
}
