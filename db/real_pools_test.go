package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pool, which exists before any machine does.

func TestAPoolCarriesItsPolicyAsItWasWritten(t *testing.T) {
	pool, _ := joining(t)

	var listed []RunnerPool
	var named RunnerPool
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		if err := w.CreateRunnerPool(ctx, RunnerPool{
			Name: "sandboxed", Labels: []string{"runtime=runsc"},
			AcceptedNamespaces: []string{"finance"},
			Ceilings:           Ceilings{CPU: "0.5", Memory: "8Gi", PIDs: 512},
			CreatedBy:          "admin",
		}); err != nil {
			return err
		}
		var err error
		if listed, err = w.RunnerPools(ctx); err != nil {
			return err
		}
		named, err = w.RunnerPoolNamed(ctx, "sandboxed")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]RunnerPool{}
	var order []string
	for _, p := range listed {
		byName[p.Name] = p
		order = append(order, p.Name)
	}
	if !slices.IsSorted(order) {
		t.Errorf("the pools are listed as %v, and a listing is ordered by name", order)
	}
	made, ok := byName["sandboxed"]
	switch {
	case !ok:
		t.Fatalf("the listing holds %v", byName)
	case made.Ceilings != (Ceilings{CPU: "0.5", Memory: "8Gi", PIDs: 512}):
		// As it was written: half a core stays "0.5" and eight gibibytes stay "8Gi".
		t.Errorf("the ceilings came back as %+v", made.Ceilings)
	case made.Containment != ContainmentHardened:
		t.Errorf("a pool that named no tier is %q", made.Containment)
	case made.CreatedBy != "admin" || made.CreatedAt.IsZero():
		t.Errorf("the pool says it was created by %q at %s", made.CreatedBy, made.CreatedAt)
	}
	if named.Name != made.Name || named.Ceilings != made.Ceilings || !slices.Equal(named.AcceptedNamespaces, made.AcceptedNamespaces) {
		t.Errorf("the pool reads as %+v by its name and as %+v in the listing", named, made)
	}

	// A pool with no ceilings has none, rather than ceilings of nothing.
	if dmz := byName["dmz"]; dmz.Ceilings != (Ceilings{}) {
		t.Errorf("a pool created with no ceiling reads as %+v", dmz.Ceilings)
	}

	// A pool with no list of its own accepts every namespace, which is what an installation
	// with one pool has.
	if !byName["dmz"].Accepts("finance") {
		t.Error("a pool that names no namespace refuses one")
	}
	if made.Accepts("team-ops") {
		t.Error("a pool that names finance accepts team-ops")
	}
}

// A pool's name is its only identity, and a second pool of the same name is refused rather than
// merged into the first.
func TestAPoolNameIsTakenOnce(t *testing.T) {
	pool, _ := joining(t)
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		return w.CreateRunnerPool(ctx, RunnerPool{Name: "dmz", Labels: []string{"zone=lan"}, CreatedBy: "admin"})
	})
	if !errors.Is(err, ErrRunnerPoolExists) {
		t.Fatalf("a second pool called dmz answered %v", err)
	}
}

// The table holds a pool to the grammar the wire gives it, whatever wrote the row: a name the API
// would refuse, a label that is not key=value or a tier nobody named is refused by PostgreSQL
// itself.
func TestThePoolTableRefusesWhatTheWireRefuses(t *testing.T) {
	pool, super := joining(t)

	for _, c := range []struct {
		name string
		pool RunnerPool
	}{
		{"a name in capitals", RunnerPool{Name: "DMZ"}},
		{"a name with an underscore", RunnerPool{Name: "gpu_nvme"}},
		{"a name beginning with a hyphen", RunnerPool{Name: "-dmz"}},
		{"a name with two hyphens together", RunnerPool{Name: "gpu--nvme"}},
		{"a name longer than the bus names a consumer", RunnerPool{Name: strings.Repeat("a", 256)}},
		{"a label with no value", RunnerPool{Name: "lan", Labels: []string{"zone"}}},
		{"a label with an empty value", RunnerPool{Name: "lan", Labels: []string{"zone="}}},
		{"a label whose key is in capitals", RunnerPool{Name: "lan", Labels: []string{"Zone=lan"}}},
		{"a label with a space in it", RunnerPool{Name: "lan", Labels: []string{"zone=lan dmz"}}},
		{"a namespace in capitals", RunnerPool{Name: "lan", AcceptedNamespaces: []string{"Finance"}}},
		{"a cpu ceiling of no cores", RunnerPool{Name: "lan", Ceilings: Ceilings{CPU: "0"}}},
		{"a cpu ceiling in words", RunnerPool{Name: "lan", Ceilings: Ceilings{CPU: "4 cores"}}},
		{"a memory ceiling in decimal units", RunnerPool{Name: "lan", Ceilings: Ceilings{Memory: "8GB"}}},
		{"a memory ceiling in bytes", RunnerPool{Name: "lan", Ceilings: Ceilings{Memory: "8589934592"}}},
		{"a pids ceiling below one", RunnerPool{Name: "lan", Ceilings: Ceilings{PIDs: -1}}},
		{"a tier nobody named", RunnerPool{Name: "lan", Containment: "gvisor"}},
	} {
		c.pool.CreatedBy = "admin"
		err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			return w.CreateRunnerPool(ctx, c.pool)
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != checkViolation {
			t.Errorf("%s answered %v, where the table refuses it", c.name, err)
		}
	}

	// A token's labels are held by the table too, and not only by the check that they are
	// its pool's, which a row written some other way would never meet.
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx,
		`insert into join_tokens (id, pool, labels, hash, issued_by, expires_at)
		 values ('01M2AAZ9G62NQXFAFCXKRPJEH5', 'dmz', '{zone}', repeat('a', 64), 'admin', now() + interval '1 hour')`)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != checkViolation {
		t.Errorf("a token permitting the label zone was written: %v", err)
	}
}

// "Labels are not self-asserted" is a chain, and this is its first link: a token draws from the
// pool, a machine draws from the token, so a label reaches a machine only where an administrator
// wrote it on a pool first.
func TestATokenCannotPermitALabelItsPoolDoesNotCarry(t *testing.T) {
	pool, _ := joining(t)
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		now := time.Now().UTC()
		_, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz", "zone=lan"}, "admin", now, now.Add(time.Hour))
		return err
	})
	if !errors.Is(err, ErrNotThePoolsLabel) {
		t.Fatalf("a token permitting a label its pool does not carry answered %v", err)
	}
}

// A token is issued at the moment its caller says, and expires when its caller says, so the hour
// it is given is the hour it has.
func TestATokenLivesFromTheMomentItIsIssued(t *testing.T) {
	pool, super := joining(t)
	at := time.Date(2026, 9, 10, 6, 12, 0, 0, time.UTC)

	var issued JoinToken
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		issued, err = w.IssueJoinToken(ctx, "dmz", nil, "admin", at, at.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !issued.IssuedAt.Equal(at) || !issued.ExpiresAt.Equal(at.Add(time.Hour)) {
		t.Errorf("the token says it was issued at %s and expires at %s", issued.IssuedAt, issued.ExpiresAt)
	}

	// And the row says what the answer said, rather than the database's own clock.
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var stored time.Time
	if err := conn.QueryRow(ctx, `select issued_at from join_tokens where id = $1`, issued.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(at) {
		t.Errorf("the row says the token was issued at %s, and it was issued at %s", stored, at)
	}

	// One that would expire before it is issued is not a token at all.
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.IssueJoinToken(ctx, "dmz", nil, "admin", at, at)
		return err
	})
	if err == nil {
		t.Error("a token was issued that expires at the moment it is issued")
	}
}

func TestAJoinTokenForAPoolNobodyCreated(t *testing.T) {
	pool, _ := joining(t)
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		now := time.Now().UTC()
		_, err := w.IssueJoinToken(ctx, "imaginary", nil, "admin", now, now.Add(time.Hour))
		return err
	})
	if !errors.Is(err, ErrNoRunnerPool) {
		t.Fatalf("a token for a pool nobody created answered %v", err)
	}
}
