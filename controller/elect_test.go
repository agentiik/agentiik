package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The election, against a real PostgreSQL, because an advisory lock is a property of a
// connection and there is nothing about that a fake could be wrong in the same way.

// One at a time, which is the whole sentence: a second instance waits, and takes over the
// moment the first lets go.
func TestOnlyOneControllerLeadsAtATime(t *testing.T) {
	pool, _ := dbtest.Open(t)

	first, err := New(pool, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(pool, "second")
	if err != nil {
		t.Fatal(err)
	}
	second.Poll = 20 * time.Millisecond

	leading := make(chan db.Term, 1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := first.Lead(t.Context(), func(ctx context.Context, term db.Term) error {
			leading <- term
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("the first controller: %s", err)
		}
	}()

	var held db.Term
	select {
	case held = <-leading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first controller never took the lock")
	}
	if held.Token < 1 || held.Holder != "first" {
		t.Fatalf("the first term is %+v", held)
	}

	// While the first holds it, the second waits rather than leading.
	took := make(chan db.Term, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := second.Lead(t.Context(), func(ctx context.Context, term db.Term) error {
			took <- term
			return nil
		})
		if err != nil {
			t.Errorf("the second controller: %s", err)
		}
	}()

	select {
	case term := <-took:
		t.Fatalf("two controllers led at once: the second took term %+v while the first held it", term)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case term := <-took:
		if term.Token <= held.Token {
			t.Errorf("the second term is %d and the first was %d, and the counter only goes up", term.Token, held.Token)
		}
		if term.Holder != "second" {
			t.Errorf("the second term is held by %q", term.Holder)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second controller never took over")
	}
	wg.Wait()
}

// A lock is a session's, and a session can end under a controller that is still deciding: a
// backend terminated, a connection a proxy recycled. Nothing the term does uses that session, so
// the holder asks it on every poll whether it still holds the lock, and the term ends with
// ErrLockLost when it cannot say so, rather than going on deciding with no lock held.
func TestATermEndsWhenItsLockSessionEnds(t *testing.T) {
	pool, super := dbtest.Open(t)
	c, err := New(pool, "cut-off")
	if err != nil {
		t.Fatal(err)
	}
	c.Poll = 50 * time.Millisecond

	leading := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- c.Lead(t.Context(), func(ctx context.Context, term db.Term) error {
			close(leading)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-leading:
	case <-time.After(10 * time.Second):
		t.Fatal("the controller never took the lock")
	}

	var terminated bool
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select coalesce(bool_and(pg_terminate_backend(pid)), false) from pg_locks
		 where locktype = 'advisory' and granted
		   and database = (select oid from pg_database where datname = current_database())`).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("the session holding the lock could not be terminated: %v", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, ErrLockLost) {
			t.Errorf("the term whose lock session was terminated ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the term went on for 10s after the session holding its lock was terminated")
	}
}

// "every controller write carries the lock's acquisition counter as a fencing token and a
// write bearing an older counter is refused. Election alone would not be safe; the fencing
// token is what makes it so."
func TestAFormerHolderCannotWrite(t *testing.T) {
	pool, _ := dbtest.Open(t)

	was, err := New(pool, "was-active")
	if err != nil {
		t.Fatal(err)
	}
	var old db.Term
	if err := was.Lead(t.Context(), func(ctx context.Context, term db.Term) error {
		old = term
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Its write works while it is the holder, which is what makes the refusal below mean
	// something.
	if err := was.Fenced(t.Context(), old, func(ctx context.Context, w *db.Wide) error {
		return nil
	}); err != nil {
		t.Fatalf("the holder's own write was refused: %s", err)
	}

	now, err := New(pool, "is-active")
	if err != nil {
		t.Fatal(err)
	}
	if err := now.Lead(t.Context(), func(ctx context.Context, term db.Term) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// And now the former holder, which has not noticed, writes.
	err = was.Fenced(t.Context(), old, func(ctx context.Context, w *db.Wide) error {
		t.Error("a former holder's write reached its body, and the fence is what should have stopped it before anything was read")
		return nil
	})
	if !errors.Is(err, db.ErrFenced) {
		t.Fatalf("a former holder's write answered %v", err)
	}
	if !strings.Contains(err.Error(), "is-active") {
		t.Errorf("the refusal does not name who holds the term: %q", err)
	}
}

// A term is taken once per acquisition, and the counter only goes up. A counter that could
// repeat would be a fencing token that lets a former holder through.
func TestTheTermCounterOnlyGoesUp(t *testing.T) {
	pool, _ := dbtest.Open(t)
	c, err := New(pool, "one")
	if err != nil {
		t.Fatal(err)
	}

	var seen []int64
	for range 4 {
		if err := c.Lead(t.Context(), func(ctx context.Context, term db.Term) error {
			seen = append(seen, term.Token)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("the counter went %v", seen)
		}
	}
	if seen[0] < 1 {
		t.Errorf("the first term is %d, and a term nobody has taken is zero", seen[0])
	}
}

// A controller with no name is refused, because two instances each believing they are active
// is exactly the moment somebody needs to tell them apart.
func TestAControllerIsNamed(t *testing.T) {
	if _, err := New(nil, "named"); err == nil {
		t.Error("a controller was built with no database")
	}
	pool, _ := dbtest.Open(t)
	if _, err := New(pool, ""); err == nil {
		t.Error("a controller was built with no name")
	}
}

// The fencing token is only as good as the number of writes that carry it, so this package
// reaches the database in exactly one place. A second call site would be something a former
// holder could still do, and a read is not exempt: a controller reads in order to decide.
func TestOnlyOnePlaceReachesTheDatabase(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	opens := regexp.MustCompile(`pool\.Installation\(`)
	var found []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for range opens.FindAllString(string(body), -1) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("this package opens a transaction in %d places, %v, and the fencing token is only as good as the number of writes that carry it", len(found), found)
	}
	if found[0] != "elect.go" {
		t.Errorf("the one door is in %s, and it belongs beside the election it is fenced by", found[0])
	}
}

// The fence is a row lock and not a read, which is what keeps a write and a takeover from
// overlapping: a controller taking the term waits for an in-flight write to finish, and
// everything that starts afterwards is refused.
func TestATakeoverWaitsForAWriteAlreadyInFlight(t *testing.T) {
	pool, _ := dbtest.Open(t)
	c, err := New(pool, "holder")
	if err != nil {
		t.Fatal(err)
	}
	var term db.Term
	if err := c.Lead(t.Context(), func(ctx context.Context, got db.Term) error {
		term = got
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	inside := make(chan struct{})
	finish := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- c.Fenced(t.Context(), term, func(ctx context.Context, w *db.Wide) error {
			close(inside)
			<-finish
			return nil
		})
	}()
	<-inside

	took := make(chan db.Term, 1)
	go func() {
		next, err := pool.BeginTerm(t.Context(), "taking-over")
		if err != nil {
			t.Errorf("taking the term: %s", err)
			return
		}
		took <- next
	}()

	select {
	case next := <-took:
		t.Fatalf("the term was taken to %d while a write was still in flight", next.Token)
	case <-time.After(300 * time.Millisecond):
	}

	close(finish)
	if err := <-written; err != nil {
		t.Fatalf("the write that was in flight: %s", err)
	}
	select {
	case next := <-took:
		if next.Token <= term.Token {
			t.Errorf("the term went from %d to %d", term.Token, next.Token)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the term was never taken after the write finished")
	}
}
