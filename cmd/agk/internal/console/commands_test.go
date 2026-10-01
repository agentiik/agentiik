package console

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

var colon = tea.KeyPressMsg{Code: ':', Text: ":"}

// paletteLines are the palette's lines as drawn, between its frame.
func paletteLines(m Model) []string {
	lines := strings.Split(screen(m), "\n")
	// The palette's left border is the frame's character before what is typed into it.
	left := -1
	for _, l := range lines {
		r := []rune(l)
		for i := 0; i+2 < len(r); i++ {
			if r[i] == '│' && r[i+1] == ' ' && (strings.HasPrefix(string(r[i+2:]), ": ▏") || strings.HasPrefix(string(r[i+2:]), "notifications: ") || strings.HasPrefix(string(r[i+2:]), ": ")) {
				left = i
				break
			}
		}
		if left >= 0 {
			break
		}
	}
	var out []string
	if left < 0 {
		return out
	}
	for _, l := range lines {
		r := []rune(l)
		if left >= len(r) || r[left] != '│' {
			continue
		}
		rest := r[left+1:]
		if j := strings.IndexRune(string(rest), '│'); j >= 0 {
			out = append(out, strings.TrimSpace(string(rest)[:j]))
		}
	}
	return out
}

// : opens the palette over the view on the commands of the view, each with its key, and closes on
// esc.
func TestColonOpensThePaletteOnTheViewsCommands(t *testing.T) {
	m := press(t, opened(t, &installation{runs: someRuns()}, Options{}, 120, 30), colon)
	lines := paletteLines(m)
	if len(lines) < 6 || !strings.HasPrefix(lines[0], ": ▏") {
		t.Fatalf("the palette is not open:\n%s", screen(m))
	}
	var labels []string
	for _, l := range lines[1:6] {
		labels = append(labels, strings.Join(strings.Fields(l), " "))
	}
	want := []string{"Open the run selected enter", "Graph of the run selected g", "Filter the runs /", "Every key ?", "Quit q"}
	if !slices.Equal(labels, want) {
		t.Errorf("the palette opens on %q, not %q", labels, want)
	}
	if lastLine(m) != "type Find   ↑↓ Choose   enter Do   esc Close" {
		t.Errorf("the palette's keys are %q", lastLine(m))
	}
	for i, l := range strings.Split(screen(m), "\n") {
		if w := len([]rune(l)); w != 120 {
			t.Errorf("line %d is %d columns wide under the palette", i, w)
		}
	}
	if m = press(t, m, esc); m.palette != nil || m.view != runsView {
		t.Errorf("esc does not close the palette")
	}
}

// What is typed matches commands, runs, workflows and namespaces as the filter matches runs; a
// workflow chosen narrows the runs to it, and a run chosen is opened.
func TestThePaletteFindsRunsWorkflowsAndNamespaces(t *testing.T) {
	in := &installation{runs: someRuns()}
	m := typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "nightly")
	lines := paletteLines(m)
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "finance/nightly-export") || !strings.Contains(strings.Join(lines, "\n"), "01RUNCCCCCCCCCCCCCCCCCCCCC  finance/nightly-export  succeeded") {
		t.Fatalf("nightly does not find the workflow first, and its run:\n%s", strings.Join(lines, "\n"))
	}
	m = press(t, m, enter)
	if m.palette != nil || m.filter != "namespace=finance workflow=nightly-export" || in.asked[len(in.asked)-1] != "/api/v1/runs?limit=100&namespace=finance&workflow=nightly-export" {
		t.Errorf("choosing the workflow leaves the filter %q, having asked %s", m.filter, in.asked[len(in.asked)-1])
	}

	in.run = aFailedRun()
	m = press(t, typed(t, press(t, m, colon), "01RUNB"), enter)
	if m.view != runView || m.selected != failedRun {
		t.Errorf("choosing a run does not open it: view %d, %q", m.view, m.selected)
	}

	m = typed(t, press(t, opened(t, &installation{runs: someRuns()}, Options{}, 120, 30), colon), "finance")
	if !slices.ContainsFunc(paletteLines(m), func(l string) bool { return strings.HasPrefix(l, "finance ") && strings.HasSuffix(l, "namespace") }) {
		t.Errorf("the namespace is not found:\n%s", strings.Join(paletteLines(m), "\n"))
	}
}

// A command for what the principal does not hold is left out of the palette.
func TestThePaletteListsNothingThatIsNotHeld(t *testing.T) {
	m := typed(t, press(t, openedOn(t, &installation{}, aFailedRun(), 120), colon), "replay")
	if strings.Contains(strings.Join(paletteLines(m), "\n"), "Replay from") {
		t.Errorf("a replay is offered without workflow:run:\n%s", screen(m))
	}
	m = typed(t, press(t, openedOn(t, &installation{me: mayRunIn()}, aFailedRun(), 120), colon), "replay")
	if lines := paletteLines(m); len(lines) < 2 || !strings.HasPrefix(lines[1], "Replay from invoice") || !strings.HasSuffix(lines[1], "p") {
		t.Errorf("a replay is not offered with its key to who may run the workflow:\n%s", strings.Join(lines, "\n"))
	}
	m = typed(t, press(t, opened(t, &installation{runs: someRuns()}, Options{}, 120, 30), colon), "runners")
	if strings.Contains(strings.Join(paletteLines(m), "\n"), "Runners") {
		t.Errorf("the runners are offered to who is not an administrator")
	}
}

// The palette opens on the commands used last, the latest first.
func TestThePaletteOpensOnTheCommandsUsedLast(t *testing.T) {
	m := opened(t, &installation{runs: someRuns()}, Options{}, 120, 30)
	m = press(t, typed(t, press(t, m, colon), "every"), enter)
	if !m.listing {
		t.Fatalf("choosing Every key does not list the keys")
	}
	m = press(t, m, esc, colon)
	if lines := paletteLines(m); len(lines) < 2 || !strings.HasPrefix(lines[1], "Every key") {
		t.Errorf("the palette does not open on the command used last:\n%s", strings.Join(lines, "\n"))
	}
}

// The palette's Notifications lists every notification, and dismisses the one chosen.
func TestThePaletteDismissesANotification(t *testing.T) {
	in := &installation{runs: someRuns(), me: withNotices(recovery, widened)}
	m := press(t, typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "notif"), enter)
	lines := paletteLines(m)
	if m.palette == nil || m.palette.prompt != "notifications: " || len(lines) < 3 || !strings.HasPrefix(lines[0], "notifications: ") || !strings.HasPrefix(lines[1], "A recovery code was issued") {
		t.Fatalf("Notifications does not list the notifications:\n%s", screen(m))
	}
	asked := len(in.asked)
	m = press(t, m, down, enter)
	if len(in.sent) != 1 || in.sent[0] != "DELETE /api/v1/me/notifications/"+widened.ID+" null" || !slices.Contains(in.asked[asked:], "/api/v1/me") {
		t.Errorf("choosing a notification sent %v and read %v", in.sent, in.asked[asked:])
	}
}
