package db

import (
	"context"
	"encoding/base64"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// What Provision leaves behind, read back from PostgreSQL's own catalogues rather than from what
// its statements say they do.
//
// Whether row level security holds for the role it creates is not asked again here: database()
// provisions through it, so every namespace check in this package runs as that role.

// connect opens a connection for the test and closes it when the test ends.
func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

// login answers whether role opens with password on the database super names.
//
// The password goes into the configuration rather than into an address, since an address holds
// no character outside ASCII and a password may.
func login(t *testing.T, super, role, password string) error {
	t.Helper()
	config, err := pgx.ParseConfig(super)
	if err != nil {
		t.Fatal(err)
	}
	config.User, config.Password = role, password
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		return err
	}
	return conn.Close(t.Context())
}

// shape is everything Provision decides about a role, as one list a test can compare: its
// attributes, whether it may connect and use the schema, and each privilege it holds on each
// table of the schema.
func shape(t *testing.T, conn *pgx.Conn, role string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `
		select format('role login=%s super=%s bypassrls=%s createdb=%s createrole=%s replication=%s',
		              rolcanlogin::text, rolsuper::text, rolbypassrls::text,
		              rolcreatedb::text, rolcreaterole::text, rolreplication::text)
		  from pg_roles where rolname = $1::text
		union all
		select format('database connect=%s', has_database_privilege($1::text, current_database(), 'connect')::text)
		union all
		select format('schema usage=%s create=%s',
		              has_schema_privilege($1::text, 'public', 'usage')::text,
		              has_schema_privilege($1::text, 'public', 'create')::text)
		union all
		select format('table %s %s', c.relname, lower(a.privilege_type))
		  from pg_class c cross join lateral aclexplode(c.relacl) a
		 where c.relnamespace = 'public'::regnamespace and a.grantee = to_regrole($1::text)
		order by 1`, role)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// provisioned is the shape Provision promises, written out: a login that bypasses nothing and
// creates nothing, and read and write on every table of the schema but the migration record.
func provisioned(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(),
		`select relname::text from pg_class
		  where relnamespace = 'public'::regnamespace and relkind in ('r', 'p')
		  order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"database connect=true",
		"role login=true super=false bypassrls=false createdb=false createrole=false replication=false",
		"schema usage=true create=false",
	}
	for _, table := range tables {
		if table == "schema_migrations" {
			continue
		}
		for _, privilege := range []string{"delete", "insert", "select", "update"} {
			want = append(want, "table "+table+" "+privilege)
		}
	}
	slices.Sort(want)
	return want
}

// An installation is provisioned on every upgrade, so the second time has to be the first time
// with nothing left to do: no migration applied again, and the role neither widened nor
// narrowed nor locked out.
func TestProvisioningTwiceChangesNothing(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	conn := connect(t, super)

	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	first, err := Provision(ctx, conn, role, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(all) {
		t.Fatalf("a first provisioning applied %d migrations of the %d embedded", len(first), len(all))
	}
	before := shape(t, conn, role)
	if want := provisioned(t, conn); !slices.Equal(before, want) {
		t.Fatalf("the role came out as\n%s\nand Provision promises\n%s", strings.Join(before, "\n"), strings.Join(want, "\n"))
	}

	again, err := Provision(ctx, conn, role, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second provisioning applied %v", again)
	}
	if after := shape(t, conn, role); !slices.Equal(after, before) {
		t.Errorf("a second provisioning changed the role from\n%s\nto\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if err := login(t, super, role, "test"); err != nil {
		t.Errorf("the role no longer opens with its password after a second provisioning: %s", err)
	}
}

// The role Provision creates is one Open accepts, and the connection that created it is one Open
// refuses, which is the line between the two addresses an installation holds.
func TestOpenAcceptsTheProvisionedRoleAndRefusesTheAdministrator(t *testing.T) {
	super, app := database(t)
	ctx := t.Context()

	pool, err := Open(ctx, app)
	if err != nil {
		t.Fatalf("the provisioned role was refused: %s", err)
	}
	defer pool.Close()

	// Even through the installation door, the migration record is not the application's to
	// read, let alone to rewrite.
	err = pool.Installation(ctx, SchemaUpgrade, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `delete from schema_migrations`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the application role could delete the migration record: %v", err)
	}

	if _, err := Open(ctx, super); err == nil || !strings.Contains(err.Error(), "NOSUPERUSER") {
		t.Errorf("the administrator's connection was not refused as a superuser: %v", err)
	}
}

// A role somebody widened by hand is brought back, attribute by attribute and grant by grant, and
// a password given again replaces the one it had.
func TestProvisioningNarrowsAWidenedRoleBack(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	conn := connect(t, super)
	if _, err := Provision(ctx, conn, role, "test"); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		`alter role ` + role + ` nologin bypassrls createdb createrole replication`,
		`grant truncate, references, trigger on runs to ` + role,
		`grant all on schema_migrations to ` + role,
		`grant create on schema public to ` + role,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	if _, err := Provision(ctx, conn, role, "rotated"); err != nil {
		t.Fatal(err)
	}
	if got, want := shape(t, conn, role), provisioned(t, conn); !slices.Equal(got, want) {
		t.Errorf("the widened role came out as\n%s\nand Provision promises\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := login(t, super, role, "test"); err == nil {
		t.Error("the role still opens with the password it had before")
	}
	pool, err := Open(ctx, withCredentials(super, role, "rotated"))
	if err != nil {
		t.Fatalf("the role does not open with the password it was given: %s", err)
	}
	pool.Close()
}

// An address with no password in it is not an instruction to remove one: pgx reads a password
// from a password file where the address carries none, and a role created without one
// authenticates some other way.
func TestAnEmptyPasswordLeavesTheRolesOwn(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	conn := connect(t, super)

	// Created with none, it is given none.
	if _, err := Provision(ctx, conn, role, ""); err != nil {
		t.Fatal(err)
	}
	var none bool
	if err := conn.QueryRow(ctx, `select rolpassword is null from pg_authid where rolname = $1`, role).Scan(&none); err != nil {
		t.Fatal(err)
	}
	if !none {
		t.Error("a role created with no password was given one")
	}

	// Holding one, it keeps it.
	if _, err := Provision(ctx, conn, role, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(ctx, conn, role, ""); err != nil {
		t.Fatal(err)
	}
	if err := login(t, super, role, "test"); err != nil {
		t.Errorf("provisioning with no password removed the role's: %s", err)
	}
}

// A managed PostgreSQL has no superuser to lend: its administrator may create roles and owns the
// database, and that is all. Provisioning twice as one is what a managed installation does on
// its second upgrade, and PostgreSQL refuses such a role the mere mention of an attribute it
// does not hold itself.
func TestProvisionNeedsNoSuperuser(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	url := os.Getenv("AGENTIIK_TEST_DATABASE_URL")
	admin := role[:min(len(role), maxIdentifier-len("_admin"))] + "_admin"

	// The administrator owns the database, so it can be dropped only after the database is, and
	// the role it created before it: this runs before the cleanups blank registered.
	t.Cleanup(func() {
		drop(ctx, url, `drop database if exists `+role+` with (force)`)
		drop(ctx, url, `drop role if exists `+role)
		drop(ctx, url, `drop role if exists `+admin)
	})
	conn := connect(t, super)
	for _, stmt := range []string{
		`drop role if exists ` + admin,
		`create role ` + admin + ` login createrole nosuperuser nobypassrls password 'test'`,
		`alter database ` + role + ` owner to ` + admin,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}

	as := connect(t, withCredentials(super, admin, "test"))
	for i, password := range []string{"test", "rotated"} {
		if _, err := Provision(ctx, as, role, password); err != nil {
			t.Fatalf("provisioning %d as an administrator that is no superuser: %s", i+1, err)
		}
	}
	if got, want := shape(t, conn, role), provisioned(t, conn); !slices.Equal(got, want) {
		t.Errorf("the role came out as\n%s\nand Provision promises\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	pool, err := Open(ctx, withCredentials(super, role, "rotated"))
	if err != nil {
		t.Fatalf("the role an administrator provisioned was refused: %s", err)
	}
	pool.Close()
}

// A refusal comes before anything is applied, so a mistaken call leaves the database as it found
// it rather than half migrated.
func TestProvisionRefusesARoleItCannotMakeAndAppliesNothing(t *testing.T) {
	super, _ := blank(t)
	ctx := t.Context()
	conn := connect(t, super)

	var self string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&self); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		role, says string
	}{
		{"", "no role"},
		{strings.Repeat("r", maxIdentifier+1), "cut it to 63"},
		{self, "migrates as"},
	} {
		_, err := Provision(ctx, conn, c.role, "test")
		if err == nil {
			t.Errorf("Provision took the role %q", c.role)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("the refusal of %q does not say %q: %s", c.role, c.says, err)
		}
	}

	var migrated, still bool
	if err := conn.QueryRow(ctx,
		`select to_regclass('schema_migrations') is not null, (select rolsuper from pg_roles where rolname = current_user)`,
	).Scan(&migrated, &still); err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Error("a refused provisioning migrated the database")
	}
	if !still {
		t.Error("the administrator lost its own superuser in a refused provisioning")
	}
}

// The verifier computed here is the one PostgreSQL computes from the same password and salt, so
// a password that never reaches the server opens the role exactly as one sent in clear would.
// The second password is written decomposed, a letter and then its diaeresis, which is where
// preparing a password is more than copying it.
func TestTheVerifierIsTheOnePostgreSQLComputes(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	conn := connect(t, super)
	if _, err := conn.Exec(ctx, `create role `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `set password_encryption = 'scram-sha-256'`); err != nil {
		t.Fatal(err)
	}

	for _, password := range []string{"correct horse battery staple", "pässwörd"} {
		var stmt string
		if err := conn.QueryRow(ctx, `select format('alter role %I password %L', $1::text, $2::text)`, role, password).Scan(&stmt); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
		var theirs string
		if err := conn.QueryRow(ctx, `select rolpassword from pg_authid where rolname = $1`, role).Scan(&theirs); err != nil {
			t.Fatal(err)
		}

		// SCRAM-SHA-256$<iterations>:<salt>$<stored key>:<server key>
		mechanism, rest, _ := strings.Cut(theirs, "$")
		count, rest, _ := strings.Cut(rest, ":")
		encoded, _, _ := strings.Cut(rest, "$")
		iterations, err := strconv.Atoi(count)
		if mechanism != "SCRAM-SHA-256" || err != nil {
			t.Fatalf("PostgreSQL stored %q, which is not a SCRAM-SHA-256 verifier", theirs)
		}
		salt, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("the salt of %q: %s", theirs, err)
		}

		ours, err := scramVerifier(password, salt, iterations)
		if err != nil {
			t.Fatal(err)
		}
		if ours != theirs {
			t.Errorf("for %q PostgreSQL computes\n%s\nand this computes\n%s", password, theirs, ours)
		}
	}
}

// A password opens the role however its letters are composed, since the login prepares it the
// way the verifier was prepared.
func TestAPasswordOpensTheRoleHoweverItIsComposed(t *testing.T) {
	super, role := blank(t)
	conn := connect(t, super)
	if _, err := Provision(t.Context(), conn, role, "pässwörd"); err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"pässwörd", "pässwörd"} {
		if err := login(t, super, role, password); err != nil {
			t.Errorf("the role does not open with %+q: %s", password, err)
		}
	}
}

// Every replica of the API runs migrate before it serves, and a rolling upgrade starts them
// together. Provisionings at once take turns: none of them fails, and between them they apply
// each migration once, whether everything is left to apply or nothing is.
func TestProvisioningsAtOnceTakeTurns(t *testing.T) {
	super, role := blank(t)
	ctx := t.Context()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}

	const replicas = 4
	conns := make([]*pgx.Conn, replicas)
	for i := range conns {
		conns[i] = connect(t, super)
	}
	for round := range 3 {
		ran := make([][]string, replicas)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, conn := range conns {
			wg.Go(func() {
				<-start
				var err error
				if ran[i], err = Provision(ctx, conn, role, "test"); err != nil {
					t.Errorf("round %d, replica %d: %s", round+1, i+1, err)
				}
			})
		}
		close(start)
		wg.Wait()

		applied, left := 0, 0
		if round == 0 {
			left = len(all)
		}
		for _, r := range ran {
			applied += len(r)
		}
		if applied != left {
			t.Errorf("round %d applied %d migrations between the replicas, and %d were left", round+1, applied, left)
		}
	}
}
