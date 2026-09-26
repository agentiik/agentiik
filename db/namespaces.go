package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// The kinds of namespace, as namespaces.kind writes them.
const (
	// NamespacePersonal is the namespace a user is given, named after their login and owned by
	// them.
	NamespacePersonal = "personal"
	// NamespaceShared is one an administrator creates for a team, and every namespace v0.2 made.
	NamespaceShared = "shared"
)

// Namespace is a namespace as an administrator creates it and the API lists it.
type Namespace struct {
	Name string
	Kind string

	// Owner is the principal it is reported to when an administrator grants themselves access,
	// and empty where nobody owns it yet, as nobody owns a namespace v0.2 made.
	Owner string

	Quotas    Quotas
	CreatedAt time.Time
}

// Quotas are what one namespace may consume.
type Quotas struct {
	// MaxConcurrentTasks and MaxRetentionDays are always set, at 20 and 90 unless an
	// administrator set them otherwise.
	MaxConcurrentTasks int
	MaxRetentionDays   int

	// The others bound nothing where they are zero, or nil for AllowedRunnerPools, which then
	// allows every pool that accepts the namespace. MaxRunDuration is written on a timeout's
	// grammar, 4h, as the administrator wrote it.
	MaxRunsPerHour     int
	MaxArtifactBytes   int64
	MaxRunDuration     string
	AllowedRunnerPools []string
}

const namespaceColumns = `name, kind, coalesce(owner, ''), max_concurrent_tasks, max_retention_days,
	coalesce(max_runs_per_hour, 0), coalesce(max_artifact_bytes, 0), coalesce(max_run_duration, ''),
	allowed_runner_pools, created_at`

func scanNamespace(row pgx.Row) (Namespace, error) {
	var n Namespace
	q := &n.Quotas
	err := row.Scan(&n.Name, &n.Kind, &n.Owner, &q.MaxConcurrentTasks, &q.MaxRetentionDays,
		&q.MaxRunsPerHour, &q.MaxArtifactBytes, &q.MaxRunDuration, &q.AllowedRunnerPools, &n.CreatedAt)
	return n, err
}

// NamespaceNamed reads one.
func (w *Wide) NamespaceNamed(ctx context.Context, name string) (Namespace, error) {
	n, err := scanNamespace(w.tx.QueryRow(ctx, `select `+namespaceColumns+` from namespaces where name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Namespace{}, fmt.Errorf("%w: %s", ErrNoNamespace, name)
	}
	if err != nil {
		return Namespace{}, fmt.Errorf("db: namespace %s could not be read: %w", name, err)
	}
	return n, nil
}

// Namespaces is the listing, ordered by name.
func (w *Wide) Namespaces(ctx context.Context) ([]Namespace, error) {
	rows, err := w.tx.Query(ctx, `select `+namespaceColumns+` from namespaces order by name`)
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces could not be read: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Namespace, error) { return scanNamespace(row) })
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces could not be read: %w", err)
	}
	return all, nil
}

// SetOwner gives a namespace an owner, a principal that exists. A personal namespace is its
// user's, and handing it to anybody else is refused by the table.
func (w *Wide) SetOwner(ctx context.Context, name, owner string) error {
	tag, err := w.tx.Exec(ctx, `update namespaces set owner = $2 where name = $1`, name, owner)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, owner)
	}
	if err != nil {
		return fmt.Errorf("db: the owner of namespace %s could not be set: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoNamespace, name)
	}
	return nil
}

// SetQuotas writes a namespace's quotas whole: a zero MaxConcurrentTasks or MaxRetentionDays keeps
// the value it has, and any other zero, or a nil AllowedRunnerPools, bounds nothing.
func (w *Wide) SetQuotas(ctx context.Context, name string, q Quotas) error {
	tag, err := w.tx.Exec(ctx,
		`update namespaces
		    set max_concurrent_tasks = coalesce($2, max_concurrent_tasks),
		        max_retention_days   = coalesce($3, max_retention_days),
		        max_runs_per_hour = $4, max_artifact_bytes = $5, max_run_duration = $6,
		        allowed_runner_pools = $7
		  where name = $1`,
		name, zeroIsNull(q.MaxConcurrentTasks), zeroIsNull(q.MaxRetentionDays), zeroIsNull(q.MaxRunsPerHour),
		zeroIsNull64(q.MaxArtifactBytes), nilIfEmpty(q.MaxRunDuration), q.AllowedRunnerPools)
	if err != nil {
		return fmt.Errorf("db: the quotas of namespace %s could not be set: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoNamespace, name)
	}
	return nil
}

func zeroIsNull64(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
