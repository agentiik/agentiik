package db

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/secure/precis"
)

// The role the application connects as, created beside the schema it is granted.
//
// Open refuses a connection that walks through the policies, so an installation needs a role
// that does not, and that role is only as good as its grants: one missing a table fails on the
// first request that reaches it, and one holding more than it should is a privilege nobody
// asked for. Both follow the migrations, which is why the role is provisioned here rather than
// by a script of an installation's own: a migration that adds a table is granted by the same
// run that applied it.

// maxIdentifier is the longest name PostgreSQL keeps whole, NAMEDATALEN less one. A longer one
// is cut with a notice rather than refused, and a role created under the cut name is not the
// role the address names.
const maxIdentifier = 63

// The shape of a SCRAM-SHA-256 verifier, as PostgreSQL draws one itself: a salt of sixteen
// bytes and scram_iterations' default. A verifier carries its own count, so an installation
// that raised the setting keeps authenticating a role this wrote, and the reverse.
const (
	scramSaltBytes  = 16
	scramIterations = 4096
)

// provisionLock is the advisory lock a provisioning holds from before it migrates until its role
// is committed.
//
// Two at once is an ordinary deployment rather than a mistake: each replica of the API runs
// migrate before it serves, and a rolling upgrade starts them together. Unserialised, they race
// on the catalogue rows a migration or a grant writes, and one fails on a unique index or with
// "tuple concurrently updated" though the other did all it would have done. Serialised, the
// second waits, then finds nothing left to apply. The number is the bytes of "migrate", which is
// not the controller's key, and is written as a literal so that a person can search for it.
const provisionLock int64 = 0x6d696772617465

// Provision applies the migrations as the privileged connection it is given, then creates the
// role the API and the controller connect as, or brings an existing one back to that shape, and
// answers with the migrations it applied.
//
// The role is LOGIN NOSUPERUSER NOBYPASSRLS, and holds nothing else a role can hold: it creates
// no database, no role and no replication slot. It may connect to this database, use the schema,
// and read and write every table but schema_migrations, which the application never reads and
// which a role that could delete a row of would have the next upgrade apply that migration
// again. Its privileges are revoked before they are granted, in one transaction, so the role
// ends with exactly these whatever it held before: a role somebody widened by hand is narrowed
// back, and a second call changes nothing.
//
// A role that owns the database or anything in it is refused before anything is applied, and the
// refusal names what it owns. No revoke reaches an owner: the owner of a table may switch off its
// row level security, and the owner of the database owns schema public and may drop
// schema_migrations there.
//
// A password is set as a SCRAM verifier computed here, so the password itself never reaches
// the server. Written into CREATE ROLE, it is statement text, and PostgreSQL logs statement
// text whenever log_statement covers DDL and whenever a statement fails, which it does by
// default; createuser and psql's \password send a verifier for the same reason. An empty
// password leaves the role's as it was, and a role created without one authenticates some
// other way, such as the client certificate the Channels table allows: an address that
// carries no password, because pgx reads it from a password file, must not be what removes it.
//
// The connection needs no superuser, only the right to create roles and to own the schema,
// which is what a managed PostgreSQL gives its administrator. It refuses the role the
// connection itself is, before anything is applied, since a role made NOSUPERUSER by its own
// migration is an installation nobody can migrate again. Two provisionings of one database at
// once take turns.
func Provision(ctx context.Context, admin *pgx.Conn, role, password string) ([]string, error) {
	if role == "" {
		return nil, errors.New("db: Provision was given no role, and the application needs one to connect as")
	}
	if len(role) > maxIdentifier {
		return nil, fmt.Errorf("db: the role name %q is %d bytes long, and PostgreSQL would cut it to %d and create a role the address does not name", role, len(role), maxIdentifier)
	}
	var itself bool
	var self string
	if err := admin.QueryRow(ctx, `select $1::text in (current_user, session_user), current_user::text`, role).Scan(&itself, &self); err != nil {
		return nil, fmt.Errorf("db: the connection could not be asked which role it is: %w", err)
	}
	if itself {
		return nil, fmt.Errorf("db: %s is the role this connection migrates as, and making it NOSUPERUSER NOBYPASSRLS would take from the migrations the privileges they run with: the application connects as a role of its own", role)
	}

	if _, err := admin.Exec(ctx, `select pg_advisory_lock($1)`, provisionLock); err != nil {
		return nil, fmt.Errorf("db: the connection could not wait for another provisioning of this database to end: %w", err)
	}
	defer admin.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, provisionLock)

	if err := refuseAnOwner(ctx, admin, role, self); err != nil {
		return nil, err
	}

	ran, err := Migrate(ctx, admin)
	if err != nil {
		return ran, err
	}

	secret := ""
	if password != "" {
		salt := make([]byte, scramSaltBytes)
		rand.Read(salt)
		if secret, err = scramVerifier(password, salt, scramIterations); err != nil {
			return ran, fmt.Errorf("db: the password of %s could not be turned into a verifier: %w", role, err)
		}
	}

	tx, err := admin.Begin(ctx)
	if err != nil {
		return ran, fmt.Errorf("db: a transaction for the role %s could not be begun: %w", role, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var database string
	if err := tx.QueryRow(ctx, `select current_database()`).Scan(&database); err != nil {
		return ran, fmt.Errorf("db: the connection could not be asked which database it is on: %w", err)
	}
	verb, options, err := roleShape(ctx, tx, role)
	if err != nil {
		return ran, err
	}
	if secret != "" {
		// Written as a literal as it is: a verifier is base64, digits and the separators SCRAM
		// puts between them, and holds no quote to escape.
		options = append(options, "password '"+secret+"'")
	}

	// Names are quoted as identifiers, since a role and a database cannot be parameters of the
	// statements that name them.
	name := pgx.Identifier{role}.Sanitize()
	var statements []string
	if len(options) > 0 {
		statements = append(statements, verb+" role "+name+" with "+strings.Join(options, " "))
	}
	statements = append(statements,
		// Granted rather than left to PUBLIC, since a hardened cluster revokes it from PUBLIC.
		"grant connect on database "+pgx.Identifier{database}.Sanitize()+" to "+name,
		"revoke all on schema public from "+name,
		"grant usage on schema public to "+name,
		"revoke all on all tables in schema public from "+name,
		"grant select, insert, update, delete on all tables in schema public to "+name,
		"revoke all on schema_migrations from "+name,
	)
	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			// The statement is not repeated, since the first one may carry the verifier.
			return ran, fmt.Errorf("db: the role %s could not be provisioned, and was left as it was: %w", role, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ran, fmt.Errorf("db: the role %s could not be committed: %w", role, err)
	}
	return ran, nil
}

// refuseAnOwner answers an error naming what role owns in this database, the database included,
// and nil when it owns nothing there. The ownership is read from pg_shdepend, where PostgreSQL
// records it for every object that has an owner, whatever its kind.
func refuseAnOwner(ctx context.Context, admin *pgx.Conn, role, self string) error {
	rows, err := admin.Query(ctx, `
		select pg_describe_object(d.classid, d.objid, d.objsubid)
		  from pg_shdepend d, pg_database this
		 where this.datname = current_database()
		   and d.refclassid = 'pg_authid'::regclass
		   and d.refobjid = (select oid from pg_roles where rolname = $1)
		   and d.deptype = 'o'
		   and (d.dbid = this.oid or (d.classid = 'pg_database'::regclass and d.objid = this.oid))
		 order by 1`, role)
	if err != nil {
		return fmt.Errorf("db: the connection could not be asked what %s owns: %w", role, err)
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("db: the connection could not be asked what %s owns: %w", role, err)
	}
	if len(owned) == 0 {
		return nil
	}
	return fmt.Errorf("db: %s owns %s, and no revoke reaches an owner, which may drop what it owns or switch off its row level security: hand what it owns to the role that migrates, with REASSIGN OWNED BY %s TO %s run in this database, then provision again",
		role, strings.Join(owned, ", "), pgx.Identifier{role}.Sanitize(), pgx.Identifier{self}.Sanitize())
}

// roleShape answers the verb and the attributes that give role the shape Provision promises:
// create and every attribute for a role that does not exist yet, and for one that does, alter
// and only the attributes it holds otherwise, which for a role already in shape is none.
//
// Only what differs, because PostgreSQL refuses a role that is not a superuser the mere mention
// of SUPERUSER, BYPASSRLS, REPLICATION or CREATEDB in ALTER ROLE, even to write the value the
// role already holds, and a managed PostgreSQL's administrator is such a role. CREATE ROLE is
// judged by the values and takes them all.
func roleShape(ctx context.Context, tx pgx.Tx, role string) (string, []string, error) {
	var super, bypass, login, createdb, createrole, replication bool
	err := tx.QueryRow(ctx,
		`select rolsuper, rolbypassrls, rolcanlogin, rolcreatedb, rolcreaterole, rolreplication
		   from pg_roles where rolname = $1`, role,
	).Scan(&super, &bypass, &login, &createdb, &createrole, &replication)
	if errors.Is(err, pgx.ErrNoRows) {
		return "create", []string{"login", "nosuperuser", "nobypassrls", "nocreatedb", "nocreaterole", "noreplication"}, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("db: the connection could not be asked what %s is: %w", role, err)
	}
	var options []string
	for _, a := range []struct {
		holds, wanted bool
		option        string
	}{
		{login, true, "login"},
		{super, false, "nosuperuser"},
		{bypass, false, "nobypassrls"},
		{createdb, false, "nocreatedb"},
		{createrole, false, "nocreaterole"},
		{replication, false, "noreplication"},
	} {
		if a.holds != a.wanted {
			options = append(options, a.option)
		}
	}
	return "alter", options, nil
}

// scramVerifier is what PostgreSQL stores for a SCRAM-SHA-256 password, as RFC 5802 and 7677
// define it and in the text form pg_authid holds.
//
// The password is prepared as pgx prepares it when it authenticates, with the PRECIS
// OpaqueString profile that replaced SASLprep, and kept as it was where the profile refuses it,
// as both pgx and PostgreSQL do. A verifier computed from other bytes than the login's is a role
// no password opens.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	prepared, err := precis.OpaqueString.String(password)
	if err != nil {
		prepared = password
	}
	salted, err := pbkdf2.Key(sha256.New, prepared, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	stored := sha256.Sum256(scramHMAC(salted, "Client Key"))
	server := scramHMAC(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(server)), nil
}

func scramHMAC(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
