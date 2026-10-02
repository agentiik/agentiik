package console

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

const failedRun = "01RUNBBBBBBBBBBBBBBBBBBBBB"

// aFailedRun is monthly-invoicing failed at invoice: normalize published two ports, invoice's
// second of three shards spent its attempts on a transient failure, archive was never reached.
func aFailedRun() db.RunDetail {
	at := now.Add(-10 * time.Minute)
	code := func(n int) *int { return &n }
	shard := func(i int) *agk.Shard { return &agk.Shard{Index: i, Of: 3} }
	return db.RunDetail{
		RunSummary: db.RunSummary{
			Namespace: "finance", Run: failedRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f",
			State: agk.Failed, Trigger: agk.TriggerManual, TriggeredBy: "alice", CreatedAt: at, StartedAt: at, FinishedAt: at.Add(112 * time.Second),
		},
		Steps: []db.StepSummary{
			{Step: "normalize", Verdict: agk.VerdictSucceeded, Attempts: 1, StartedAt: at, FinishedAt: at.Add(1200 * time.Millisecond),
				Image: "ghcr.io/acme/agk-normalize@sha256:9f2c1d00000000000000000000000000000000000000000000000000000000b7", OutputPorts: []agk.Port{"ok", "rejected"},
				Ports: map[agk.Port]db.Envelope{"ok": {Digest: "sha256:abababababab", Size: 2210, Items: 3}, "rejected": {Digest: "sha256:cdcdcdcdcdcd", Size: 120, Items: 0}}},
			{Step: "invoice", Verdict: agk.VerdictFailed, Attempts: 4, StartedAt: at.Add(2 * time.Second), FinishedAt: at.Add(109 * time.Second), Image: "ghcr.io/acme/agk-invoice@sha256:1ab74e"},
			{Step: "archive", Verdict: agk.VerdictSkipped},
		},
		Tasks: []db.TaskSummary{
			{Step: "normalize", State: agk.TaskSucceeded, Attempt: 1, ExitCode: code(0), Runner: "runner-a"},
			{Step: "invoice", State: agk.TaskSucceeded, Attempt: 1, Shard: shard(1), ExitCode: code(0), Runner: "runner-a"},
			{Step: "invoice", State: agk.TaskFailed, Attempt: 1, Shard: shard(2), ExitCode: code(108), Runner: "runner-a"},
			{Step: "invoice", State: agk.TaskFailed, Attempt: 4, Shard: shard(2), ExitCode: code(108), Runner: "runner-dmz-02"},
			{Step: "invoice", State: agk.TaskSucceeded, Attempt: 1, Shard: shard(3), ExitCode: code(0), Runner: "runner-a"},
		},
	}
}

func inspecting(t *testing.T, in *installation, width int) Model {
	t.Helper()
	in.run = aFailedRun()
	return opened(t, in, Options{Run: failedRun}, width, 40)
}

// The run opens on its first failed step: the header says where it failed and what the exit code
// means, the steps pane each step's verdict, duration and shards.
func TestTheInspectorOpensOnWhereTheRunFailed(t *testing.T) {
	m := inspecting(t, &installation{}, 160)
	s := screen(m)
	for _, want := range []string{
		"Run 01RUNBBBBBBBBBBBBBBBBBBBBB  finance/monthly-invoicing@a3f9c1e  ● failed",
		"manual · by alice · started 11:50:00 · took 1m 52s",
		"failed at invoice, shard 2/3: exit 108, transient failure, attempt 4",
		"STEPS 3",
		"● succeeded  normalize",
		"● failed     invoice",
		"3 shards · 1 failed",
		"● skipped    archive",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the inspector does not say %q:\n%s", want, s)
		}
	}
	if m.step != "invoice" {
		t.Errorf("the inspector opens on %q, not the step that failed", m.step)
	}
	// The step chosen: each shard's last attempt, the retries moved past left out.
	for _, want := range []string{"image   ghcr.io/acme/agk-invoice@sha256:1ab74e", "2/3    ● failed    4        exit 108, transient failure", "runner-dmz-02"} {
		if !strings.Contains(s, want) {
			t.Errorf("the step does not say %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "2/3 ") != 1 {
		t.Errorf("shard 2's earlier attempt is listed beside its last:\n%s", s)
	}
}

// The run's pane lays its steps beside the step chosen where it has room, and above it where it
// has not; a step's name is whole at 120 columns.
func TestTheInspectorLaysOutItsPanesByTheWindow(t *testing.T) {
	if s := screen(inspecting(t, &installation{}, 300)); !lineWith(s, "● succeeded  normalize", "image ") {
		t.Errorf("at 300 columns the steps and the step are not side by side:\n%s", s)
	}
	if s := screen(inspecting(t, &installation{}, 120)); !strings.Contains(s, "● succeeded  normalize ") {
		t.Errorf("at 120 columns a step's name is cut:\n%s", s)
	}
	narrow := screen(inspecting(t, &installation{}, 100))
	if lineWith(narrow, "● succeeded  normalize", "image ") {
		t.Errorf("at 100 columns the steps and the step are still side by side:\n%s", narrow)
	}
	for i, l := range strings.Split(narrow, "\n") {
		if w := len([]rune(l)); w != 100 {
			t.Errorf("line %d is %d columns wide in a window of 100: %q", i, w, l)
		}
	}
}

func lineWith(s string, a, b string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, a) && strings.Contains(l, b) {
			return true
		}
	}
	return false
}

// ↓ and k move between the steps, and [ ] between the ports of the step chosen, each with its items.
func TestTheInspectorMovesBetweenStepsAndPorts(t *testing.T) {
	m := inspecting(t, &installation{}, 160)
	m = press(t, m, up)
	if m.step != "normalize" {
		t.Fatalf("↑ from invoice chooses %q", m.step)
	}
	s := screen(m)
	if !strings.Contains(s, "▸ ok               3   2.2 KiB   sha256:abababababab") || !strings.Contains(s, "  rejected         0   120 B") {
		t.Errorf("normalize's ports are not listed with their items:\n%s", s)
	}
	m = press(t, m, tea.KeyPressMsg{Code: ']', Text: "]"})
	if m.port != 1 || !strings.Contains(screen(m), "▸ rejected") {
		t.Errorf("] does not choose the next port: %d\n%s", m.port, screen(m))
	}
	m = press(t, m, tea.KeyPressMsg{Code: ']', Text: "]"}, j, j, j)
	if m.step != "archive" || m.port != 0 {
		t.Errorf("j past the last step leaves %q, port %d", m.step, m.port)
	}
}

// An envelope is drawn to whoever holds run:read_data, and to nobody else, hidden rather than refused.
func TestAnEnvelopeIsShownUnderRunReadDataAlone(t *testing.T) {
	envelope := map[string]any{"meta": map[string]any{"port": "ok"}, "items": []any{map[string]any{"id": "a", "data": map[string]any{"order": "ORD-0001"}}}}
	path := "/api/v1/runs/" + failedRun + "/steps/normalize/outputs/ok"

	reader := &installation{me: &principal{Principal: "alice", Permissions: map[string][]string{"finance": {"run:read", "run:read_data"}}}, envelopes: map[string]any{path: envelope}}
	m := press(t, inspecting(t, reader, 160), up)
	if !strings.Contains(screen(m), `"order": "ORD-0001"`) {
		t.Errorf("the envelope is not drawn under run:read_data:\n%s", screen(m))
	}

	plain := &installation{envelopes: map[string]any{path: envelope}}
	m = press(t, inspecting(t, plain, 160), up)
	if strings.Contains(screen(m), "ORD-0001") || strings.Contains(screen(m), "Reading the envelope") {
		t.Errorf("the envelope is shown, or offered, without run:read_data:\n%s", screen(m))
	}
	for _, a := range plain.asked {
		if strings.Contains(a, "/outputs/") {
			t.Errorf("an envelope was asked for without run:read_data: %s", a)
		}
	}
}

// An administrator reads each runner's pool and labels beside its name; nobody else asks.
func TestAnAdministratorSeesARunnersPoolAndLabels(t *testing.T) {
	admin := &installation{me: &principal{Principal: "dana", Admin: true, Permissions: map[string][]string{"finance": {"run:read"}}},
		runners: []db.Runner{{ID: "runner-dmz-02", Pool: "dmz", Labels: []string{"arch=amd64", "zone=dmz"}}}}
	if s := screen(inspecting(t, admin, 160)); !strings.Contains(s, "runner-dmz-02 · pool dmz · arch=amd64,zone=dmz") {
		t.Errorf("an administrator does not see the runner's pool and labels:\n%s", s)
	}
	other := &installation{}
	if s := screen(inspecting(t, other, 160)); strings.Contains(s, "pool dmz") {
		t.Errorf("a pool is shown to who may not read the runners:\n%s", s)
	}
	for _, a := range other.asked {
		if a == "/api/v1/runners" {
			t.Error("the runners were asked for by somebody who is not an administrator")
		}
	}
}
