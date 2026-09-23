package db

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The built-in store's sealed values, against a real PostgreSQL: which version each write is
// given, what a forgotten value leaves behind, and who sees the rows. What is in them is package
// secret's to test; here they are bytes.

// sealedFor stands for what package secret would seal: bytes this package cannot read, marked with
// the version they were sealed at so that a test can tell two writes apart.
func sealedFor(version int, mark string) SealedValue {
	return SealedValue{
		Version: version, Master: "2026-09",
		Salt: []byte("salt " + mark), WrappedKey: []byte("key " + mark), WrapNonce: []byte("wrap " + mark),
		Ciphertext: []byte("sealed " + mark), Nonce: []byte("nonce " + mark),
	}
}

func writeSealed(t *testing.T, pool *Pool, namespace, name, mark string) (int, error) {
	t.Helper()
	var at int
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		return ns.WriteSealed(ctx, name, func(version int) (SealedValue, error) {
			at = version
			return sealedFor(version, mark), nil
		})
	})
	return at, err
}

func sealedOf(t *testing.T, pool *Pool, namespace, name string) (SealedValue, error) {
	t.Helper()
	var s SealedValue
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		s, err = ns.SealedValue(ctx, name)
		return err
	})
	return s, err
}

// Every write of a name is sealed at the next version and replaces the one before, and a value
// forgotten and written again carries on counting rather than starting at one again.
func TestEveryWriteOfAValueIsSealedAtAVersionOfItsOwn(t *testing.T) {
	pool, _ := opened(t)

	if _, err := sealedOf(t, pool, "finance", "billing"); !errors.Is(err, ErrNoValue) {
		t.Fatalf("a value never written reads as %v", err)
	}
	for want, mark := range []string{"first", "rotated"} {
		at, err := writeSealed(t, pool, "finance", "billing", mark)
		if err != nil {
			t.Fatal(err)
		}
		if at != want+1 {
			t.Errorf("the %s write was sealed at version %d, want %d", mark, at, want+1)
		}
	}
	got, err := sealedOf(t, pool, "finance", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if want := sealedFor(2, "rotated"); got.Version != 2 || !bytes.Equal(got.Ciphertext, want.Ciphertext) || got.Master != want.Master ||
		!bytes.Equal(got.Salt, want.Salt) || !bytes.Equal(got.WrappedKey, want.WrappedKey) ||
		!bytes.Equal(got.WrapNonce, want.WrapNonce) || !bytes.Equal(got.Nonce, want.Nonce) {
		t.Errorf("the value reads back as %+v, want %+v", got, want)
	}

	// Forgotten, it is gone, and forgetting it again or forgetting what was never written is
	// not an error.
	for _, name := range []string{"billing", "billing", "never-written"} {
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.ForgetSealed(ctx, name)
		}); err != nil {
			t.Fatalf("forgetting %s answered %v", name, err)
		}
	}
	if _, err := sealedOf(t, pool, "finance", "billing"); !errors.Is(err, ErrNoValue) {
		t.Fatalf("a forgotten value reads as %v", err)
	}

	// Written again, it is the third write of that name and not a first one, so a ciphertext
	// kept from the first write cannot open in place of this one.
	at, err := writeSealed(t, pool, "finance", "billing", "again")
	if err != nil {
		t.Fatal(err)
	}
	if at != 3 {
		t.Errorf("a value written after being forgotten was sealed at version %d, and the name had been written twice before", at)
	}
}

// A seal that fails writes nothing, and one that answers a version other than the one it was
// handed is refused rather than stored at a version its ciphertext is not bound to.
func TestASealThatFailsOrLiesWritesNothing(t *testing.T) {
	pool, _ := opened(t)
	if _, err := writeSealed(t, pool, "finance", "billing", "first"); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("the keyring would not seal")
	for what, seal := range map[string]func(int) (SealedValue, error){
		"a seal that fails":         func(int) (SealedValue, error) { return SealedValue{}, refused },
		"a seal at another version": func(v int) (SealedValue, error) { return sealedFor(v+1, "lying"), nil },
	} {
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.WriteSealed(ctx, "billing", seal)
		})
		if err == nil {
			t.Errorf("%s was written", what)
		}
	}
	got, err := sealedOf(t, pool, "finance", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || string(got.Ciphertext) != "sealed first" {
		t.Errorf("after two refused writes the value reads %+v", got)
	}
}

// Two writes of one name at once each get a version of their own: the second waits for the first
// rather than sealing the same version.
func TestTwoWritesAtOnceAreSealedAtTwoVersions(t *testing.T) {
	pool, _ := opened(t)

	const writers = 8
	versions := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			at, err := writeSealed(t, pool, "finance", "billing", "concurrent")
			if err != nil {
				t.Errorf("a concurrent write answered %v", err)
			}
			versions[i] = at
		})
	}
	wg.Wait()
	slices.Sort(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("%d concurrent writes were sealed at versions %v, and each should have had its own", writers, versions)
		}
	}
}

// The version is held by the table itself, whatever writes to it: it never goes down, a sealed
// value is whole or absent, and a value is kept only under a name a declaration could hold, in a
// namespace that exists.
func TestTheSealedValuesTableHoldsItsOwnRules(t *testing.T) {
	pool, super := opened(t)
	if _, err := writeSealed(t, pool, "finance", "billing", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSealed(t, pool, "finance", "billing", "rotated"); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for what, stmt := range map[string]string{
		"a row copied back from before the rotation": `update secret_values set version = 1, ciphertext = 'sealed first' where name = 'billing'`,
		"half a sealed value":                        `update secret_values set nonce = null where name = 'billing'`,
		"a value at version zero":                    `insert into secret_values (namespace, name, version, master, salt, wrapped_key, wrap_nonce, ciphertext, nonce) values ('finance', 'ledger', 0, 'm', 's', 'k', 'w', 'c', 'n')`,
		"a name that is a path":                      `insert into secret_values (namespace, name, version) values ('finance', 'kv/billing', 0)`,
		"a namespace nobody created":                 `insert into secret_values (namespace, name, version) values ('nowhere', 'billing', 0)`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err == nil {
			t.Errorf("%s was written", what)
		}
	}

	// A reseal under another master key keeps the version, and is not taken for going back.
	if _, err := conn.Exec(t.Context(), `update secret_values set master = '2027-01' where name = 'billing'`); err != nil {
		t.Errorf("a value resealed at its own version was refused: %s", err)
	}

	if _, err := writeSealed(t, pool, "nowhere", "billing", "first"); !errors.Is(err, ErrNoNamespace) {
		t.Errorf("a value written into a namespace nobody created answered %v", err)
	}
}

// An update is not the only way to put a row back. As the application's own role, bound to the
// namespace the rows belong to, the way the API and the controller hold the table: a row deleted
// and inserted again whole, a row deleted so that the next write counts from one, a row moved to
// another name, and a forgotten row filled at its own version are each refused, and what the
// table held is what it holds afterwards.
func TestNoRowOfTheStoreIsPutBack(t *testing.T) {
	pool, super := opened(t)
	for _, mark := range []string{"leaked", "rotated"} {
		if _, err := writeSealed(t, pool, "finance", "billing", mark); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writeSealed(t, pool, "finance", "ledger", "forgotten"); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.ForgetSealed(ctx, "ledger")
	}); err != nil {
		t.Fatal(err)
	}

	leaked := sealedFor(1, "leaked")
	for what, stmts := range map[string][]string{
		"a row deleted and inserted again whole": {
			`delete from secret_values where name = 'billing'`,
			`insert into secret_values (namespace, name, version, master, salt, wrapped_key, wrap_nonce, ciphertext, nonce)
			 values ('finance', 'billing', 1, $1, $2, $3, $4, $5, $6)`,
		},
		"a row deleted": {`delete from secret_values where name = 'billing'`},
		"a row inserted holding a value": {
			`insert into secret_values (namespace, name, version, master, salt, wrapped_key, wrap_nonce, ciphertext, nonce)
			 values ('finance', 'payroll', 1, $1, $2, $3, $4, $5, $6)`,
		},
		"a row inserted at a version no write reached": {`insert into secret_values (namespace, name, version) values ('finance', 'pension', 7)`},
		"a row moved to another name":                  {`update secret_values set name = 'payroll' where name = 'billing'`},
		"a forgotten row filled at its own version": {
			`update secret_values
			   set master = $1, salt = $2, wrapped_key = $3, wrap_nonce = $4, ciphertext = $5, nonce = $6
			 where name = 'ledger'`,
		},
	} {
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			for _, stmt := range stmts {
				var args []any
				if strings.Contains(stmt, "$1") {
					args = []any{leaked.Master, leaked.Salt, leaked.WrappedKey, leaked.WrapNonce, leaked.Ciphertext, leaked.Nonce}
				}
				if _, err := ns.tx.Exec(ctx, stmt, args...); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			t.Errorf("%s was taken from the application's role", what)
		}
	}

	// Nor by emptying the table, which the role that owns it could do and a superuser can.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `truncate secret_values`); err == nil {
		t.Error("the table was emptied")
	}

	if got, err := sealedOf(t, pool, "finance", "billing"); err != nil || got.Version != 2 || string(got.Ciphertext) != "sealed rotated" {
		t.Errorf("billing reads %+v, %v, and was last written as rotated at version 2", got, err)
	}
	if _, err := sealedOf(t, pool, "finance", "ledger"); !errors.Is(err, ErrNoValue) {
		t.Errorf("the forgotten ledger reads as %v", err)
	}
	if at, err := writeSealed(t, pool, "finance", "ledger", "written again"); err != nil || at != 2 {
		t.Errorf("ledger written again was sealed at version %d, %v, and it had been written once", at, err)
	}
}

// One namespace cannot read, overwrite or forget another's values, and cannot plant one in it; the
// table is behind the namespace policy, and the policy binds the table's owner too.
func TestAnotherNamespacesValuesAreInvisible(t *testing.T) {
	pool, super := opened(t)
	if _, err := writeSealed(t, pool, "finance", "billing", "finance's"); err != nil {
		t.Fatal(err)
	}

	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		var seen int
		if err := ns.tx.QueryRow(ctx, `select count(*) from secret_values`).Scan(&seen); err != nil {
			return err
		}
		if seen != 0 {
			t.Errorf("team-ops reads %d sealed values with no filter of its own, and it keeps none", seen)
		}
		for _, stmt := range []string{
			`update secret_values set ciphertext = 'mine'`,
			`delete from secret_values`,
		} {
			tag, err := ns.tx.Exec(ctx, stmt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%s from team-ops touched %d of finance's values", stmt, tag.RowsAffected())
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sealedOf(t, pool, "team-ops", "billing"); !errors.Is(err, ErrNoValue) {
		t.Errorf("team-ops reading finance's value answered %v", err)
	}
	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		return ns.ForgetSealed(ctx, "billing")
	}); err != nil {
		t.Fatal(err)
	}

	// The same name written in team-ops is team-ops' own, sealed at its own first version, and
	// finance's is left as it was.
	if at, err := writeSealed(t, pool, "team-ops", "billing", "team-ops'"); err != nil || at != 1 {
		t.Fatalf("team-ops writing its own billing was sealed at %d: %v", at, err)
	}
	if got, err := sealedOf(t, pool, "finance", "billing"); err != nil || string(got.Ciphertext) != "sealed finance's" {
		t.Errorf("finance's value reads %+v, %v after team-ops wrote and forgot the same name", got, err)
	}

	// Planted empty, the way a write begins a row, so that the policy is all that stands in
	// its way.
	err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx, `insert into secret_values (namespace, name, version) values ('finance', 'planted', 0)`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("a value planted in another namespace answered %v", err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var enabled, forced bool
	if err := conn.QueryRow(t.Context(),
		`select relrowsecurity, relforcerowsecurity from pg_class where relname = 'secret_values'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatal(err)
	}
	if !enabled || !forced {
		t.Errorf("secret_values has row level security enabled %v and forced %v", enabled, forced)
	}
}
