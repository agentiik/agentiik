package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// What a task was handed on its input ports, against a real PostgreSQL: "every step keeps its input
// and output envelopes, readable as they are", for as long as its run is kept and no longer.

// handedTask writes one dispatch of render, the way the controller writes it, and issues its grant
// naming what it was handed on in.
func handedTask(t *testing.T, pool *Pool, super, row string, attempt, shard int, handed GrantInput) Granted {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var index, of *int
	if shard > 0 {
		index, of = &shard, new(2)
	}
	if _, err := conn.Exec(t.Context(), `
		insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of, state)
		values ('finance', $1, $2, 'render', $3, $4, $5, 'dispatched')
		on conflict do nothing`, row, financeRun, attempt, index, of); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	sh := agk.Shard{}
	if shard > 0 {
		sh = agk.Shard{Index: shard, Of: 2}
	}
	var granted Granted
	err = pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", agk.NewTaskID(financeRun, "render", attempt, sh), row,
			GrantScope{Run: financeRun, Step: "render", Inputs: []GrantInput{handed}}, time.Now().UTC().Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return granted
}

func stepInput(t *testing.T, pool *Pool, step agk.Step, attempt, shard int, port agk.Port) (Envelope, error) {
	t.Helper()
	var e Envelope
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		e, err = ns.StepInput(ctx, financeRun, step, attempt, shard, port)
		return err
	})
	return e, err
}

// The envelope a shard is handed is often one nothing else names, a slice of a fan-out, so the
// grant counts it: once per grant, as a task issued a grant again holds two, and the purge lowers
// every one of them once the run's retention has run out.
func TestAnInputATaskWasHandedIsCountedUntilItsRunIsPurged(t *testing.T) {
	pool, super := opened(t)
	d := digestOf("4")
	handed := GrantInput{Port: "in", Digest: d, Items: 3, Size: 64}

	handedTask(t, pool, super, "01M2T2AAAAAAAAAAAAAAAAAAAA", 1, 1, handed)
	if got := refsOf(t, pool, "finance", d); got != 1 {
		t.Fatalf("an input one grant names is counted %d times", got)
	}
	handedTask(t, pool, super, "01M2T2AAAAAAAAAAAAAAAAAAAA", 1, 1, handed)
	if got := refsOf(t, pool, "finance", d); got != 2 {
		t.Fatalf("an input two grants name is counted %d times", got)
	}

	e, err := stepInput(t, pool, "render", 1, 1, "in")
	if err != nil || e.Digest != d || e.Size != 64 || e.Items != 3 || !e.PurgedAt.IsZero() {
		t.Fatalf("the input reads %+v, %v", e, err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update runs set started_at = now(), finished_at = now(), expires_at = now() - interval '1 minute'
		 where namespace = 'finance' and id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if purged, err := pool.PurgeEnvelopes(t.Context(), 0); err != nil || purged != 1 {
		t.Fatalf("the envelope purge took %d runs: %v", purged, err)
	}
	if got := refsOf(t, pool, "finance", d); got != 0 || !collectableNow(t, pool, "finance", d) {
		t.Errorf("after purging the run the input is counted %d times", got)
	}
	// The digest stays, as the record of what the task was handed.
	e, err = stepInput(t, pool, "render", 1, 1, "in")
	if err != nil || e.Digest != d || e.PurgedAt.IsZero() {
		t.Errorf("what is left of a purged input is %+v, %v", e, err)
	}
}

// A grant counting its reference onto an object a sweep has claimed is told to write the bytes
// again, as a reference to an artifact is, since the sweep may be about to delete them.
func TestAGrantOntoAnInputASweepClaimedIsToldToWriteItAgain(t *testing.T) {
	pool, super := opened(t)
	d := digestOf("5")
	handed := GrantInput{Port: "in", Digest: d, Items: 1, Size: 32}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `
		insert into artifact_objects (namespace, digest, size_bytes, media_type, refs, collectable_at)
		values ('finance', $1, 32, 'application/json', 0, now() - interval '2 days')`, "sha256:"+d); err != nil {
		t.Fatal(err)
	}
	if first := handedTask(t, pool, super, "01M2T3AAAAAAAAAAAAAAAAAAAA", 1, 0, handed); len(first.Rewrite) != 0 {
		t.Fatalf("a grant onto an object nobody claimed was told to write %v again", first.Rewrite)
	}
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set refs = 0, collectable_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := pool.Collectable(t.Context(), 0, 0); err != nil || len(claimed) != 1 {
		t.Fatalf("the sweep claimed %v: %v", claimed, err)
	}

	granted := handedTask(t, pool, super, "01M2T3AAAAAAAAAAAAAAAAAAAA", 1, 0, handed)
	if len(granted.Rewrite) != 1 || granted.Rewrite[0] != d {
		t.Errorf("a grant onto an input a sweep had claimed was told to write %v again", granted.Rewrite)
	}
}

func TestAGrantNamingAnInputWithNoSizeIsRefused(t *testing.T) {
	pool, super := opened(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `
		insert into tasks (namespace, id, run_id, step, attempt, state)
		values ('finance', '01M2T4AAAAAAAAAAAAAAAAAAAA', $1, 'render', 1, 'dispatched')`, financeRun); err != nil {
		t.Fatal(err)
	}
	for _, in := range []GrantInput{
		{Port: "in", Digest: digestOf("6"), Items: 1},
		{Port: "in", Digest: "sha256:" + digestOf("6"), Items: 1, Size: 12},
	} {
		err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			_, err := w.IssueGrant(ctx, "finance", agk.NewTaskID(financeRun, "render", 1, agk.Shard{}), "01M2T4AAAAAAAAAAAAAAAAAAAA",
				GrantScope{Run: financeRun, Step: "render", Inputs: []GrantInput{in}}, time.Now().UTC().Add(time.Hour))
			return err
		})
		if err == nil {
			t.Errorf("a grant naming the input %+v was issued", in)
		}
	}
}

// A dispatch is asked about by its attempt, the last one where none is named, and by its shard,
// which a step that was not fanned out has none of.
func TestAStepsInputIsReadForTheDispatchAskedAbout(t *testing.T) {
	pool, super := opened(t)
	first, second, retried := digestOf("7"), digestOf("8"), digestOf("9")
	handedTask(t, pool, super, "01M2T5AAAAAAAAAAAAAAAAAAAA", 1, 1, GrantInput{Port: "in", Digest: first, Items: 1, Size: 10})
	handedTask(t, pool, super, "01M2T6AAAAAAAAAAAAAAAAAAAA", 1, 2, GrantInput{Port: "in", Digest: second, Items: 1, Size: 11})
	handedTask(t, pool, super, "01M2T7AAAAAAAAAAAAAAAAAAAA", 2, 1, GrantInput{Port: "in", Digest: retried, Items: 1, Size: 12})

	for _, c := range []struct {
		attempt, shard int
		want           string
	}{
		{1, 1, first},
		{1, 2, second},
		{2, 1, retried},
		{0, 1, retried},
		{0, 2, second},
	} {
		if e, err := stepInput(t, pool, "render", c.attempt, c.shard, "in"); err != nil || e.Digest != c.want {
			t.Errorf("attempt %d shard %d reads %+v, %v, want %s", c.attempt, c.shard, e, err, c.want)
		}
	}

	for _, c := range []struct {
		name           string
		step           agk.Step
		attempt, shard int
		port           agk.Port
		want           error
	}{
		{"no shard of a fanned out step", "render", 0, 0, "in", ErrNoEnvelope},
		{"an attempt never dispatched", "render", 3, 1, "in", ErrNoEnvelope},
		{"a shard never dispatched", "render", 2, 2, "in", ErrNoEnvelope},
		{"a port it was handed nothing on", "render", 1, 1, "orders", ErrNoEnvelope},
		{"a step never dispatched", "archive", 0, 0, "in", ErrNoEnvelope},
		{"a step the run does not have", "notify", 0, 0, "in", ErrNoStep},
	} {
		if _, err := stepInput(t, pool, c.step, c.attempt, c.shard, c.port); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.StepInput(ctx, "01M2ZZZZZZZZZZZZZZZZZZZZZZ", "render", 0, 0, "in")
		return err
	})
	if !errors.Is(err, ErrNoRun) {
		t.Errorf("a run nobody started: %v", err)
	}

	// And the run lists each task with what it was handed, by digest.
	var detail RunDetail
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		detail, err = ns.RunDetail(ctx, financeRun)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	handed := map[agk.TaskID]string{}
	for _, task := range detail.Tasks {
		handed[task.Task] = task.Inputs["in"].Digest
	}
	for key, want := range map[agk.TaskID]string{
		agk.NewTaskID(financeRun, "render", 1, agk.Shard{Index: 1, Of: 2}): first,
		agk.NewTaskID(financeRun, "render", 1, agk.Shard{Index: 2, Of: 2}): second,
		agk.NewTaskID(financeRun, "render", 2, agk.Shard{Index: 1, Of: 2}): retried,
		agk.NewTaskID(financeRun, "archive", 1, agk.Shard{}):               "",
	} {
		if got, listed := handed[key]; !listed || got != want {
			t.Errorf("the run lists %s as handed %q, want %q", key, got, want)
		}
	}
}
