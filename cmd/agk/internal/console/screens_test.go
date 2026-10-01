package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/agentiik/agentiik/db"
)

// Every view is drawn on the answers recorded for the web console's own tests, the scenario of
// console/tests/fixtures/alice.json, into a virtual terminal of a fixed size: Lip Gloss's canvas, a
// grid of cells as wide and as high as the window, which the screen is written into as a terminal
// would write it. What the grid holds is compared with the screens kept in testdata/screens, at 80
// by 24 and at 160 by 44, and in each colour depth, which may change the colours and never a cell.
// No controller, runner or database is behind it: the scenario is the installation.
//
//	go test ./cmd/agk/internal/console -run TestEveryViewOnTheRecordedAnswers -update
//
// writes the screens again, to be read in the diff of the pull request that changes them.

var update = flag.Bool("update", false, "rewrite the screens kept in testdata/screens")

// recordedNow is the instant the scenario was recorded at: its running run started two minutes
// before.
var recordedNow = time.Date(2026, 9, 30, 6, 2, 0, 0, time.UTC)

// answer is one answer of the scenario: its status and its body.
type answer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// scenario is the web console's recorded answers, by "METHOD /path" or "METHOD /path?query".
type scenario map[string]answer

func loadScenario(t *testing.T) scenario {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "console", "tests", "fixtures", "alice.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s scenario
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// recorded is what the scenario holds for a request, as console/tests/serve.js reads it: the answer
// for its whole query, else for a part of it naming the most of its parameters, else for its path.
func (s scenario) recorded(path string) (answer, bool) {
	at, query, _ := strings.Cut(path, "?")
	key := "GET " + at
	if a, ok := s[key+"?"+query]; ok && query != "" {
		return a, true
	}
	asked, _ := url.ParseQuery(query)
	best, named := answer{}, 0
	for recorded, a := range s {
		wants, ok := strings.CutPrefix(recorded, key+"?")
		if !ok {
			continue
		}
		parts, _ := url.ParseQuery(wants)
		all := true
		for k, vs := range parts {
			for _, v := range vs {
				found := false
				for _, got := range asked[k] {
					found = found || got == v
				}
				all = all && found
			}
		}
		if all && len(parts) > named {
			best, named = a, len(parts)
		}
	}
	if named > 0 {
		return best, true
	}
	a, ok := s[key]
	return a, ok
}

func (s scenario) read(_ context.Context, path string, out any) error {
	a, ok := s.recorded(path)
	if !ok || a.Status >= 400 {
		// What the scenario does not hold is answered as the API answers what the caller may not see.
		return errors.New("no such thing, or not yours")
	}
	return json.Unmarshal(a.Body, out)
}

// onScenario is a console drawn on the scenario in a window of the size given, at a colour depth,
// its first reads come back and the keys given pressed.
func onScenario(t *testing.T, s scenario, o Options, width, height int, d depth, keys ...tea.KeyPressMsg) Model {
	t.Helper()
	o.Read, o.Now = s.read, func() time.Time { return recordedNow }
	o.Installation = "agentiik.example.com"
	n := New(t.Context(), o)
	n.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	n.depth, n.settled = d, true
	var m tea.Model = n
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	for _, msg := range run(m.Init()) {
		m = send(t, m, msg)
	}
	return press(t, m.(Model), keys...)
}

// cells is the screen as the virtual terminal holds it: each line of the grid, its cells' text,
// with what overflows the grid refused rather than clipped.
func cells(t *testing.T, screen string, width, height int) string {
	t.Helper()
	lines := strings.Split(screen, "\n")
	if len(lines) != height {
		t.Fatalf("the screen is %d lines high in a window of %d", len(lines), height)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != width {
			t.Fatalf("line %d is %d cells wide in a window of %d", i, w, width)
		}
	}
	c := lipgloss.NewCanvas(width, height)
	c.Compose(lipgloss.NewLayer(screen))
	var b strings.Builder
	for y := range height {
		var line strings.Builder
		for x := 0; x < width; {
			cell := c.CellAt(x, y)
			if cell == nil || cell.Content == "" {
				line.WriteByte(' ')
				x++
				continue
			}
			line.WriteString(cell.Content)
			x += max(1, cell.Width)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

var (
	sgr        = regexp.MustCompile("\x1b\\[([0-9;]*)m")
	trueOrNot  = regexp.MustCompile(`(^|;)[34]8;2;`)
	twoFifty   = regexp.MustCompile(`(^|;)[34]8;5;`)
	sixteenSGR = regexp.MustCompile(`(^|;)(3[0-7]|4[0-7]|9[0-7]|10[0-7])(;|$)`)
)

// colouredAs fails where a screen holds a colour its depth does not draw: none without colour, the
// sixteen named alone at sixteen, where a view may hold none of them, and at 256 and in true colour
// the ground the palette paints under every cell, in the depth's own form.
func colouredAs(t *testing.T, screen string, d depth) {
	t.Helper()
	var truly, of256, sixteenNamed int
	for _, m := range sgr.FindAllStringSubmatch(screen, -1) {
		switch {
		case trueOrNot.MatchString(m[1]):
			truly++
		case twoFifty.MatchString(m[1]):
			of256++
		case sixteenSGR.MatchString(m[1]):
			sixteenNamed++
		}
	}
	ok := map[depth]bool{
		noColour:    truly == 0 && of256 == 0 && sixteenNamed == 0,
		sixteen:     truly == 0 && of256 == 0,
		twoFiftySix: truly == 0 && of256 > 0,
		trueColour:  of256 == 0 && truly > 0,
	}[d]
	if !ok {
		t.Errorf("at depth %d the screen holds %d true colours, %d of the 256 and %d of the sixteen", d, truly, of256, sixteenNamed)
	}
}

// The views and how each is reached from the console opened.
var views = []struct {
	name string
	o    Options
	keys []tea.KeyPressMsg
}{
	{"runs", Options{}, nil},
	{"run", Options{Run: "01JMZ8V1P9C4XQ7K2N4D6F8H0A"}, nil},
	{"graph", Options{Run: "01JMZ8V1P9C4XQ7K2N4D6F8H0A"}, []tea.KeyPressMsg{keyG}},
	{"graph-list", Options{Run: "01JMZ8V1P9C4XQ7K2N4D6F8H0A"}, []tea.KeyPressMsg{keyG, keyG}},
	{"workflows", Options{}, []tea.KeyPressMsg{two}},
	{"sharing", Options{}, []tea.KeyPressMsg{three}},
	{"keys", Options{}, []tea.KeyPressMsg{help}},
	{"palette", Options{}, []tea.KeyPressMsg{{Code: ':', Text: ":"}}},
}

// Every view, at 80 by 24 and at 160 by 44, is the screen kept for it in each colour depth, its
// colours those of the depth.
func TestEveryViewOnTheRecordedAnswers(t *testing.T) {
	s := loadScenario(t)
	for _, v := range views {
		for _, size := range [][2]int{{80, 24}, {160, 44}} {
			name := fmt.Sprintf("%s-%dx%d", v.name, size[0], size[1])
			kept := filepath.Join("testdata", "screens", name+".txt")
			var first string
			for _, d := range []depth{noColour, sixteen, twoFiftySix, trueColour} {
				m := onScenario(t, s, v.o, size[0], size[1], d, v.keys...)
				screen := m.screen()
				m.unfollow()
				got := cells(t, screen, size[0], size[1])
				colouredAs(t, screen, d)
				if d == noColour {
					first = got
					if *update {
						if err := os.WriteFile(kept, []byte(got), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					want, err := os.ReadFile(kept)
					if err != nil {
						t.Fatalf("%s: %v; -update writes it", name, err)
					}
					if got != string(want) {
						t.Errorf("%s is not the screen kept in %s:\n%s", name, kept, got)
					}
					continue
				}
				if got != first {
					t.Errorf("%s at depth %d moves a cell the colourless screen has:\n%s", name, d, got)
				}
			}
		}
	}
}

// Where there is no terminal, the runs are the lines kept for them: one a run, newest first.
func TestTheRunsAsPlainLinesOnTheRecordedAnswers(t *testing.T) {
	s := loadScenario(t)
	var listed struct {
		Runs []db.ListedRun `json:"runs"`
	}
	if err := s.read(t.Context(), "/api/v1/runs?limit=100", &listed); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Lines(&b, listed.Runs, recordedNow); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join("testdata", "screens", "runs.lines")
	if *update {
		if err := os.WriteFile(kept, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("%v; -update writes it", err)
	}
	if b.String() != string(want) {
		t.Errorf("the runs as lines are not those kept in %s:\n%s", kept, b.String())
	}
}
