package console

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// What a brick wrote, or whoever pushed a workflow named, reaches the terminal as what it says and
// nothing a terminal acts on: no sequence, no C1 control, no carriage return and no tab, and every
// line of the screen as wide as the window.

// hostile are the log lines of agentiik/agentiik#858 as agk logs' follower hands them to the
// console, and a last that says the log was drawn to its end.
var hostile = []string{
	"invoice | c1-csi \u009b2J c1-osc \u009d52;c;QzFDbGlw\u009c end",
	"invoice | \x1b]8;;https://evil.example/login\ahttps://agentiik.acme.example/runs\x1b]8;;\a",
	"invoice | spoof\r\x1b[32m● succeeded\x1b[0m",
	"invoice | a\tb\tc\td\t" + strings.Repeat("y", 120),
	"invoice | the end of what was written",
}

// terminal is what a program wrote to its terminal, read while it writes.
type terminal struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *terminal) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *terminal) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// The log of agentiik/agentiik#858, followed live by a console run by Bubble Tea as console.Run
// runs it, at the depths a terminal over SSH draws, writes none of its controls to the terminal:
// what they would have done is shown as their escapes, a link is its text, and the line holding
// tabs is cut inside its pane, whose border closes it.
func TestALogReachesTheTerminalAsTextAlone(t *testing.T) {
	for _, c := range []struct{ name, colorterm, term string }{
		{"true colour", "truecolor", "xterm-256color"},
		{"256 colours", "", "xterm-256color"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &stepLogs{lines: map[string][]string{"invoice": hostile}, live: map[string]bool{"invoice": true}}
			in := &installation{run: aFailedRun()}
			// Bubble Tea runs the commands of a batch at once, and the stand-in keeps what it was
			// asked.
			var asking sync.Mutex
			o := Options{
				Installation: "agentiik.example.com", Run: failedRun, Follow: logs.follow,
				Now: func() time.Time { return now },
				Read: func(ctx context.Context, path string, out any) error {
					asking.Lock()
					defer asking.Unlock()
					return in.read(ctx, path, out)
				},
				Getenv: func(k string) string { return map[string]string{"COLORTERM": c.colorterm, "TERM": c.term}[k] },
				// Settled, so that nothing waits on the answer to OSC 11.
				Theme: "dark",
			}
			m := New(t.Context(), o)
			var out terminal
			p := tea.NewProgram(m, tea.WithContext(t.Context()), tea.WithOutput(&out), tea.WithInput(nil),
				tea.WithWindowSize(160, 40), tea.WithColorProfile(m.depth.profile()), tea.WithoutSignals())
			go func() {
				deadline := time.Now().Add(5 * time.Second)
				for !strings.Contains(out.String(), "the end of what was written") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				p.Quit()
			}()
			final, err := p.Run()
			if err != nil {
				t.Fatal(err)
			}
			written := out.String()
			if !strings.Contains(written, "the end of what was written") {
				t.Fatalf("the log was not drawn to its end within five seconds:\n%q", written)
			}
			for _, raw := range []string{"\u009b", "\u009d", "\u009c", "\x1b]8;;https://evil.example", "\x1b]52", "spoof\r", "\t"} {
				if strings.Contains(written, raw) {
					t.Errorf("the terminal was written %q", raw)
				}
			}
			for _, shown := range []string{`c1-csi \u009b2J c1-osc \u009`, "invoice | https://agentiik.acme.exampl", `spoof\r● succeeded`} {
				if !strings.Contains(written, shown) {
					t.Errorf("the terminal was not written %q", shown)
				}
			}

			s := screen(final.(Model))
			tabs := -1
			for i, l := range strings.Split(s, "\n") {
				if w := lipgloss.Width(l); w != 160 {
					t.Errorf("line %d is %d columns wide in a window of 160: %q", i, w, l)
				}
				if strings.Contains(l, "invoice | a     b       c       d") {
					tabs = i
					if !strings.HasSuffix(l, "… │") {
						t.Errorf("the line holding tabs is not cut inside its pane: %q", l)
					}
				}
			}
			if tabs < 0 {
				t.Errorf("the line holding tabs is not drawn, its tabs to every eighth column:\n%s", s)
			}
		})
	}
}

// A toast says what a notification's fields and a run's name hold, and is drawn by Lip Gloss in
// its frame rather than by theme.line, as safe to draw.
func TestAToastReachesTheTerminalAsTextAlone(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 24)
	m, _ = m.toasted("run:x", "Run \u009b2J ended\r\x1b]0;pwned\a failed.")
	raw := m.screen()
	for _, c := range []string{"\u009b", "\x1b]", "\r"} {
		if strings.Contains(raw, c) {
			t.Errorf("the toast draws %q", c)
		}
	}
	s := screen(m)
	if !strings.Contains(s, `│ Run \u009b2J ended\r failed.`) {
		t.Errorf("the toast does not show its controls as their escapes:\n%s", s)
	}
	for i, l := range strings.Split(s, "\n") {
		if w := lipgloss.Width(l); w != 120 {
			t.Errorf("line %d is %d columns wide under a toast: %q", i, w, l)
		}
	}
}

// The runs as plain lines are one line a run in seven columns whatever a field holds, each field
// written as the screen draws it.
func TestPlainLinesKeepToTheirColumnsWhateverAFieldHolds(t *testing.T) {
	runs := []db.ListedRun{{RunSummary: db.RunSummary{
		Namespace: "fin\nance", Run: "01RUN\u009b2JAAAAAAAAAAAAAAAAA", Workflow: "monthly\tinvoicing",
		State: agk.Failed, Trigger: agk.TriggerManual, TriggeredBy: "alice\x1b]0;pwned\a\r", CreatedAt: now,
	}}}
	var b strings.Builder
	if err := Lines(&b, runs, now); err != nil {
		t.Fatal(err)
	}
	want := "failed\t" + `01RUN\u009b2JAAAAAAAAAAAAAAAAA` + "\t" + `fin\nance` + "/monthly invoicing\tmanual\t2026-09-25T12:00:00Z\t\t" + `alice\r` + "\n"
	if b.String() != want {
		t.Errorf("Lines wrote\n%q\nwhere\n%q", b.String(), want)
	}
}
