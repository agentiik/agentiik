package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Where a namespace's secrets live.
//
// "The namespace declares where a secret lives (provider and path), through the API and
// agentiik_secret in Terraform. The workflow file only names a secret it uses." A declaration is
// the whole of what is kept here: which store, and where in it. The value is never written
// through anything in this file, and nothing here reads one, which is what lets a route answer
// every declaration of a namespace to whoever may read its workflows.

// ErrNoDeclaration is a secret the namespace does not declare.
var ErrNoDeclaration = errors.New("db: the namespace declares no secret of that name")

// ErrNoNamespace is a declaration written into a namespace nobody created.
var ErrNoNamespace = errors.New("db: no namespace of that name")

// Declaration is one secret of one namespace: the store its value is read from, and where in it.
type Declaration struct {
	Name     string
	Provider string

	// Path is where the value sits inside its store, and empty for the built-in one, which
	// keeps a value under the namespace and the name it is declared by.
	Path string

	DeclaredBy string
	DeclaredAt time.Time
}

// Declarations are every secret the namespace declares, by name.
func (n *NS) Declarations(ctx context.Context) ([]Declaration, error) {
	rows, err := n.tx.Query(ctx,
		`select name, provider, coalesce(path, ''), declared_by, declared_at
		 from secret_declarations where namespace = $1 order by name`, n.namespace)
	if err != nil {
		return nil, fmt.Errorf("db: the secret declarations could not be read: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Declaration, error) {
		var d Declaration
		err := row.Scan(&d.Name, &d.Provider, &d.Path, &d.DeclaredBy, &d.DeclaredAt)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the secret declarations could not be read: %w", err)
	}
	return out, nil
}

// Declaration is one secret the namespace declares, or ErrNoDeclaration.
func (n *NS) Declaration(ctx context.Context, name string) (Declaration, error) {
	var d Declaration
	err := n.tx.QueryRow(ctx,
		`select name, provider, coalesce(path, ''), declared_by, declared_at
		 from secret_declarations where namespace = $1 and name = $2`, n.namespace, name).
		Scan(&d.Name, &d.Provider, &d.Path, &d.DeclaredBy, &d.DeclaredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Declaration{}, ErrNoDeclaration
	}
	if err != nil {
		return Declaration{}, fmt.Errorf("db: secret %s could not be read: %w", name, err)
	}
	return d, nil
}

// Declare writes one declaration, replacing the one of that name if there is one, and answers
// whether it is new.
//
// One secret at a time and never the set, so that two writers each declaring their own secret
// cannot undo each other. Replacing is the ordinary case rather than a conflict: a declaration
// moved to another path is the same secret declared again.
func (n *NS) Declare(ctx context.Context, d Declaration) (bool, error) {
	switch {
	case d.Name == "" || d.Provider == "":
		return false, fmt.Errorf("db: a declaration names its secret and its provider, and this one is %q in %q", d.Name, d.Provider)
	case d.DeclaredBy == "":
		return false, fmt.Errorf("db: secret %s is declared by nobody", d.Name)
	}
	at := d.DeclaredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// xmax is zero on a row this statement inserted and set on one it updated, which is how
	// one statement says which of the two it did without a read before it that a concurrent
	// writer could make stale.
	var created bool
	err := n.tx.QueryRow(ctx,
		`insert into secret_declarations (namespace, name, provider, path, declared_by, declared_at)
		 values ($1, $2, $3, $4, $5, $6)
		 on conflict (namespace, name) do update
		   set provider = excluded.provider, path = excluded.path,
		       declared_by = excluded.declared_by, declared_at = excluded.declared_at
		 returning xmax = 0`,
		n.namespace, d.Name, d.Provider, nilIfEmpty(d.Path), d.DeclaredBy, at).Scan(&created)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return false, ErrNoNamespace
	}
	if err != nil {
		return false, fmt.Errorf("db: secret %s could not be declared: %w", d.Name, err)
	}
	return created, nil
}

// Undeclare removes one declaration, or answers ErrNoDeclaration where there was none.
func (n *NS) Undeclare(ctx context.Context, name string) error {
	tag, err := n.tx.Exec(ctx,
		`delete from secret_declarations where namespace = $1 and name = $2`, n.namespace, name)
	if err != nil {
		return fmt.Errorf("db: secret %s could not be undeclared: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoDeclaration
	}
	return nil
}

// foreignKeyViolation is PostgreSQL's code for a row naming a row that is not there, which a
// declaration is when its namespace was never created.
const foreignKeyViolation = "23503"
