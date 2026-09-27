package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// max_artifact_bytes, against a real PostgreSQL: "Total live artifact storage; beyond it, new
// writes are refused", counted so that two writes at once cannot both take the last of it.

// bounded opens the artifact fixture with finance held to limit bytes, and answers the superuser's
// connection for moving clocks.
func bounded(t *testing.T, limit int64) (*Pool, *pgx.Conn) {
	t.Helper()
	pool, super := opened(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.WithoutCancel(t.Context())) })
	if _, err := conn.Exec(t.Context(),
		`update namespaces set max_artifact_bytes = $1 where name = 'finance'`, limit); err != nil {
		t.Fatal(err)
	}
	return pool, conn
}

// written records one reference of finance's run, of size bytes, living an hour.
func written(t *testing.T, pool *Pool, name, digest string, size int64) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.WriteArtifact(ctx, Reference{
			URI: uri(financeRun, "archive", "out", name), Digest: digest, Size: size, For: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// makeRoom makes room in a namespace, in a transaction of its own, for a write of up to length
// bytes under a policy lasting an hour.
func makeRoom(t *testing.T, pool *Pool, namespace, digest string, length int64) (Room, error) {
	t.Helper()
	return makeRoomFor(t, pool, namespace, Upload{Digest: digest, Length: length, Until: time.Now().Add(time.Hour)})
}

func makeRoomFor(t *testing.T, pool *Pool, namespace string, u Upload) (Room, error) {
	t.Helper()
	var room Room
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		room, err = ns.MakeRoom(ctx, u)
		return err
	})
	return room, err
}

// settled settles room in finance, at size bytes stored, or given back where size is negative.
func settled(t *testing.T, pool *Pool, room Room, size int64) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if size < 0 {
			return ns.Unwritten(ctx, room)
		}
		return ns.Stored(ctx, room, size)
	}); err != nil {
		t.Fatal(err)
	}
}

// aged makes finance's last count older than it is, so that the next write takes it again.
func aged(t *testing.T, conn *pgx.Conn, by string) {
	t.Helper()
	if _, err := conn.Exec(t.Context(),
		`update artifact_room set counted_at = counted_at - $1::interval where namespace = 'finance'`, by); err != nil {
		t.Fatal(err)
	}
}

// heldOf is what finance's room says it holds.
func heldOf(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var held int64
	if err := conn.QueryRow(t.Context(), `select held from artifact_room where namespace = 'finance'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	return held
}

// A namespace's live artifacts are counted once per digest, and an object that would take them
// past the quota is refused with what the namespace holds; one that fits is given room, and one
// the namespace already holds needs none, however full it is. A namespace that sets no quota is
// refused nothing and holds nothing.
func TestAnObjectPastMaxArtifactBytesIsRefused(t *testing.T) {
	pool, _ := bounded(t, 1000)
	// Two references to one object of 600 bytes, which is 600 bytes held and not 1,200.
	written(t, pool, "first.bin", digestOf("a"), 600)
	written(t, pool, "second.bin", digestOf("a"), 600)

	_, err := makeRoom(t, pool, "finance", digestOf("b"), 401)
	var none *NoRoom
	if !errors.As(err, &none) {
		t.Fatalf("401 bytes more than 600 of 1,000 answered %v", err)
	}
	if none.Held != 600 || none.Limit != 1000 || none.Asked != 401 || !strings.Contains(err.Error(), "max_artifact_bytes") {
		t.Errorf("the refusal says %+v: %s", none, err)
	}

	room, err := makeRoom(t, pool, "finance", digestOf("b"), 400)
	if err != nil || !room.Held() || room.Bound() != 400 {
		t.Fatalf("400 bytes more than 600 of 1,000 answered %+v, %v", room, err)
	}
	// The room is held now, so the next object has none left.
	if _, err := makeRoom(t, pool, "finance", digestOf("c"), 1); !errors.As(err, &none) || none.Held != 1000 {
		t.Errorf("one byte more than the 1,000 held answered %v", err)
	}
	// And bytes the namespace holds as a live artifact take no room: a replay writing what it
	// wrote before.
	if room, err := makeRoom(t, pool, "finance", digestOf("a"), 5000); err != nil || room.Held() {
		t.Errorf("writing again an artifact the namespace holds answered %+v, %v", room, err)
	}
	// team-ops sets no quota.
	if room, err := makeRoom(t, pool, "team-ops", digestOf("d"), 1<<40); err != nil || room.Held() {
		t.Errorf("a namespace with no quota answered %+v, %v", room, err)
	}
}

// Live is what Resolve calls live: an artifact past its expiry holds no room, whether or not a
// sweep has retired it, and neither does one retired, or an upload that lapsed.
func TestWhatIsNoLongerLiveHoldsNoRoom(t *testing.T) {
	pool, conn := bounded(t, 1000)
	written(t, pool, "lapsed.bin", digestOf("a"), 600)
	written(t, pool, "retired.bin", digestOf("b"), 300)
	if _, err := makeRoom(t, pool, "finance", digestOf("c"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := makeRoom(t, pool, "finance", digestOf("d"), 1); !errors.As(err, new(*NoRoom)) {
		t.Fatalf("the namespace was not full to begin with: %v", err)
	}

	for _, stmt := range []string{
		`update artifacts set expires_at = now() - interval '1 second' where name = 'lapsed.bin'`,
		`update artifacts set status = 'collected', retired_at = now() where name = 'retired.bin'`,
		`update artifact_uploads set until = now() - interval '1 second'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	// Found at the next count, which a write about to be refused takes again once the last is a
	// second old.
	aged(t, conn, "2 seconds")
	room, err := makeRoom(t, pool, "finance", digestOf("d"), 1000)
	if err != nil || !room.Held() {
		t.Fatalf("the whole quota, once nothing live was left, answered %+v, %v", room, err)
	}
	// The lapsed upload was let go, and not only left out of the count.
	var left int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from artifact_uploads where digest = 'sha256:' || $1`, digestOf("c")).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Error("a lapsed upload was left in the table")
	}
}

// Room is made at the most an object may be and counted at its size once its bytes are in; room
// made for bytes that never arrived is given back. Both are there for the very next write, and not
// only at the next count.
func TestRoomIsSettledAtTheSizeStoredOrGivenBack(t *testing.T) {
	pool, conn := bounded(t, 1000)

	stored, err := makeRoom(t, pool, "finance", digestOf("a"), 900)
	if err != nil {
		t.Fatal(err)
	}
	settled(t, pool, stored, 300)
	unwritten, err := makeRoom(t, pool, "finance", digestOf("b"), 700)
	if err != nil {
		t.Fatalf("700 bytes beside an object stored at 300 of the 900 it was given answered %v", err)
	}
	settled(t, pool, unwritten, -1)
	if _, err := makeRoom(t, pool, "finance", digestOf("c"), 700); err != nil {
		t.Errorf("700 bytes once the room of an unwritten object was given back answered %v", err)
	}
	if held := heldOf(t, conn); held != 1000 {
		t.Errorf("finance holds %d, and 300 stored and 700 on their way are 1,000", held)
	}
}

// Two writes of one object each hold their own room, the object counting once at the most either
// may be: the second is held to what it may take beyond the first's room, and the first failing
// leaves the second's bytes counted.
func TestTwoWritesOfOneObjectEachHoldTheirOwnRoom(t *testing.T) {
	pool, conn := bounded(t, 2000)
	first, err := makeRoom(t, pool, "finance", digestOf("a"), 400)
	if err != nil {
		t.Fatal(err)
	}
	// 5,000 bytes under a key another write holds 400 for is 4,600 more than fits.
	if _, err := makeRoom(t, pool, "finance", digestOf("a"), 5000); !errors.As(err, new(*NoRoom)) {
		t.Fatalf("5,000 bytes of an object another write holds 400 for answered %v", err)
	}
	second, err := makeRoom(t, pool, "finance", digestOf("a"), 300)
	if err != nil || !second.Held() || second.Bound() != 300 {
		t.Fatalf("300 bytes of an object another write holds 400 for answered %+v, %v", second, err)
	}
	if held := heldOf(t, conn); held != 400 {
		t.Errorf("two writes of one object hold %d, and the object counts once, at 400", held)
	}
	settled(t, pool, first, -1)
	if held := heldOf(t, conn); held != 300 {
		t.Errorf("the first write failing leaves %d held, and the second's 300 are still on their way", held)
	}
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 1701); !errors.As(err, new(*NoRoom)) {
		t.Errorf("1,701 bytes beside the second write's 300 answered %v", err)
	}
	aged(t, conn, "2 seconds")
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 1700); err != nil {
		t.Errorf("1,700 bytes beside the second write's 300, counted again, answered %v", err)
	}
}

// An upload is counted past the expiry of the policy it was written with, for the quarter of an
// hour its result may take to be heard, and not after.
func TestAnUploadIsCountedAQuarterOfAnHourPastItsPolicy(t *testing.T) {
	pool, conn := bounded(t, 1000)
	if _, err := makeRoomFor(t, pool, "finance", Upload{Digest: digestOf("a"), Length: 600, Until: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	aged(t, conn, "2 minutes")
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 401); !errors.As(err, new(*NoRoom)) {
		t.Errorf("401 bytes beside an upload whose policy expired a minute ago answered %v", err)
	}
	if _, err := conn.Exec(t.Context(), `update artifact_uploads set until = until - interval '15 minutes'`); err != nil {
		t.Fatal(err)
	}
	aged(t, conn, "2 minutes")
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 1000); err != nil {
		t.Errorf("the whole quota, a quarter of an hour after the upload's policy, answered %v", err)
	}
}

// What a namespace holds is counted whole at the first write, and again once the count is a
// minute old, so that what expired is found without a write having to be refused first.
func TestTheCountIsTakenAgainOnceItIsAMinuteOld(t *testing.T) {
	pool, conn := bounded(t, 1000)
	written(t, pool, "lapsing.bin", digestOf("a"), 600)
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 100); err != nil {
		t.Fatal(err)
	}
	if held := heldOf(t, conn); held != 700 {
		t.Fatalf("the first write counted %d, and 600 held and 100 on their way are 700", held)
	}
	if _, err := conn.Exec(t.Context(), `update artifacts set expires_at = now() - interval '1 second' where name = 'lapsing.bin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := makeRoom(t, pool, "finance", digestOf("c"), 100); err != nil {
		t.Fatal(err)
	}
	if held := heldOf(t, conn); held != 800 {
		t.Errorf("a write on a fresh count holds %d, and adds its 100 to the 700 counted", held)
	}
	aged(t, conn, "1 minute")
	if _, err := makeRoom(t, pool, "finance", digestOf("d"), 100); err != nil {
		t.Fatal(err)
	}
	if held := heldOf(t, conn); held != 300 {
		t.Errorf("a write on a count a minute old holds %d, and the three uploads of 100 are what is left", held)
	}
}

// An object of no stated length is given the room left, and refused where none is.
func TestAnObjectOfNoStatedLengthIsGivenTheRoomLeft(t *testing.T) {
	pool, _ := bounded(t, 1000)
	written(t, pool, "held.bin", digestOf("a"), 600)

	// Up to artifact_max_bytes, where it is less.
	room, err := makeRoomFor(t, pool, "finance", Upload{Digest: digestOf("b"), Length: -1, Most: 100, Until: time.Now().Add(time.Hour)})
	if err != nil || !room.Held() || room.Bound() != 100 {
		t.Fatalf("an object of no stated length, with artifact_max_bytes 100, answered %+v, %v", room, err)
	}
	room, err = makeRoom(t, pool, "finance", digestOf("c"), -1)
	if err != nil || !room.Held() || room.Bound() != 300 {
		t.Fatalf("an object of no stated length answered %+v, %v", room, err)
	}
	_, err = makeRoom(t, pool, "finance", digestOf("d"), -1)
	var none *NoRoom
	if !errors.As(err, &none) || !strings.Contains(err.Error(), "no stated length") {
		t.Errorf("an object of no stated length with no room left answered %v", err)
	}
}

// Two writes in one namespace at once are counted one after the other: the second waits until the
// first has committed the room it made, counts it, and is refused the room that is no longer
// there, although each fits alone. A namespace that sets no quota takes no lock, as before v0.3.0,
// so its writes do not wait on each other.
func TestTwoWritesAtOnceCannotBothTakeTheLastRoom(t *testing.T) {
	pool, _ := bounded(t, 1000)
	// A write before them, so that finance's room is there to be read and written rather than
	// created by the first of the two, which would hold the second at its creation.
	if _, err := makeRoom(t, pool, "finance", digestOf("c"), 1); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		namespace string
		waits     bool
	}{
		{"finance", true},
		{"team-ops", false},
	} {
		t.Run(c.namespace, func(t *testing.T) {
			made, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				first <- pool.In(context.Background(), c.namespace, func(ctx context.Context, ns *NS) error {
					if _, err := ns.MakeRoom(ctx, Upload{Digest: digestOf("a"), Length: 600, Until: time.Now().Add(time.Hour)}); err != nil {
						return err
					}
					close(made)
					<-release
					return nil
				})
			}()
			select {
			case <-made:
			case err := <-first:
				t.Fatalf("the first write answered %v", err)
			}

			// Long enough for the second to have counted had it not waited, and far longer where
			// it should not wait at all, so that a slow machine is not taken for a lock.
			patience := 500 * time.Millisecond
			if !c.waits {
				patience = 10 * time.Second
			}
			second := make(chan error, 1)
			go func() {
				_, err := makeRoom(t, pool, c.namespace, digestOf("b"), 600)
				second <- err
			}()
			var err error
			select {
			case err = <-second:
				if c.waits {
					close(release)
					t.Fatalf("the second write answered %v while the first had not committed", err)
				}
			case <-time.After(patience):
				if !c.waits {
					close(release)
					t.Fatal("a write in a namespace with no quota waited on another")
				}
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatalf("the first write answered %v", err)
			}
			if c.waits {
				err = <-second
				if !errors.As(err, new(*NoRoom)) {
					t.Errorf("the second write answered %v once the first had committed", err)
				}
			} else if err != nil {
				t.Errorf("the second write in a namespace with no quota answered %v", err)
			}
		})
	}
}

// What the quota metrics read: for every namespace, what it holds against each quota that counts
// something, beside the quota where it sets one.
func TestConsumptionReadsWhatEachNamespaceHoldsAgainstItsQuotas(t *testing.T) {
	pool, conn := bounded(t, 1000)
	written(t, pool, "held.bin", digestOf("a"), 600)
	if _, err := makeRoom(t, pool, "finance", digestOf("b"), 100); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`update namespaces set max_runs_per_hour = 50, max_concurrent_tasks = 7 where name = 'finance'`,
		// The seed's run of finance was created now, and team-ops' two hours ago.
		`update runs set created_at = now() - interval '2 hours' where namespace = 'team-ops'`,
		// One task of finance in flight and one over, which max_concurrent_tasks does not count.
		`insert into tasks (namespace, id, run_id, step, attempt, state, published_at)
		   values ('finance', '01M2T2AAAAAAAAAAAAAAAAAAAA', '` + financeRun + `', 'render', 1, 'running', now()),
		          ('finance', '01M2T3AAAAAAAAAAAAAAAAAAAA', '` + financeRun + `', 'render', 2, 'failed', now())`,
		// And an upload that lapsed, which holds nothing any more.
		`insert into artifact_uploads (namespace, id, digest, bytes, until)
		   values ('finance', '01M2V1AAAAAAAAAAAAAAAAAAAA', 'sha256:` + digestOf("c") + `', 5000, now() - interval '1 second')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}

	var got []Consumption
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		got, err = w.Consumption(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []Consumption{
		{Namespace: "finance", Tasks: 1, RunsLastHour: 1, ArtifactBytes: 700,
			MaxConcurrentTasks: 7, MaxRunsPerHour: 50, MaxArtifactBytes: 1000},
		{Namespace: "team-ops", MaxConcurrentTasks: 20},
	}
	if len(got) != len(want) {
		t.Fatalf("the consumption reads %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("namespace %s reads %+v, want %+v", want[i].Namespace, got[i], want[i])
		}
	}
}
