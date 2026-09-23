package secret_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/secret"
	"github.com/jackc/pgx/v5"
)

// The built-in store against a real PostgreSQL: a value written on one side opens on the other,
// the table holds nothing that is the value, and a row that is not where it was sealed does not
// open.

// stored is a database with the two namespaces the tests use, and the superuser that can reach
// past the policy to do what an attacker holding the table would.
func stored(t *testing.T) (*db.Pool, *pgx.Conn) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	return pool, conn
}

func keyring(t *testing.T, current *secret.Master, past ...*secret.Master) *secret.Keyring {
	t.Helper()
	k, err := secret.NewKeyring(current, past...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func builtin(t *testing.T, pool *db.Pool, keys *secret.Keyring) *secret.Builtin {
	t.Helper()
	b, err := secret.NewBuiltin(pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// written puts a value in the built-in store the way a declaration's PUT does, inside a
// transaction of the namespace.
func written(t *testing.T, pool *db.Pool, b *secret.Builtin, namespace, name, value string) {
	t.Helper()
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *db.NS) error {
		return b.Write(ctx, ns, name, []byte(value))
	}); err != nil {
		t.Fatal(err)
	}
}

func inTheStore(name string) db.Declaration {
	return db.Declaration{Name: name, Provider: api.ProviderBuiltin}
}

// A value written into the built-in store opens where it was written, and rotating it is writing
// again. What the table holds is none of it in the clear.
func TestAValueInTheBuiltInStoreOpensWhereItWasWritten(t *testing.T) {
	pool, conn := stored(t)
	b := builtin(t, pool, keyring(t, master(t, "2026-09")))
	const first, rotated = "bk_live_first_notreal", "bk_live_rotated_notreal"

	for _, value := range []string{first, rotated} {
		written(t, pool, b, "finance", "billing", value)
		got, err := b.Read(t.Context(), "finance", inTheStore("billing"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != value {
			t.Errorf("the built-in store answered %q, and %q was written", got, value)
		}
	}

	// Every column of the row, read the way a dump of the database would read it.
	var version int
	var under string
	var parts [5][]byte
	if err := conn.QueryRow(t.Context(),
		`select version, master, salt, wrapped_key, wrap_nonce, ciphertext, nonce
		 from secret_values where namespace = 'finance' and name = 'billing'`).
		Scan(&version, &under, &parts[0], &parts[1], &parts[2], &parts[3], &parts[4]); err != nil {
		t.Fatal(err)
	}
	if version != 2 || under != "2026-09" {
		t.Errorf("the row is at version %d under %q, and it was written twice under 2026-09", version, under)
	}
	for _, part := range parts {
		for _, value := range []string{first, rotated} {
			if bytes.Contains(part, []byte(value)) {
				t.Errorf("the table holds %q in the clear", value)
			}
		}
	}
}

// A sealed value opens only in the namespace, under the name and at the version it was sealed
// for. Copied by somebody holding the table into another namespace's row, another secret's, or
// back over a later write of its own, it refuses to open, and the refusal names the secret and
// carries neither value.
func TestASealedValueMovedElsewhereDoesNotOpen(t *testing.T) {
	pool, conn := stored(t)
	b := builtin(t, pool, keyring(t, master(t, "2026-09")))
	const leaked, rotated = "bk_live_leaked_notreal", "bk_live_rotated_notreal"

	written(t, pool, b, "finance", "billing", leaked)
	var beforeMaster string
	var before [5][]byte
	if err := conn.QueryRow(t.Context(),
		`select master, salt, wrapped_key, wrap_nonce, ciphertext, nonce
		 from secret_values where namespace = 'finance' and name = 'billing'`).
		Scan(&beforeMaster, &before[0], &before[1], &before[2], &before[3], &before[4]); err != nil {
		t.Fatal(err)
	}
	written(t, pool, b, "finance", "billing", rotated)
	written(t, pool, b, "finance", "ledger", "gl_live_notreal")
	written(t, pool, b, "team-ops", "billing", "to_live_notreal")
	written(t, pool, b, "team-ops", "billing", "to_live_again_notreal")

	// finance's billing, sealed at version two, copied whole into two rows that are also at
	// version two or raised to it, so that only the namespace or only the name differs.
	for _, stmt := range []string{
		`update secret_values set version = 2 where namespace = 'finance' and name = 'ledger'`,
		`update secret_values t
		   set master = s.master, salt = s.salt, wrapped_key = s.wrapped_key,
		       wrap_nonce = s.wrap_nonce, ciphertext = s.ciphertext, nonce = s.nonce
		 from secret_values s
		 where s.namespace = 'finance' and s.name = 'billing'
		   and ((t.namespace = 'team-ops' and t.name = 'billing') or (t.namespace = 'finance' and t.name = 'ledger'))`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ namespace, name string }{{"team-ops", "billing"}, {"finance", "ledger"}} {
		got, err := b.Read(t.Context(), c.namespace, inTheStore(c.name))
		if err == nil {
			t.Errorf("finance's billing, copied into %s/%s, opened there as %q", c.namespace, c.name, got)
			continue
		}
		said := err.Error()
		if !strings.Contains(said, c.namespace+"/"+c.name) || strings.Contains(said, rotated) || strings.Contains(said, leaked) {
			t.Errorf("the refusal reads %q", said)
		}
	}

	// And what was sealed before the rotation, put back over the rotated value at the version
	// the row has moved on to.
	if _, err := conn.Exec(t.Context(),
		`update secret_values
		   set master = $1, salt = $2, wrapped_key = $3, wrap_nonce = $4, ciphertext = $5, nonce = $6
		 where namespace = 'finance' and name = 'billing'`,
		beforeMaster, before[0], before[1], before[2], before[3], before[4]); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Read(t.Context(), "finance", inTheStore("billing")); err == nil {
		t.Errorf("the value sealed before the rotation, put back, opened as %q", got)
	} else if strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), rotated) {
		t.Errorf("the refusal reads %q", err)
	}
}

// A value sealed under a key the installation has since rotated away from keeps opening while the
// ring holds that key, and once the ring no longer does the refusal says so, naming the secret.
func TestAValueOpensUnderWhicheverKeyOfTheRingSealedIt(t *testing.T) {
	pool, _ := stored(t)
	was, now := master(t, "2026-01"), master(t, "2026-09")
	written(t, pool, builtin(t, pool, keyring(t, was)), "finance", "billing", "bk_live_notreal")

	got, err := builtin(t, pool, keyring(t, now, was)).Read(t.Context(), "finance", inTheStore("billing"))
	if err != nil || string(got) != "bk_live_notreal" {
		t.Fatalf("a value under the previous key read as %q, %v", got, err)
	}

	_, err = builtin(t, pool, keyring(t, now)).Read(t.Context(), "finance", inTheStore("billing"))
	if !errors.Is(err, secret.ErrNotMine) || !strings.Contains(err.Error(), "finance/billing") {
		t.Errorf("a value under a key the ring no longer holds answered %v", err)
	}
}

// A secret declared in the built-in store with nothing written to it, or whose value was forgotten,
// is a secret the store does not hold, and the refusal says which.
func TestABuiltinSecretWithNoValueIsNotHeld(t *testing.T) {
	pool, _ := stored(t)
	b := builtin(t, pool, keyring(t, master(t, "2026-09")))

	_, err := b.Read(t.Context(), "finance", inTheStore("billing"))
	if !errors.Is(err, api.ErrNoSecret) || !strings.Contains(err.Error(), "finance/billing") {
		t.Errorf("a value never written answered %v", err)
	}

	written(t, pool, b, "finance", "billing", "bk_live_notreal")
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return b.Forget(ctx, ns, "billing")
	}); err != nil {
		t.Fatal(err)
	}
	_, err = b.Read(t.Context(), "finance", inTheStore("billing"))
	if !errors.Is(err, api.ErrNoSecret) || strings.Contains(err.Error(), "bk_live_notreal") {
		t.Errorf("a forgotten value answered %v", err)
	}

	// And a declaration that is not the built-in store's is not read from it.
	for _, d := range []db.Declaration{
		{Name: "billing", Provider: api.ProviderEnv, Path: "AGENTIIK_SECRET_FINANCE_BILLING"},
		{Name: "billing", Provider: api.ProviderBuiltin, Path: "finance/billing"},
	} {
		if _, err := b.Read(t.Context(), "finance", d); err == nil {
			t.Errorf("the built-in store read %+v", d)
		}
	}
}

// The built-in store is a database and a keyring, and is refused without either.
func TestTheBuiltInStoreNeedsADatabaseAndAKeyring(t *testing.T) {
	if _, err := secret.NewBuiltin(nil, keyring(t, master(t, "2026-09"))); err == nil {
		t.Error("a built-in store with no database was built")
	}
	if _, err := secret.NewBuiltin(&db.Pool{}, nil); err == nil {
		t.Error("a built-in store with no keyring was built")
	}
}
