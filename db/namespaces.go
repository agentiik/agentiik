package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Namespaces, created and removed by agentiik-api namespace, the server-side verb that stands in
// for v0.3.0's routes: until principals arrive nothing else creates one, and a workflow, a run
// and a secret all belong to a namespace that exists.

// NamespaceHolds is a namespace refused removal for what it still holds. A namespace is removed
// only once it is empty, because what it holds is somebody's work, and removing it would be
// removing that too.
type NamespaceHolds struct {
	Name                     string
	Workflows, Runs, Secrets int
	// Other is a table still referring to it where none of the three counted does, such as
	// artifact_objects, whose objects outlive the runs that wrote them until they are
	// collected.
	Other string
}

func (h *NamespaceHolds) Error() string { return "db: " + h.Held() }

// Held says what the namespace holds, in a sentence a person reads.
func (h *NamespaceHolds) Held() string {
	var held []string
	for _, c := range []struct {
		n    int
		what string
	}{{h.Workflows, "workflow"}, {h.Runs, "run"}, {h.Secrets, "secret"}} {
		switch {
		case c.n == 1:
			held = append(held, "1 "+c.what)
		case c.n > 1:
			held = append(held, fmt.Sprintf("%d %ss", c.n, c.what))
		}
	}
	if h.Other != "" {
		held = append(held, "rows of "+h.Other)
	}
	return fmt.Sprintf("namespace %s holds %s, and a namespace is removed only once it holds nothing, since what it holds is somebody's work", h.Name, strings.Join(held, ", "))
}

// CreateNamespace creates a namespace, and answers whether it did: false is one that already
// existed, which is left as it was, so that an installation script run twice creates it once.
func (w *Wide) CreateNamespace(ctx context.Context, name string) (bool, error) {
	tag, err := w.tx.Exec(ctx, `insert into namespaces (name) values ($1) on conflict (name) do nothing`, name)
	if err != nil {
		return false, fmt.Errorf("db: namespace %s could not be created: %w", name, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveNamespace removes a namespace that holds no workflow, run or secret, and refuses one that
// does with a *NamespaceHolds. One that does not exist is ErrNoNamespace.
//
// The row is locked before anything is counted, so that a workflow pushed or a secret written
// while the counts are read waits for this transaction and then finds the namespace gone, rather
// than landing in a namespace counted as empty.
func (w *Wide) RemoveNamespace(ctx context.Context, name string) error {
	var found string
	err := w.tx.QueryRow(ctx, `select name from namespaces where name = $1 for update`, name).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNoNamespace, name)
	}
	if err != nil {
		return fmt.Errorf("db: namespace %s could not be read: %w", name, err)
	}
	holds := &NamespaceHolds{Name: name}
	if err := w.tx.QueryRow(ctx,
		`select (select count(*) from workflows where namespace = $1),
		        (select count(*) from runs where namespace = $1),
		        (select count(*) from (select name from secret_declarations where namespace = $1
		                               union select name from secret_values where namespace = $1) s)`,
		name).Scan(&holds.Workflows, &holds.Runs, &holds.Secrets); err != nil {
		return fmt.Errorf("db: what namespace %s holds could not be counted: %w", name, err)
	}
	if holds.Workflows > 0 || holds.Runs > 0 || holds.Secrets > 0 {
		return holds
	}
	_, err = w.tx.Exec(ctx, `delete from namespaces where name = $1`, name)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		holds.Other = pg.TableName
		if holds.Other == "" {
			holds.Other = "a table that refers to it"
		}
		return holds
	}
	if err != nil {
		return fmt.Errorf("db: namespace %s could not be removed: %w", name, err)
	}
	return nil
}
