package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/trigger"
)

// Schedules on a frozen clock: "an unattended schedule runs", on the instant it is due in its zone
// across both daylight saving shifts and a leap day, once however many controllers look at it, and
// what an outage missed is made up where catch_up asks for it and skipped where it does not.

// scheduledCommit is the version the schedules of these tests are armed from.
const scheduledCommit = "5c4e3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c"

// scheduleOf is a workflow of one step and one schedule, as written under on.
func scheduleOf(entry string) string {
	return `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
on:
  schedule:
    - ` + entry + `
inputs:
  orders: { schema: { type: array }, default: [] }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`
}

type scheduled struct {
	core    *Core
	pool    *db.Pool
	super   string
	starter *trigger.Starter
}

// scheduling arms the schedule written as entry at the clock's instant, with no jitter drawn.
func scheduling(t *testing.T, at time.Time, entry string) scheduled {
	t.Helper()
	clock.set(at)
	doc := scheduleOf(entry)
	core, _, pool, super := decidingOn(t, doc)
	clock.set(at)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		 values ('finance', 'monthly-invoicing', $1, '{}', 'alice', now())`, scheduledCommit); err != nil {
		t.Fatal(err)
	}
	was := trigger.Draw
	trigger.Draw = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(func() { trigger.Draw = was })

	wf, err := graph.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := trigger.Entries(wf, "finance", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.Arm(ctx, "monthly-invoicing", scheduledCommit, entries, "alice", at)
	}); err != nil {
		t.Fatal(err)
	}
	starter, err := trigger.New(trigger.Options{Pool: pool, Versions: core.versions, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return scheduled{core: core, pool: pool, super: super, starter: starter}
}

// fire runs one pass of the scheduler at the given instant.
func (s scheduled) fire(t *testing.T, core *Core, at string) error {
	t.Helper()
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(when)
	var trouble []error
	err = core.Fire(t.Context(), s.starter, func(err error) { trouble = append(trouble, err) })
	for _, e := range trouble {
		t.Errorf("a schedule could not fire: %v", e)
	}
	return err
}

// runs are the runs the schedule started, each as its trigger, principal and occurrence, oldest first.
func (s scheduled) runs(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, s.super).Query(t.Context(),
		`select trigger || ' ' || triggered_by || ' ' || (trigger_context -> 'trigger' ->> 'scheduled_for')
		 from runs where trigger = 'schedule' order by trigger_context -> 'trigger' ->> 'scheduled_for'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// firedFor is each occurrence a run was started for, as scheduled_for writes it.
func (s scheduled) firedFor(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range s.runs(t) {
		f := strings.Fields(r)
		out = append(out, f[len(f)-1])
	}
	return out
}

// row is the schedule's row as a listing reads it.
func (s scheduled) row(t *testing.T) db.Armed {
	t.Helper()
	var armed []db.Armed
	if err := s.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		armed, err = ns.ArmedBy(ctx, "monthly-invoicing")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(armed) != 1 {
		t.Fatalf("the workflow has armed %d triggers", len(armed))
	}
	return armed[0]
}

func same(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

func TestAnUnattendedScheduleRunsOnceWhenItIsDue(t *testing.T) {
	s := scheduling(t, time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), `cron: "0 6 * * *"`)
	if err := s.fire(t, s.core, "2026-10-01T05:59:59Z"); err != nil {
		t.Fatal(err)
	}
	same(t, "a second before it is due", s.runs(t))

	for _, at := range []string{"2026-10-01T06:00:00Z", "2026-10-01T06:00:01Z", "2026-10-01T06:30:00Z"} {
		if err := s.fire(t, s.core, at); err != nil {
			t.Fatal(err)
		}
	}
	// Attributed to the namespace's built-in identity, "not to the person who last edited the
	// workflow", and fired once however many passes look at it.
	same(t, "once it is due", s.runs(t), "schedule finance/agentiik 2026-10-01T06:00:00Z")
	row := s.row(t)
	if !row.DueAt.Equal(time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)) || row.FiredRun == "" || row.Skipped != "" {
		t.Errorf("after firing the schedule is due at %s, fired %q, skipped %q", row.DueAt, row.FiredRun, row.Skipped)
	}
}

func TestBothDaylightSavingShiftsFireEachOccurrenceOnce(t *testing.T) {
	// Paris goes from 02:00 CET to 03:00 CEST on 29 March 2026: 02:30 runs at 03:00, 01:00 UTC.
	s := scheduling(t, time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC), `{ cron: "30 2 * * *", timezone: Europe/Paris }`)
	for _, at := range []string{"2026-03-29T00:59:59Z", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z"} {
		if err := s.fire(t, s.core, at); err != nil {
			t.Fatal(err)
		}
	}
	same(t, "the night the clocks go forward", s.firedFor(t), "2026-03-29T01:00:00Z")

	// And back on 25 October: 02:30 comes twice, 00:30 and 01:30 UTC, and runs at the first.
	for _, at := range []string{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z", "2026-10-26T01:30:00Z"} {
		if err := s.fire(t, s.core, at); err != nil {
			t.Fatal(err)
		}
	}
	// The months between are missed while no pass looked, and catch_up is false: the scheduler
	// skipped them, and the two nights of the shift are the runs it made.
	got := s.firedFor(t)
	if len(got) != 3 || got[1] != "2026-10-25T00:30:00Z" || got[2] != "2026-10-26T01:30:00Z" {
		t.Errorf("around the night the clocks go back the schedule fired for %v", got)
	}
}

func TestALeapDayComesRound(t *testing.T) {
	s := scheduling(t, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC), `cron: "0 0 29 2 *"`)
	if due := s.row(t).DueAt; !due.Equal(time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a schedule for the 29th of February is due at %s", due)
	}
	if err := s.fire(t, s.core, "2028-02-29T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	same(t, "on the leap day", s.firedFor(t), "2028-02-29T00:00:00Z")
}

// "Fire schedules only from the election-lock holder, fencing writes with its acquisition counter,
// so failover never skips or doubles one." A controller that took over after the occurrence passed
// fires it, late, and the one it took over from is refused.
func TestAControllerTakingOverFiresTheOccurrenceItFoundAndTheOldOneIsFenced(t *testing.T) {
	s := scheduling(t, time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), `cron: "0 6 * * *"`)
	next, _, _, _ := resumeOn(t, s.pool, s.super, s.core)
	if err := s.fire(t, next, "2026-10-01T06:03:00Z"); err != nil {
		t.Fatal(err)
	}
	err := s.fire(t, s.core, "2026-10-01T06:03:01Z")
	if !errors.Is(err, db.ErrFenced) {
		t.Errorf("the controller that lost the lead fired with %v", err)
	}
	same(t, "across the failover", s.firedFor(t), "2026-10-01T06:00:00Z")
}

func TestWhatAnOutageMissedIsSkippedWithoutCatchUp(t *testing.T) {
	s := scheduling(t, time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC), `cron: "0 * * * *"`)
	// Down from 06:30 to 11:05: 07:00 to 10:00 were missed, and 11:00 is reached within ten minutes.
	if err := s.fire(t, s.core, "2026-10-01T11:05:00Z"); err != nil {
		t.Fatal(err)
	}
	same(t, "after the outage", s.firedFor(t), "2026-10-01T11:00:00Z")
	if due := s.row(t).DueAt; !due.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("after the outage the schedule is due at %s", due)
	}
}

func TestWhatAnOutageMissedIsMadeUpOnceEachWithCatchUp(t *testing.T) {
	s := scheduling(t, time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC), `{ cron: "0 * * * *", catch_up: true }`)
	for _, at := range []string{"2026-10-01T11:05:00Z", "2026-10-01T11:05:01Z"} {
		if err := s.fire(t, s.core, at); err != nil {
			t.Fatal(err)
		}
	}
	var want []string
	for h := 7; h <= 11; h++ {
		want = append(want, fmt.Sprintf("2026-10-01T%02d:00:00Z", h))
	}
	same(t, "made up", s.firedFor(t), want...)
}

// "A scheduled or event firing starts no run and is recorded as a skipped firing, with that
// reason", where the namespace's max_runs_per_hour is spent; and the schedule moves on.
func TestAFiringPastTheHourlyQuotaIsRecordedAsSkipped(t *testing.T) {
	s := scheduling(t, time.Date(2026, 10, 1, 5, 58, 0, 0, time.UTC), `cron: "* * * * *"`)
	if _, err := dbtest.Superuser(t, s.super).Exec(t.Context(),
		`update namespaces set max_runs_per_hour = 1 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"2026-10-01T05:59:00Z", "2026-10-01T06:00:00Z"} {
		if err := s.fire(t, s.core, at); err != nil {
			t.Fatal(err)
		}
	}
	same(t, "within the quota", s.firedFor(t), "2026-10-01T05:59:00Z")
	row := s.row(t)
	if !strings.Contains(row.Skipped, "max_runs_per_hour, 1") || !row.FiredFor.Equal(time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)) || row.FiredRun != "" {
		t.Errorf("the second firing is recorded for %s as %q, run %q", row.FiredFor, row.Skipped, row.FiredRun)
	}
	// And counted among the runs the namespace was refused, as a request answered 429 is.
	var refused int
	if err := dbtest.Superuser(t, s.super).QueryRow(t.Context(),
		`select coalesce(sum(refused), 0) from run_refusals where namespace = 'finance'`).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	if refused != 1 {
		t.Errorf("finance counts %d runs refused, where one firing was", refused)
	}
}
