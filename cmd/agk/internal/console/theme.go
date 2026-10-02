package console

//go:generate go run ./palettegen ../../../../console/vendor/tokens.json palette.go

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/agentiik/agentiik/agk"
)

// token is one colour of the design system on one ground: its value, and its nearest of the 256.
type token struct {
	hex   string
	xterm int
}

// ground is every colour of the design system on one ground, by the token file's names.
type ground map[string]token

// depth is how many colours the terminal shows, read from the environment as the documentation's
// table reads it, and nothing else: a terminal guessed at from its name would draw differently on
// two machines that say the same thing about themselves.
type depth int

const (
	// noColour is NO_COLOR set and not empty: bold, faint and reverse alone.
	noColour depth = iota
	// sixteen is the terminal's own named colours, and no painted ground.
	sixteen
	// twoFiftySix is each token's nearest of the 256, on the nearest ground.
	twoFiftySix
	// trueColour is the palette's values, exactly, on the palette's ground.
	trueColour
)

func depthOf(getenv func(string) string) depth {
	if getenv == nil {
		return noColour
	}
	if getenv("NO_COLOR") != "" {
		return noColour
	}
	switch getenv("COLORTERM") {
	case "truecolor", "24bit":
		return trueColour
	}
	if strings.HasSuffix(getenv("TERM"), "256color") {
		return twoFiftySix
	}
	return sixteen
}

// profile is the depth as Bubble Tea's renderer takes it, so that it neither converts a colour
// the console chose nor guesses the depth again from heuristics of its own.
func (d depth) profile() colorprofile.Profile {
	switch d {
	case trueColour:
		return colorprofile.TrueColor
	case twoFiftySix:
		return colorprofile.ANSI256
	case sixteen:
		return colorprofile.ANSI
	}
	return colorprofile.Ascii
}

// theme is how the console draws: at what depth, and on which ground; and, while a click is being
// placed, where on the screen the lines it draws will sit and the book what a click lands on is
// written in.
type theme struct {
	depth depth
	light bool

	picks  *[]pick
	dx, dy int
}

// painted says whether the console paints the palette's ground under what it draws, which is
// what makes the contrast the design system chose hold whatever the terminal's own theme: with
// the palette's values or their nearest of the 256, and never with the terminal's sixteen, which
// its theme sets.
func (t theme) painted() bool { return t.depth >= twoFiftySix }

// colour is a token at the theme's depth, or nil where the depth draws none.
func (t theme) colour(name string) color.Color {
	g := darkGround
	if t.light {
		g = lightGround
	}
	tk, ok := g[name]
	if !ok {
		return nil
	}
	switch t.depth {
	case trueColour:
		return lipgloss.Color(tk.hex)
	case twoFiftySix:
		return lipgloss.Color(strconv.Itoa(tk.xterm))
	case sixteen:
		return named[name]
	}
	return nil
}

// named is the terminal's own colour for each token sixteen colours draw: a state's hue by its
// name, as the documentation's table gives it. The others are the terminal's default foreground.
var named = map[string]color.Color{
	"accent":    lipgloss.Blue,
	"running":   lipgloss.Blue,
	"succeeded": lipgloss.Green,
	"waiting":   lipgloss.Yellow,
	"failed":    lipgloss.Red,
}

// role is what a piece of text is to the person reading it, which the theme draws.
type role int

const (
	plain role = iota
	// strong is a heading or a key: bold.
	strong
	// muted is what is read second, such as a key's effect.
	muted
	// quiet is what is absent or settled into nothing: faint, or the palette's faint.
	quiet
	succeededText
	runningText
	waitingText
	failedText
)

// part is a piece of a line and what it is.
type part struct {
	role role
	text string
}

// line draws parts as one line as wide as the window, its padding painted as its parts are, and
// selected drawn as the selection: the accent's dim fill where the ground is painted, and reverse
// where it is not, which every terminal has.
func (t theme) line(selected bool, width int, parts ...part) string {
	var b strings.Builder
	used := 0
	for _, p := range parts {
		b.WriteString(t.style(selected, p.role).Render(p.text))
		used += lipgloss.Width(p.text)
	}
	if used < width {
		b.WriteString(t.style(selected, plain).Render(strings.Repeat(" ", width-used)))
	}
	return b.String()
}

func (t theme) style(selected bool, r role) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch {
	case selected && t.painted():
		// accentDim's nearest of the 256 is a grey a step or two from the ground's on either ground,
		// which no eye tells from it, so the 256 fill the selection with accentLine's, the first
		// blue of the cube that still carries the text.
		fill := "accentDim"
		if t.depth == twoFiftySix {
			fill = "accentLine"
		}
		s = s.Background(t.colour(fill)).Foreground(t.colour("text"))
	case selected:
		s = s.Reverse(true)
	case t.painted():
		s = s.Background(t.colour("bg")).Foreground(t.colour("text"))
	}
	switch r {
	case strong:
		s = s.Bold(true)
	case muted:
		if t.painted() {
			s = s.Foreground(t.colour("muted"))
		}
	case quiet:
		if t.painted() {
			s = s.Foreground(t.colour("faint"))
		} else {
			s = s.Faint(true)
		}
	case succeededText:
		s = t.hue(s, "succeeded")
	case runningText:
		s = t.hue(s, "running")
	case waitingText:
		s = t.hue(s, "waiting")
	case failedText:
		s = t.hue(s, "failed")
	}
	return s
}

func (t theme) hue(s lipgloss.Style, name string) lipgloss.Style {
	if c := t.colour(name); c != nil {
		return s.Foreground(c)
	}
	return s
}

// stateRole is the colour a run's state is drawn in, as the web console draws it: the accent's hue
// running, green succeeded, amber waiting, red failed or timed out, and faint where nothing came
// of it. No state is its colour alone: each is a word beside a dot.
func stateRole(s agk.RunState) role {
	switch s {
	case agk.Running:
		return runningText
	case agk.Succeeded:
		return succeededText
	case agk.Waiting:
		return waitingText
	case agk.Failed, agk.TimedOut:
		return failedText
	}
	return quiet
}
