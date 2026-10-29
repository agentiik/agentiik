package console

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

// A dismissal reads GET /api/v1/me once, and the reads every half minute stay one chain: a dismissal
// answered the half minute's own message, whose read asked for the next, so that each one started
// a second chain reading /me every 30 seconds for as long as the console stayed open.
func TestADismissalKeepsTheReadsOfMeOneChain(t *testing.T) {
	in := &installation{runs: someRuns(), me: withNotices(recovery, widened, counter)}
	m := opened(t, in, Options{}, 120, 30)
	// The chain opening asked for is the one pending; each link asked for after it is counted.
	links := 0
	m.tick = func(_ time.Duration, then func(time.Time) tea.Msg) tea.Cmd {
		if _, ok := then(now).(meAgain); ok {
			links++
		}
		return nil
	}
	dismiss := func(m Model, down int) Model {
		t.Helper()
		m = press(t, typed(t, press(t, m, colon), "notif"), enter)
		for range down {
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
		}
		asked := len(in.asked)
		m = press(t, m, enter)
		if read := slices.DeleteFunc(slices.Clone(in.asked[asked:]), func(p string) bool { return p != "/api/v1/me" }); len(read) != 1 {
			t.Errorf("a dismissal read /me %d times", len(read))
		}
		return m
	}
	m = dismiss(m, 1)
	if links != 0 {
		t.Fatalf("a dismissal asked for %d more reads half a minute later, beside the chain's own", links)
	}
	m = readAgain(t, m, meAgain{})
	m = dismiss(m, 2)
	if links != 1 {
		t.Errorf("the chain and a dismissal asked for %d reads half a minute later, where the chain asks for one", links)
	}
}

// A notification the installation does not dismiss is said so on the key line, as the installation
// says why, until the next key, which goes where it was going; it was dropped with nothing said.
func TestARefusedDismissalIsSaid(t *testing.T) {
	in := &installation{runs: someRuns(), me: withNotices(widened), refusing: errors.New("the installation could not be reached")}
	m := press(t, typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "notif"), enter, enter)
	if len(in.sent) != 1 || m.palette != nil || lastLine(m) != "The notification could not be dismissed: the installation could not be reached" {
		t.Fatalf("a dismissal refused sent %v, and the key line says %q", in.sent, lastLine(m))
	}
	m = press(t, m, down)
	if m.selected != "01RUNBBBBBBBBBBBBBBBBBBBBB" || !strings.HasPrefix(lastLine(m), "↑↓ Move") {
		t.Errorf("the key after a dismissal refused selects %q, the key line %q", m.selected, lastLine(m))
	}
}

// A recovery code's notification is listed and dismissed by nobody, as the API keeps it its 90 days
// whoever asks: choosing it closes the palette and sends nothing.
func TestThePaletteDismissesNoRecoveryCodesNotification(t *testing.T) {
	for _, kind := range []string{"recovery_code_issued", "recovery_code_used"} {
		kept := notice{ID: "01M2AK7T9W1Y3A5C7E9G1J3K5N", Kind: kind, By: "carol", Credential: "bmV3UGFzc2tleQ"}
		in := &installation{runs: someRuns(), me: withNotices(kept)}
		m := press(t, typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "notif"), enter)
		if lines := paletteLines(m); m.palette == nil || len(lines) < 2 || !strings.Contains(lines[1], "carol") {
			t.Fatalf("Notifications does not list %s:\n%s", kind, screen(m))
		}
		m = press(t, m, enter)
		if len(in.sent) != 0 || m.palette != nil {
			t.Errorf("choosing %s sent %v, the palette open: %v", kind, in.sent, m.palette != nil)
		}
	}
}

// A new login's notification is listed in the web console's words and dismissed when chosen, as the
// other kinds but a recovery code's are.
func TestThePaletteDismissesANewLoginsNotification(t *testing.T) {
	changed := notice{ID: "01M2AK5V7X9Z1B3D5F7H9K1M3P", Kind: "login_changed", From: "mcp", To: "mc-petit", By: "alice"}
	in := &installation{runs: someRuns(), me: withNotices(changed)}
	m := press(t, typed(t, press(t, opened(t, in, Options{}, 120, 30), colon), "notif"), enter)
	if lines := paletteLines(m); m.palette == nil || len(lines) < 2 || !strings.Contains(lines[1], "alice gave you the login mc-petit in place of mcp") {
		t.Fatalf("Notifications does not list the new login:\n%s", screen(m))
	}
	m = press(t, m, enter)
	if len(in.sent) != 1 || in.sent[0] != "DELETE /api/v1/me/notifications/"+changed.ID+" null" {
		t.Errorf("choosing the new login's notification sent %v", in.sent)
	}
}
