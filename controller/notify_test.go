package controller

import (
	"context"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// What the controller leaves for the API to turn into a push message. The two "share the
// database and nothing else", so what is tested here is the row: nobody calls anybody.

func events(t *testing.T, co *Core) []db.Event {
	t.Helper()
	var out []db.Event
	if err := co.controller.Fenced(t.Context(), co.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		out, err = w.Undelivered(ctx, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func kindsOf(events []db.Event) []db.Kind {
	out := make([]db.Kind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

// "Emits the notification events that the API turns into push messages: failure ... completion
// of a run the recipient started."
func TestARunThatFailsIsWorthWakingSomebodyFor(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	if len(taken) != 1 {
		t.Fatalf("the first pass published %d tasks", len(taken))
	}

	// Nothing yet: a run that is going is not news.
	if got := events(t, core); len(got) != 0 {
		t.Fatalf("a run that had not ended emitted %v", kindsOf(got))
	}

	core.answer(t, failed(taken[0], 1, core.now()))
	got := events(t, core)
	if len(got) != 2 {
		t.Fatalf("a failed run emitted %v", kindsOf(got))
	}
	// The failure first, because a person told a run completed before being told it failed
	// has been told two true things in the order that makes the second one confusing.
	if got[0].Kind != db.Failure || got[1].Kind != db.Completion {
		t.Errorf("the events came out as %v", kindsOf(got))
	}
	for _, e := range got {
		if e.Run != decidedRun || e.State != agk.Failed {
			t.Errorf("an event reads %+v", e)
		}
		if e.StartedBy != "alice" {
			t.Errorf("the event says the run was started by %q", e.StartedBy)
		}
		if e.Namespace != "finance" {
			t.Errorf("the event is in namespace %q", e.Namespace)
		}
	}

	// Deciding the run again emits nothing more: a run becomes terminal once, in one
	// committed decision, and the decision is what carries the event.
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	core.answer(t, succeeded(t, taken[0], core.now()))
	if again := events(t, core); len(again) != 2 {
		t.Errorf("after two more passes there are %d events", len(again))
	}
}

// A run that worked is a completion and not a failure, and one somebody cancelled is a
// completion too: they know, because they asked.
func TestWhatEachEndingEmits(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		core, q, pool, _ := deciding(t)
		createRun(t, pool)
		if err := core.Decide(t.Context(), decidedRun); err != nil {
			t.Fatal(err)
		}
		for pass := 1; pass <= 6; pass++ {
			taken := q.taken()
			if len(taken) == 0 {
				break
			}
			for _, task := range taken {
				core.answer(t, succeeded(t, task, core.now()))
			}
		}
		got := events(t, core)
		if len(got) != 1 || got[0].Kind != db.Completion || got[0].State != agk.Succeeded {
			t.Fatalf("a run that worked emitted %v", kindsOf(got))
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		core, q, pool, _ := deciding(t)
		createRun(t, pool)
		if err := core.Decide(t.Context(), decidedRun); err != nil {
			t.Fatal(err)
		}
		q.taken()
		if err := core.Cancel(t.Context(), decidedRun); err != nil {
			t.Fatal(err)
		}
		got := events(t, core)
		if len(got) != 1 || got[0].Kind != db.Completion || got[0].State != agk.Cancelled {
			t.Fatalf("a cancelled run emitted %v, and somebody asked for it", kindsOf(got))
		}
	})

	t.Run("timed out", func(t *testing.T) {
		core, q, pool, _ := decidingOn(t, boundedWorkflow)
		createRun(t, pool)
		if err := core.Decide(t.Context(), decidedRun); err != nil {
			t.Fatal(err)
		}
		q.taken()
		clock.advance(2 * time.Hour)
		if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
			t.Fatal(err)
		}
		got := events(t, core)
		if len(got) != 2 || got[0].Kind != db.Failure || got[0].State != agk.TimedOut {
			t.Fatalf("a run that ran out of time emitted %v", kindsOf(got))
		}
	})
}

// The API stamps what it has delivered, and stamping is what takes an event out of the list.
func TestDeliveringAnEventTakesItOutOfTheList(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	taken := q.taken()
	core.answer(t, failed(taken[0], 1, core.now()))

	waiting := events(t, core)
	if len(waiting) != 2 {
		t.Fatalf("%d events are waiting", len(waiting))
	}

	var stamped int
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		stamped, err = w.Delivered(ctx, waiting[:1], core.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stamped != 1 {
		t.Fatalf("stamping one delivered event stamped %d", stamped)
	}
	left := events(t, core)
	if len(left) != 1 || left[0].Kind != db.Completion {
		t.Fatalf("after delivering the failure, %v are left", kindsOf(left))
	}

	// And stamping it twice stamps nothing, so a delivery that ran twice does not look like
	// two deliveries.
	if err := core.controller.Fenced(t.Context(), core.term, func(ctx context.Context, w *db.Wide) error {
		var err error
		stamped, err = w.Delivered(ctx, waiting[:1], core.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stamped != 0 {
		t.Errorf("stamping an event that was already delivered stamped %d", stamped)
	}
}
