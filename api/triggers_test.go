package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/cron"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Arming: "schedules, webhooks and events" run the default branch, so what its head declares under
// on is armed as a push lands it there, and nothing a push to another branch declares is.

// triggered is the workflow document with an on block, schedules and webhooks as written.
func triggered(on string) string {
	return workflowDocument + "on:\n" + on
}

// armedList is what GET .../triggers answers.
type armedList struct {
	Commit   string `json:"commit"`
	Triggers []struct {
		Kind     string          `json:"kind"`
		Position int             `json:"position"`
		Declared json.RawMessage `json:"declared"`
		URL      string          `json:"url"`
		Next     *time.Time      `json:"next"`
		FiresAt  *time.Time      `json:"fires_at"`
		Failures *int64          `json:"failures"`
		ArmedBy  string          `json:"armed_by"`
	} `json:"triggers"`
}

func (g *gitServer) armed(as string) armedList {
	g.t.Helper()
	w, _ := call(g.t, g.h, "GET", "/api/v1/finance/workflows/monthly-invoicing/triggers", as, nil)
	if w.Code != http.StatusOK {
		g.t.Fatalf("the triggers answered %d: %s", w.Code, w.Body)
	}
	var out armedList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		g.t.Fatal(err)
	}
	return out
}

// audited are the trigger entries recorded in finance, as action and detail, oldest first.
func (g *gitServer) audited() []string {
	g.t.Helper()
	rows, err := dbtest.Superuser(g.t, g.super).Query(g.t.Context(),
		`select action || ' ' || actor || ' ' || (detail::jsonb ->> 'kind') || ' ' || (detail::jsonb -> 'declared')::text
		 from audit_log where action like 'trigger.%' order by seq`)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestAPushToTheDefaultBranchArmsWhatItsHeadDeclares(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("alice")
	work.write("agentiik.yaml", triggered(`  schedule:
    - cron: "0 6 * * *"
      timezone: Europe/Paris
  webhook:
    - path: /invoicing
`))
	first := work.commit("first")
	before := time.Now()
	work.must("push", "-q", "origin", "main")

	armed := g.armed("bob")
	if armed.Commit != first || len(armed.Triggers) != 2 {
		t.Fatalf("after the first push the workflow has armed %+v, want the two triggers of %s", armed, first)
	}
	schedule, hook := armed.Triggers[0], armed.Triggers[1]
	if schedule.Kind != "schedule" || string(schedule.Declared) != `{"catch_up":false,"cron":"0 6 * * *","jitter":"0s","timezone":"Europe/Paris"}` {
		t.Errorf("the schedule is armed as %s %s", schedule.Kind, schedule.Declared)
	}
	paris, _ := cron.Zone("Europe/Paris")
	six, _ := cron.Parse("0 6 * * *")
	if want := six.Next(before, paris); schedule.Next == nil || !schedule.Next.Equal(want) || !schedule.FiresAt.Equal(want) {
		t.Errorf("the schedule comes round next at %v, firing at %v, want %v, with no jitter to draw", schedule.Next, schedule.FiresAt, want)
	}
	// The values in force written in: POST, hmac and async where the file writes none.
	if hook.Kind != "webhook" || string(hook.Declared) != `{"auth":"hmac","method":"POST","path":"/invoicing","response":"async"}` ||
		hook.URL != "https://agentiik.example.com/hooks/finance/invoicing" || hook.Failures == nil || *hook.Failures != 0 || hook.ArmedBy != "alice" {
		t.Errorf("the webhook is armed as %+v", hook)
	}

	// A branch other than the default runs nothing unattended, whatever it declares.
	work.must("checkout", "-q", "-b", "feature")
	work.write("agentiik.yaml", triggered(`  schedule:
    - cron: "*/5 * * * *"
`))
	work.commit("a feature")
	work.must("push", "-q", "origin", "feature")
	if again := g.armed("bob"); again.Commit != first || len(again.Triggers) != 2 {
		t.Errorf("a push to a feature branch changed what is armed to %+v", again)
	}

	// The next head on main changes the schedule and declares the webhook again: the schedule is
	// disarmed and armed anew, and the webhook keeps what was counted against its path.
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(), `update triggers set failures = 3 where kind = 'webhook'`); err != nil {
		t.Fatal(err)
	}
	work.must("checkout", "-q", "main")
	work.write("agentiik.yaml", triggered(`  schedule:
    - cron: "0 7 * * *"
      timezone: Europe/Paris
  webhook:
    - path: /invoicing
`))
	second := work.commit("second")
	work.must("push", "-q", "origin", "main")
	after := g.armed("bob")
	if after.Commit != second || len(after.Triggers) != 2 {
		t.Fatalf("after the second push the workflow has armed %+v", after)
	}
	if !strings.Contains(string(after.Triggers[0].Declared), `"0 7 * * *"`) || *after.Triggers[1].Failures != 3 {
		t.Errorf("the second head armed %s, and the webhook counts %d failures, want 3", after.Triggers[0].Declared, *after.Triggers[1].Failures)
	}

	got := g.audited()
	want := []string{
		`trigger.arm alice schedule {"cron": "0 6 * * *", "jitter": "0s", "catch_up": false, "timezone": "Europe/Paris"}`,
		`trigger.arm alice webhook {"auth": "hmac", "path": "/invoicing", "method": "POST", "response": "async"}`,
		`trigger.arm alice schedule {"cron": "0 7 * * *", "jitter": "0s", "catch_up": false, "timezone": "Europe/Paris"}`,
		`trigger.disarm alice schedule {"cron": "0 6 * * *", "jitter": "0s", "catch_up": false, "timezone": "Europe/Paris"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the log records\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// "Within a namespace a path and a method answer one trigger": a push to the default branch arming
// a pair another workflow of the namespace has armed is refused, and moves nothing.
func TestAPushArmingAWebhookAnotherWorkflowHoldsIsRefused(t *testing.T) {
	g := servingGit(t, everyone())
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "payroll", "main"); err != nil {
			return err
		}
		return ns.Arm(ctx, "payroll", strings.Repeat("a", 40), []db.Entry{{
			Kind: agk.TriggerWebhook, Position: 0, Declared: json.RawMessage(`{"path":"/invoicing","method":"POST"}`), Path: "/invoicing", Method: "POST",
		}}, "owner", time.Now())
	}); err != nil {
		t.Fatal(err)
	}

	work := g.newClone("alice")
	work.write("agentiik.yaml", triggered(`  webhook:
    - path: /invoicing
`))
	work.commit("first")
	out, err := work.run("push", "origin", "main")
	if err == nil {
		t.Fatalf("a push arming a webhook payroll holds was accepted:\n%s", out)
	}
	if !strings.Contains(out, "POST /invoicing is answered by the webhook of payroll, armed in finance") {
		t.Errorf("git printed:\n%s", out)
	}
	if ref := g.refs()["refs/heads/main"]; ref != "" {
		t.Errorf("main moved to %s all the same", ref)
	}

	// Another method, or another path, answers another trigger.
	work.write("agentiik.yaml", triggered(`  webhook:
    - path: /invoicing
      method: PUT
`))
	work.commit("second")
	work.must("push", "-q", "origin", "main")
	if armed := g.armed("bob"); len(armed.Triggers) != 1 {
		t.Errorf("a webhook answering PUT armed %+v", armed)
	}
}

// Naming another branch the default arms what its head declares, in the transaction that names it.
func TestNamingAnotherDefaultBranchArmsItsHead(t *testing.T) {
	g := servingGit(t, everyone())
	work := g.newClone("owner")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")
	work.must("checkout", "-q", "-b", "release")
	work.write("agentiik.yaml", triggered(`  schedule:
    - cron: "30 2 * * *"
`))
	second := work.commit("scheduled")
	work.must("push", "-q", "origin", "release")
	if armed := g.armed("bob"); armed.Commit != first || len(armed.Triggers) != 0 {
		t.Fatalf("before the change the workflow has armed %+v", armed)
	}

	w, _ := call(t, g.h, "PATCH", "/api/v1/finance/workflows/monthly-invoicing", "owner", map[string]any{"default_branch": "release"})
	if w.Code != http.StatusOK {
		t.Fatalf("naming release the default answered %d: %s", w.Code, w.Body)
	}
	if armed := g.armed("bob"); armed.Commit != second || len(armed.Triggers) != 1 || armed.Triggers[0].ArmedBy != "owner" {
		t.Errorf("after naming release the default, the workflow has armed %+v", armed)
	}
}
