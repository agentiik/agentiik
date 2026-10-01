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

// Namespaces, created and removed by an administrator through the API and by agentiik-api
// namespace, the server-side verb that runs where the API runs: both write through here, so that
// one namespace is one row, one built-in identity and one set of refusals whichever made it.

// NamespaceHolds is a namespace refused removal for what it still holds. A namespace is removed
// only once it is empty, because what it holds is somebody's work, and removing it would be
// removing that too.
type NamespaceHolds struct {
	Name                     string
	Workflows, Runs, Secrets int

	// Objects are the stored objects its runs wrote, which outlive the runs until they are
	// collected, and ServiceAccounts its service accounts besides its built-in identity, whose
	// tokens a script somewhere still presents.
	Objects, ServiceAccounts int

	// Other is a table still referring to it where none of those counted does.
	Other string
}

func (h *NamespaceHolds) Error() string { return "db: " + h.Held() }

// Held says what the namespace holds, in a sentence a person reads.
func (h *NamespaceHolds) Held() string {
	var held []string
	for _, c := range []struct {
		n          int
		one, other string
	}{
		{h.Workflows, "workflow", "workflows"}, {h.Runs, "run", "runs"}, {h.Secrets, "secret", "secrets"},
		{h.Objects, "stored object", "stored objects"},
		{h.ServiceAccounts, "service account besides " + h.Name + "/" + BuiltIn, "service accounts besides " + h.Name + "/" + BuiltIn},
	} {
		switch {
		case c.n == 1:
			held = append(held, "1 "+c.one)
		case c.n > 1:
			held = append(held, fmt.Sprintf("%d %s", c.n, c.other))
		}
	}
	if h.Other != "" {
		held = append(held, "rows of "+h.Other)
	}
	return fmt.Sprintf("namespace %s holds %s, and a namespace is removed only once it holds nothing, since what it holds is somebody's work", h.Name, strings.Join(held, ", "))
}

// CreateNamespace creates a namespace with its built-in identity, NS/agentiik, and answers whether
// it did: false is one that already existed, which is left as it was, so that an installation
// script run twice creates it once. A name that is a user's login is ErrNameTaken, one another
// namespace held before it was renamed a *NameHeld naming it, which is ErrNameTaken as well, and an
// owner nobody created ErrNoPrincipal. Its storage name is the name it is created with.
//
// n.Kind is shared where it is empty, and n.Owner is written on the row alone: the grant that lets
// an owner act on the namespace is its creator's to write, beside this, with GrantAccess. The
// quotas are written as SetQuotas writes them, so a zero MaxConcurrentTasks or MaxRetentionDays
// starts at the table's default, 20 or 90.
//
// The built-in identity is created with the namespace because "scheduled, webhook and event runs
// are attributed to" it, and it "holds no grant until an owner gives it one", so creating it grants
// nothing. A namespace v0.2 made has none until one is given it.
func (w *Wide) CreateNamespace(ctx context.Context, n Namespace) (bool, error) {
	if n.Kind == "" {
		n.Kind = NamespaceShared
	}
	// Asked first, since a refusal in the statement below ends the transaction it would be asked
	// in; the trigger that refuses it all the same is what holds when two creations race.
	if held, err := heldBy(ctx, w.tx, n.Name, ""); err != nil {
		return false, err
	} else if held != nil && held.Former {
		return false, held
	}
	tag, err := w.tx.Exec(ctx,
		`insert into namespaces (name, kind, owner) values ($1, $2, $3) on conflict (name) do nothing`,
		n.Name, n.Kind, nilIfEmpty(n.Owner))
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.ConstraintName == namesShared:
		return false, fmt.Errorf("%w: %s", ErrNameTaken, n.Name)
	case errors.As(err, &pg) && pg.ConstraintName == formerNamesHeld:
		return false, &NameHeld{Name: n.Name, Former: true}
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return false, fmt.Errorf("%w: %s", ErrNoPrincipal, n.Owner)
	case err != nil:
		return false, fmt.Errorf("db: namespace %s could not be created: %w", n.Name, err)
	case tag.RowsAffected() == 0:
		return false, nil
	}
	if err := w.SetQuotas(ctx, n.Name, n.Quotas); err != nil {
		return false, err
	}
	if err := w.CreateServiceAccount(ctx, ServiceAccount{Namespace: n.Name, Name: BuiltIn}); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveNamespace removes a namespace that holds no workflow, run, secret, stored object or service
// account but its built-in identity, and refuses one that does with a *NamespaceHolds. One that
// does not exist is ErrNoNamespace.
//
// It removes a personal namespace as it removes a shared one: whether one may be removed, and by
// whom, is its caller's to say, since the route that removes a namespace refuses a personal one and
// removing a user takes their empty personal namespace with them.
//
// The built-in identity goes first, with the tokens and grants it holds: it is the namespace's own
// and nobody created it, so it is no reason to keep the namespace, and a service account refers to
// its namespace, which could not go while it stayed. The namespace's grants and its authentication
// policy go with the row.
//
// The row is locked before anything is counted, so that a workflow pushed, a secret written or a
// service account created while the counts are read waits for this transaction and then finds the
// namespace gone, rather than landing in a namespace counted as empty.
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
		                               union select name from secret_values where namespace = $1) s),
		        (select count(*) from artifact_objects where namespace = $1),
		        (select count(*) from service_accounts where namespace = $1 and name <> $2)`,
		name, BuiltIn).Scan(&holds.Workflows, &holds.Runs, &holds.Secrets, &holds.Objects, &holds.ServiceAccounts); err != nil {
		return fmt.Errorf("db: what namespace %s holds could not be counted: %w", name, err)
	}
	if holds.Workflows > 0 || holds.Runs > 0 || holds.Secrets > 0 || holds.Objects > 0 || holds.ServiceAccounts > 0 {
		return holds
	}
	if _, err := w.tx.Exec(ctx, `delete from principals where id = $1 || '/' || $2 and kind = 'service_account'`, name, BuiltIn); err != nil {
		return fmt.Errorf("db: the built-in identity of namespace %s could not be removed: %w", name, err)
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

// formerNamesHeld is what refuses a namespace a name another namespace held before it was renamed.
const formerNamesHeld = "namespace_former_names"

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

	// Storage is the name it was created with, which its objects and sealed values are kept
	// under, and FormerNames the names it held before a rename, in the order it left them.
	Storage     string
	FormerNames []string

	// AvatarUpdatedAt is when its picture was set, and zero where it has none.
	AvatarUpdatedAt time.Time
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
	allowed_runner_pools, created_at, storage, former_names, avatar_updated_at`

func scanNamespace(row pgx.Row) (Namespace, error) {
	var n Namespace
	var avatarAt *time.Time
	q := &n.Quotas
	err := row.Scan(&n.Name, &n.Kind, &n.Owner, &q.MaxConcurrentTasks, &q.MaxRetentionDays,
		&q.MaxRunsPerHour, &q.MaxArtifactBytes, &q.MaxRunDuration, &q.AllowedRunnerPools, &n.CreatedAt,
		&n.Storage, &n.FormerNames, &avatarAt)
	if avatarAt != nil {
		n.AvatarUpdatedAt = *avatarAt
	}
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

// Ownerless is the names of the namespaces whose record names no owner, ordered by name, each held
// for key share, as a grant written in one holds it: a removal under way is waited for, and the
// namespace it removes is left out rather than named to a grant that could no longer be written,
// and a removal that comes after waits for the transaction that read them.
func (w *Wide) Ownerless(ctx context.Context) ([]string, error) {
	rows, err := w.tx.Query(ctx, `select name from namespaces where owner is null order by name for key share`)
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces nobody owns could not be read: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces nobody owns could not be read: %w", err)
	}
	return names, nil
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
