package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The reference half of the store, against a real PostgreSQL, for the same reason as the
// file beside this one: what is being tested is what the database does when Go is wrong, and
// a fake would agree with whatever the code believes.

const (
	financeRun = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	opsRun     = "01M2AAZ9G62NQXFAFCXKRPJEH5"
)

func digestOf(c string) string { return strings.Repeat(c, 64) }

// steps writes one step per run, and a task under the finance one, so that a reference and a
// log have something to hang from.
func steps(t *testing.T, super string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		`insert into steps (namespace, run_id, step) values
		   ('finance', '` + financeRun + `', 'archive'),
		   ('finance', '` + financeRun + `', 'render'),
		   ('team-ops', '` + opsRun + `', 'archive')`,
		`insert into tasks (namespace, id, run_id, step, attempt, state, log_uri, log_lines)
		   values ('finance', '01M2T1AAAAAAAAAAAAAAAAAAAA', '` + financeRun + `', 'archive', 1,
		           'succeeded', 'agk://log/finance/01M2T1AAAAAAAAAAAAAAAAAAAA', 812)`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
}

func opened(t *testing.T) (*Pool, string) {
	t.Helper()
	super, app := database(t)
	seed(t, super)
	steps(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, super
}

func uri(run, step, port, name string) agk.URI {
	return agk.URI{Run: agk.RunID(run), Step: agk.Step(step), Port: agk.Port(port), Name: name}
}

// The sentence the Decision block states, executed: a logical URI resolves to a physical key,
// and that key carries the namespace.
func TestALogicalURIResolvesToAPhysicalKey(t *testing.T) {
	pool, _ := opened(t)
	u := uri(financeRun, "archive", "out", "invoices.zip")

	var written Written
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		written, err = ns.WriteArtifact(ctx, Reference{
			URI: u, Digest: digestOf("a"), Size: 4096, MediaType: "application/zip",
			For: 90 * 24 * time.Hour,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "finance/sha256/" + digestOf("a"); written.Key != want {
		t.Errorf("the key is %q and the store builds %q", written.Key, want)
	}
	if !written.New {
		t.Error("the first write of a reference says it was already there")
	}

	var got Resolved
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		got, err = ns.Resolve(ctx, u)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != written.Key || got.Size != 4096 || got.MediaType != "application/zip" {
		t.Errorf("resolved to %+v", got)
	}
	if got.Status != Live {
		t.Errorf("a reference just written is %q", got.Status)
	}
}

// "Deduplication is scoped per namespace, so that two namespaces never share a physical
// object and an existence check can never reveal what another namespace holds."
func TestTwoNamespacesWritingTheSameBytesHoldTwoObjects(t *testing.T) {
	pool, super := opened(t)

	for _, c := range []struct{ namespace, run string }{
		{"finance", financeRun},
		{"team-ops", opsRun},
	} {
		err := pool.In(t.Context(), c.namespace, func(ctx context.Context, ns *NS) error {
			_, err := ns.WriteArtifact(ctx, Reference{
				URI:    uri(c.run, "archive", "out", "report.pdf"),
				Digest: digestOf("b"), Size: 12, MediaType: "application/pdf",
				For: time.Hour,
			})
			return err
		})
		if err != nil {
			t.Fatalf("%s: %s", c.namespace, err)
		}
	}

	// Read as the superuser, since the whole point is that neither namespace can see
	// both rows.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var objects, refs int
	if err := conn.QueryRow(t.Context(),
		`select count(*), coalesce(sum(refs), 0) from artifact_objects where digest = $1`,
		"sha256:"+digestOf("b")).Scan(&objects, &refs); err != nil {
		t.Fatal(err)
	}
	if objects != 2 {
		t.Errorf("two namespaces holding the same bytes hold %d objects, and a shared one is an existence check that answers for another tenant", objects)
	}
	if refs != 2 {
		t.Errorf("the two objects are referenced %d times in total", refs)
	}
}

// A retried task and a replay that recomputes identical bytes both write what they always
// wrote, and neither counts twice.
func TestWritingTheSameReferenceTwiceCountsOnce(t *testing.T) {
	pool, _ := opened(t)
	r := Reference{
		URI: uri(financeRun, "archive", "out", "invoices.zip"), Digest: digestOf("c"),
		Size: 7, MediaType: "application/zip", For: time.Hour,
	}

	for i := range 3 {
		var w Written
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			var err error
			w, err = ns.WriteArtifact(ctx, r)
			return err
		})
		if err != nil {
			t.Fatalf("write %d: %s", i+1, err)
		}
		if want := i == 0; w.New != want {
			t.Errorf("write %d says new is %v", i+1, w.New)
		}
	}

	if got := refsOf(t, pool, "finance", digestOf("c")); got != 1 {
		t.Errorf("three writes of one reference left a count of %d", got)
	}
}

// The same URI naming different bytes is a URI that stopped meaning one thing.
func TestOneURICannotNameTwoDigests(t *testing.T) {
	pool, _ := opened(t)
	u := uri(financeRun, "archive", "out", "invoices.zip")
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.WriteArtifact(ctx, Reference{URI: u, Digest: digestOf("d"), Size: 1, For: time.Hour}); err != nil {
			return err
		}
		_, err := ns.WriteArtifact(ctx, Reference{URI: u, Digest: digestOf("e"), Size: 1, For: time.Hour})
		return err
	})
	if err == nil {
		t.Fatal("one URI took two digests")
	}
	if !strings.Contains(err.Error(), "stopped meaning one thing") {
		t.Errorf("the refusal reads %q", err)
	}
}

// "retain is capped by the namespace quota and cannot exceed it; a workflow may always ask
// for less."
func TestRetainIsCappedByTheNamespace(t *testing.T) {
	pool, super := opened(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update namespaces set max_retention_days = 7 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		ask  time.Duration
		want time.Duration
	}{
		{"asking-for-more", 90 * 24 * time.Hour, 7 * 24 * time.Hour},
		{"asking-for-less", 24 * time.Hour, 24 * time.Hour},
	} {
		var w Written
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			var err error
			w, err = ns.WriteArtifact(ctx, Reference{
				URI:    uri(financeRun, "archive", "out", c.name),
				Digest: digestOf("a"), Size: 4096, For: c.ask,
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if lived := time.Until(w.ExpiresAt); lived > c.want+time.Minute || lived < c.want-time.Minute {
			t.Errorf("%s asked for %s under a ceiling of seven days and got %s", c.name, c.ask, lived)
		}
	}
}

// The one-shot artifact, end to end: a fetch counts, the last one retires the reference on
// the spot, and what comes after is gone rather than absent.
func TestAOneShotArtifactIsGoneOnTheLastFetch(t *testing.T) {
	pool, _ := opened(t)
	u := uri(financeRun, "render", "out", "payslips.pdf")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{
			URI: u, Digest: digestOf("f"), Size: 99, MediaType: "application/pdf",
			For: 24 * time.Hour, Fetches: 2,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Resolving is not fetching. A budget is spent by a response that completed, and
	// looking the artifact up is neither.
	for range 3 {
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			_, err := ns.Resolve(ctx, u)
			return err
		})
		if err != nil {
			t.Fatalf("resolving a two-fetch artifact: %s", err)
		}
	}

	for i, want := range []int{1, 0} {
		var left int
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			var err error
			left, err = ns.Fetched(ctx, u)
			return err
		})
		if err != nil {
			t.Fatalf("fetch %d: %s", i+1, err)
		}
		if left != want {
			t.Errorf("after fetch %d the budget is %d and should be %d", i+1, left, want)
		}
	}

	var got Resolved
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		got, err = ns.Resolve(ctx, u)
		return err
	})
	if !errors.Is(err, ErrGone) {
		t.Fatalf("a spent artifact answers %v, and the difference between gone and never there is the difference between 410 and 404", err)
	}
	if got.Status != Collected {
		t.Errorf("a spent artifact is %q, and the word the page uses is collected", got.Status)
	}
	// "the run detail keeps showing the artifact's name, size and digest with its
	// collection recorded".
	if got.Size != 99 || got.Digest != digestOf("f") || got.RetiredAt.IsZero() {
		t.Errorf("what is left of a collected artifact is %+v", got)
	}

	// And the object is gone from nobody: the count came down, the bytes did not.
	if refs := refsOf(t, pool, "finance", digestOf("f")); refs != 0 {
		t.Errorf("the count behind a collected artifact is %d", refs)
	}
	if !collectableNow(t, pool, "finance", digestOf("f")) {
		t.Error("an object at a count of zero is not marked collectable")
	}
}

// Nothing of that name, which is a 404 and not a 410.
func TestAnArtifactThatNeverExistedIsNotGone(t *testing.T) {
	pool, _ := opened(t)
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Resolve(ctx, uri(financeRun, "archive", "out", "nothing.txt"))
		return err
	})
	if !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("an artifact nobody wrote answers %v", err)
	}
}

// "two runs that produced identical bytes share one object, and expiring one run's output
// must not reach into the other's."
func TestExpiringOneReferenceLeavesTheOtherReadable(t *testing.T) {
	pool, super := opened(t)
	shared := digestOf("9")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		for _, name := range []string{"first.bin", "second.bin"} {
			if _, err := ns.WriteArtifact(ctx, Reference{
				URI: uri(financeRun, "archive", "out", name), Digest: shared, Size: 3,
				For: time.Hour,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, pool, "finance", shared); got != 2 {
		t.Fatalf("two references onto one object count %d", got)
	}

	// One of them ran out while the other did not.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifacts set expires_at = now() - interval '1 minute' where name = 'first.bin'`); err != nil {
		t.Fatal(err)
	}

	retired, err := pool.ExpireArtifacts(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("the purge retired %d references and one had run out", retired)
	}
	if got := refsOf(t, pool, "finance", shared); got != 1 {
		t.Errorf("after expiring one of two references the count is %d", got)
	}
	if collectableNow(t, pool, "finance", shared) {
		t.Error("an object one run still references is marked collectable")
	}

	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.Resolve(ctx, uri(financeRun, "archive", "out", "second.bin")); err != nil {
			return err
		}
		_, err := ns.Resolve(ctx, uri(financeRun, "archive", "out", "first.bin"))
		if !errors.Is(err, ErrGone) {
			t.Errorf("the expired reference answers %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// "once any input to a step it could restart from is gone, the run is marked as
	// replayable from the start only".
	var only bool
	if err := conn.QueryRow(t.Context(),
		`select replay_from_start_only from runs where namespace = 'finance' and id = $1`,
		financeRun).Scan(&only); err != nil {
		t.Fatal(err)
	}
	if !only {
		t.Error("a run that lost an artifact still offers a replay from the middle")
	}
}

// The collection protocol: claimed after the grace, never before, deleted by the caller, and
// confirmed afterwards.
func TestAnObjectIsCollectedOnlyAfterTheGrace(t *testing.T) {
	pool, super := opened(t)
	d := digestOf("7")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{
			URI: uri(financeRun, "archive", "out", "gone.bin"), Digest: d, Size: 5,
			For: time.Hour, Fetches: 1,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Fetched(ctx, uri(financeRun, "archive", "out", "gone.bin"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Collectable, and not yet collected.
	claimed, err := pool.Collectable(t.Context(), time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("an object that reached zero a moment ago was claimed under an hour's grace: %+v", claimed)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}

	claimed, err = pool.Collectable(t.Context(), time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the sweep claimed %d objects and one was due", len(claimed))
	}
	if want := "finance/sha256/" + d; claimed[0].Key != want {
		t.Errorf("the claim names %q and the store holds %q", claimed[0].Key, want)
	}

	removed, err := pool.Collected(t.Context(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("confirming one deletion removed %d rows", removed)
	}
}

// The window the grace alone does not close: a reference arrives for an object a sweep has
// already claimed. The writer is told to write the bytes again, and the confirmation leaves
// the row alone.
func TestAReferenceArrivingAfterAClaimKeepsTheObject(t *testing.T) {
	pool, super := opened(t)
	d := digestOf("8")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{
			URI: uri(financeRun, "archive", "out", "shared.bin"), Digest: d, Size: 6,
			For: time.Hour, Fetches: 1,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.Fetched(ctx, uri(financeRun, "archive", "out", "shared.bin"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifact_objects set collectable_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}

	claimed, err := pool.Collectable(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("the sweep claimed %d objects", len(claimed))
	}

	// Now somebody writes the same bytes again, between the claim and the deletion.
	var w Written
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		w, err = ns.WriteArtifact(ctx, Reference{
			URI: uri(financeRun, "render", "out", "shared.bin"), Digest: d, Size: 6,
			For: time.Hour,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !w.MustWriteBytes {
		t.Error("a reference onto an object a sweep had claimed was not told to write the bytes again, which is the one thing that closes that window")
	}

	removed, err := pool.Collected(t.Context(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("the confirmation removed %d rows, and the object had been referenced again", removed)
	}
	if got := refsOf(t, pool, "finance", d); got != 1 {
		t.Errorf("the object is referenced %d times", got)
	}
}

// Two steps publishing identical bytes share one object, which is why an envelope is counted
// rather than deleted by digest.
func TestTwoStepsPublishingTheSameEnvelopeShareOneObject(t *testing.T) {
	pool, super := opened(t)
	d := digestOf("1")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		for _, step := range []agk.Step{"archive", "render"} {
			if err := ns.PublishPorts(ctx, financeRun, step, []Published{
				{Port: "ok", Digest: d, Size: 128, Items: 2},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, pool, "finance", d); got != 2 {
		t.Fatalf("two steps publishing one envelope count %d", got)
	}

	// Republishing what was already published changes nothing.
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.PublishPorts(ctx, financeRun, "archive", []Published{
			{Port: "ok", Digest: d, Size: 128, Items: 2},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, pool, "finance", d); got != 2 {
		t.Errorf("republishing an unchanged port left a count of %d", got)
	}

	// The run expires, both steps are purged, and only then is the object collectable.
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

	purged, err := pool.PurgeEnvelopes(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 2 {
		t.Fatalf("the envelope purge took %d steps and two had expired", purged)
	}
	if got := refsOf(t, pool, "finance", d); got != 0 {
		t.Errorf("after purging both steps the count is %d", got)
	}

	// Purging twice takes nothing, which is what the stamp is for.
	again, err := pool.PurgeEnvelopes(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("the second purge took %d steps", again)
	}

	// And the digest is still readable, because it is the record of what was published.
	var ports Ports
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		ports, err = ns.PublishedPorts(ctx, financeRun, "archive")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if e := ports["ok"]; e.Digest != d || e.Items != 2 || e.PurgedAt.IsZero() {
		t.Errorf("what is left of a purged envelope is %+v", e)
	}
}

// A port republished with different bytes moves the count rather than raising a second one.
func TestRepublishingAPortMovesTheCount(t *testing.T) {
	pool, _ := opened(t)
	first, second := digestOf("2"), digestOf("3")

	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if err := ns.PublishPorts(ctx, financeRun, "archive", []Published{
			{Port: "ok", Digest: first, Size: 10, Items: 1},
		}); err != nil {
			return err
		}
		return ns.PublishPorts(ctx, financeRun, "archive", []Published{
			{Port: "ok", Digest: second, Size: 20, Items: 3},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refsOf(t, pool, "finance", first); got != 0 {
		t.Errorf("the envelope that was replaced is still counted %d times", got)
	}
	if got := refsOf(t, pool, "finance", second); got != 1 {
		t.Errorf("the envelope that replaced it is counted %d times", got)
	}
}

// The log purge, which counts nothing because nothing else names a log.
func TestTheLogPurgeClaimsAndConfirms(t *testing.T) {
	pool, super := opened(t)

	// Nothing is due while the run is going.
	due, err := pool.ExpiredLogs(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("a run that has not finished has %d logs due", len(due))
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

	due, err = pool.ExpiredLogs(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].URI == "" {
		t.Fatalf("the purge found %+v", due)
	}

	cleared, err := pool.LogsPurged(t.Context(), due)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Fatalf("confirming one purged log cleared %d rows", cleared)
	}

	// The record that there was a log outlives the log.
	var uri *string
	var lines int
	if err := conn.QueryRow(t.Context(),
		`select log_uri, log_lines from tasks where namespace = 'finance'`).Scan(&uri, &lines); err != nil {
		t.Fatal(err)
	}
	if uri != nil || lines != 812 {
		t.Errorf("after the purge the task holds uri %v and %d lines", uri, lines)
	}

	again, err := pool.ExpiredLogs(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("the second sweep found %d logs", len(again))
	}
}

// A sweep has no namespace and reaches every one of them, which is the whole reason it goes
// through the Installation door.
func TestASweepReachesEveryNamespace(t *testing.T) {
	pool, super := opened(t)

	for _, c := range []struct{ namespace, run string }{
		{"finance", financeRun},
		{"team-ops", opsRun},
	} {
		err := pool.In(t.Context(), c.namespace, func(ctx context.Context, ns *NS) error {
			_, err := ns.WriteArtifact(ctx, Reference{
				URI: uri(c.run, "archive", "out", "old.bin"), Digest: digestOf("4"),
				Size: 1, For: time.Hour,
			})
			return err
		})
		if err != nil {
			t.Fatalf("%s: %s", c.namespace, err)
		}
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update artifacts set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	retired, err := pool.ExpireArtifacts(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 2 {
		t.Errorf("the purge retired %d references across two namespaces", retired)
	}
}

// refsOf reads the count behind an object, through the door the namespace owns.
func refsOf(t *testing.T, pool *Pool, namespace, digest string) int {
	t.Helper()
	var refs int
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx,
			`select refs from artifact_objects where namespace = $1 and digest = $2`,
			namespace, "sha256:"+digest).Scan(&refs)
	})
	if err != nil {
		t.Fatalf("reading the count of %s: %s", digest, err)
	}
	return refs
}

func collectableNow(t *testing.T, pool *Pool, namespace, digest string) bool {
	t.Helper()
	var at *time.Time
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx,
			`select collectable_at from artifact_objects where namespace = $1 and digest = $2`,
			namespace, "sha256:"+digest).Scan(&at)
	})
	if err != nil {
		t.Fatalf("reading the collectability of %s: %s", digest, err)
	}
	return at != nil
}
