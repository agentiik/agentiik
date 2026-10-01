package console

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// The depth is read as the documentation's table reads it, and nothing else.
func TestTheDepthIsReadFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		vars    map[string]string
		want    depth
		profile colorprofile.Profile
	}{
		{map[string]string{"COLORTERM": "truecolor", "TERM": "xterm-256color"}, trueColour, colorprofile.TrueColor},
		{map[string]string{"COLORTERM": "24bit", "TERM": "xterm"}, trueColour, colorprofile.TrueColor},
		{map[string]string{"TERM": "xterm-256color"}, twoFiftySix, colorprofile.ANSI256},
		{map[string]string{"TERM": "screen-256color", "COLORTERM": "yes"}, twoFiftySix, colorprofile.ANSI256},
		{map[string]string{"TERM": "xterm"}, sixteen, colorprofile.ANSI},
		{map[string]string{"TERM": "linux"}, sixteen, colorprofile.ANSI},
		{map[string]string{}, sixteen, colorprofile.ANSI},
		{map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, noColour, colorprofile.Ascii},
		{map[string]string{"NO_COLOR": "", "TERM": "xterm-256color"}, twoFiftySix, colorprofile.ANSI256},
	} {
		got := depthOf(env(c.vars))
		if got != c.want || got.profile() != c.profile {
			t.Errorf("%v reads as depth %d, profile %s, where it is %d, %s", c.vars, got, got.profile(), c.want, c.profile)
		}
	}
}

// Each depth draws a state as the documentation's table gives it, and paints the ground under the
// whole line at 256 colours and above alone.
func TestEachDepthDrawsTheStatesAsTheTableSays(t *testing.T) {
	for _, c := range []struct {
		theme       theme
		running     string
		ground      string
		noGroundSet bool
	}{
		{theme{depth: trueColour}, "38;2;104;174;245", "48;2;18;18;18", false},
		{theme{depth: trueColour, light: true}, "38;2;1;93;162", "48;2;241;241;241", false},
		{theme{depth: twoFiftySix}, "38;5;75", "48;5;233", false},
		{theme{depth: twoFiftySix, light: true}, "38;5;25", "48;5;255", false},
		{theme{depth: sixteen}, "34", "", true},
		{theme{depth: noColour}, "", "", true},
	} {
		line := c.theme.line(false, 10, part{runningText, "running"})
		if c.running != "" && !strings.Contains(line, "\x1b["+c.running) {
			t.Errorf("depth %d, light %v: running is drawn %q, not in %s", c.theme.depth, c.theme.light, line, c.running)
		}
		if c.ground != "" && strings.Count(line, c.ground) != 2 {
			t.Errorf("depth %d, light %v: the ground %s is not painted under the word and the padding: %q", c.theme.depth, c.theme.light, c.ground, line)
		}
		if c.noGroundSet && strings.Contains(line, "48;") {
			t.Errorf("depth %d paints a ground: %q", c.theme.depth, line)
		}
		if c.theme.depth == noColour && strings.Contains(line, "\x1b[") {
			t.Errorf("NO_COLOR draws a colour: %q", line)
		}
	}
}

// Without colour, what is absent is faint and the selection reversed, so that NO_COLOR loses
// nothing a person reads.
func TestWithoutColourFaintAndReverseStillMark(t *testing.T) {
	th := theme{depth: noColour}
	if got := th.line(false, 2, part{quiet, "ab"}); got != "\x1b[2mab\x1b[m" {
		t.Errorf("what is absent is drawn %q without colour", got)
	}
	if got := th.line(true, 3, part{plain, "ab"}); !strings.HasPrefix(got, "\x1b[7m") {
		t.Errorf("the selection is drawn %q without colour", got)
	}
	if got := th.line(false, 2, part{strong, "ab"}); got != "\x1b[1mab\x1b[m" {
		t.Errorf("a heading is drawn %q without colour", got)
	}
}

// The selection is the accent's dim fill in true colour, and accentLine's nearest at 256, since
// accentDim's is a grey beside the ground's.
func TestTheSelectionIsSeenAtEveryDepth(t *testing.T) {
	if got := (theme{depth: trueColour}).line(true, 2, part{plain, "ab"}); !strings.Contains(got, "48;2;15;43;70") {
		t.Errorf("the selection in true colour is drawn %q", got)
	}
	dark, light := (theme{depth: twoFiftySix}).line(true, 2, part{plain, "ab"}), (theme{depth: twoFiftySix, light: true}).line(true, 2, part{plain, "ab"})
	if !strings.Contains(dark, "48;5;24") || !strings.Contains(light, "48;5;153") {
		t.Errorf("the selection at 256 is drawn %q dark and %q light", dark, light)
	}
}

// The ground follows the terminal's answer, given a tenth of a second: dark where none comes, and
// an answer after it is left, so that the screen does not flash.
func TestTheGroundFollowsTheTerminalsAnswer(t *testing.T) {
	truecolour := env(map[string]string{"COLORTERM": "truecolor"})
	light := tea.BackgroundColorMsg{Color: color.RGBA{0xFA, 0xFA, 0xFA, 0xFF}}
	dark := tea.BackgroundColorMsg{Color: color.RGBA{0x10, 0x10, 0x10, 0xFF}}

	m := opened(t, &installation{runs: someRuns()}, Options{Getenv: truecolour}, 100, 30)
	if m.settled || m.light {
		t.Fatalf("a console whose terminal has not answered is settled %v, light %v", m.settled, m.light)
	}
	if got := press(t, m); got.light {
		t.Fatal("a console is light before any answer")
	}
	var tm tea.Model = m
	tm, _ = tm.Update(light)
	if !tm.(Model).light || !strings.Contains(tm.(Model).screen(), "48;2;241;241;241") {
		t.Error("a light background does not draw the light ground")
	}

	tm, _ = tea.Model(m).Update(silent{})
	tm, _ = tm.Update(light)
	if tm.(Model).light {
		t.Error("an answer after the tenth of a second turns the screen light")
	}

	tm, _ = tea.Model(m).Update(dark)
	if tm.(Model).light || !strings.Contains(tm.(Model).screen(), "48;2;18;18;18") {
		t.Error("a dark background does not draw the dark ground")
	}
}

// AGENTIIK_THEME settles the ground from the start, and nothing is asked of the terminal; nor is
// it where no ground is painted.
func TestAThemeNamedOrNoGroundAsksNothing(t *testing.T) {
	asks := func(o Options) bool {
		m := New(t.Context(), o)
		m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
		return !m.settled
	}
	truecolour := env(map[string]string{"COLORTERM": "truecolor"})
	if asks(Options{Getenv: truecolour, Theme: "light"}) || asks(Options{Getenv: truecolour, Theme: "dark"}) {
		t.Error("a console told its theme asks the terminal for its background")
	}
	if asks(Options{Getenv: env(map[string]string{"TERM": "xterm"})}) || asks(Options{Getenv: env(map[string]string{"NO_COLOR": "1"})}) {
		t.Error("a console that paints no ground asks the terminal for its background")
	}
	if m := New(t.Context(), Options{Getenv: truecolour, Theme: "light"}); !m.light {
		t.Error("AGENTIIK_THEME=light does not draw the light ground")
	}
}
