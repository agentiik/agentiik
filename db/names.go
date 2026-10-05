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

// A namespace's names and the name it is stored under: "a shared namespace is renamed by its owner
// ... in one transaction that carries everything at once", the name it leaves staying its own as a
// former name, and its objects and sealed values kept under the name it was created with, which never
// changes (migration 0069).

// Storage is the namespace's storage name: the name it was created with, the first segment of the
// key of every object it holds, and the name its secrets' values and its webhooks' secrets are sealed
// under. "The storage name never changes, so a rename moves no byte however much a namespace holds,
// and a value sealed in it still opens." It is the namespace's name for every namespace never
// renamed, and the name itself for one that does not exist, whose keys nothing holds.
//
// Read once per handle and kept, since it never changes.
func (n *NS) Storage(ctx context.Context) (string, error) {
	if n.storage != "" {
		return n.storage, nil
	}
	storage, err := storageOf(ctx, n.tx, n.namespace)
	if err != nil {
		return "", err
	}
	n.storage = storage
	return storage, nil
}

// StorageOf is the storage name of the namespace named name, as NS.Storage answers it.
func (w *Wide) StorageOf(ctx context.Context, name string) (string, error) {
	return storageOf(ctx, w.tx, name)
}

// Storage is the storage name of the namespace named name, as NS.Storage answers it, for a caller
// that holds no handle on it.
func (p *Pool) Storage(ctx context.Context, name string) (string, error) {
	var storage string
	err := p.In(ctx, name, func(ctx context.Context, ns *NS) error {
		var err error
		storage, err = ns.Storage(ctx)
		return err
	})
	return storage, err
}

func storageOf(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	var storage string
	err := tx.QueryRow(ctx, `select storage from namespaces where name = $1`, name).Scan(&storage)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return name, nil
	case err != nil:
		return "", fmt.Errorf("db: the storage name of namespace %s could not be read: %w", name, err)
	}
	return storage, nil
}

// CurrentName answers the name the namespace that answers to name has now: name itself, where no
// namespace holds it as a former name, and otherwise the name of the namespace that does. "The old
// name stays the namespace's, as a former name, and is resolved wherever an address names a
// namespace." A name nobody holds is answered as it is, and found absent by whatever asks of it.
func (w *Wide) CurrentName(ctx context.Context, name string) (string, error) {
	return currentName(ctx, w.tx, name)
}

// CurrentName is Wide.CurrentName for a caller holding no transaction, the router resolving the
// namespace an address names among them. The namespaces are the installation's and under no policy,
// so it is asked through a handle on the name asked about rather than through the installation's
// door, which would need a reason for what every request asks.
func (p *Pool) CurrentName(ctx context.Context, name string) (string, error) {
	var current string
	err := p.In(ctx, name, func(ctx context.Context, ns *NS) error {
		var err error
		current, err = currentName(ctx, ns.tx, name)
		return err
	})
	return current, err
}

func currentName(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	var current string
	if err := tx.QueryRow(ctx,
		`select coalesce((select n.name from namespaces n where n.former_names @> array[$1::text] limit 1), $1)`,
		name).Scan(&current); err != nil {
		return "", fmt.Errorf("db: which namespace answers to %s could not be read: %w", name, err)
	}
	return current, nil
}

// ErrPersonalRename is a user's personal namespace asked to be renamed: it is named after its user's
// login, which never changes.
var ErrPersonalRename = errors.New("db: a personal namespace is named after its user's login and never renamed")

// NameHeld is a name a namespace cannot be given, saying who holds it: a user, as their login; a
// namespace, as its name; or a namespace, as a name it held before it was renamed.
type NameHeld struct {
	Name string

	// Login is set where a user holds it; otherwise Namespace is the namespace holding it, and
	// Former says it holds it as a former name.
	Login     bool
	Namespace string
	Former    bool
}

func (e *NameHeld) Error() string {
	switch {
	case e.Login:
		return fmt.Sprintf("db: %s is a user's login", e.Name)
	case e.Former:
		return fmt.Sprintf("db: %s is a name namespace %s held before it was renamed", e.Name, e.Namespace)
	}
	return fmt.Sprintf("db: %s is a namespace", e.Name)
}

// Is makes a NameHeld an ErrNameTaken, which a caller that does not tell the holders apart asks.
func (e *NameHeld) Is(target error) bool { return target == ErrNameTaken }

// RenameWaits is a rename refused for what has to end first: runs of the namespace queued, running
// or waiting, moves of its workflows to it or from it asked and not carried out, and runners not
// revoked that narrow themselves to it by name on their host.
type RenameWaits struct {
	Namespace string
	Runs      int
	Moves     int
	Runners   []string
}

func (e *RenameWaits) Error() string { return "db: " + e.Held() }

// Held says what the rename waits for, in a sentence a person reads.
func (e *RenameWaits) Held() string {
	var why []string
	switch {
	case e.Runs == 1:
		why = append(why, fmt.Sprintf("namespace %s has a run queued, running or waiting, and its tasks carry its name to their runners, so it is renamed once it has finished", e.Namespace))
	case e.Runs > 1:
		why = append(why, fmt.Sprintf("namespace %s has %d runs queued, running or waiting, and their tasks carry its name to their runners, so it is renamed once they have finished", e.Namespace, e.Runs))
	}
	switch {
	case e.Moves == 1:
		why = append(why, fmt.Sprintf("a move of a workflow to or from namespace %s is asked and not carried out yet, and it is renamed once it is", e.Namespace))
	case e.Moves > 1:
		why = append(why, fmt.Sprintf("%d moves of workflows to or from namespace %s are asked and not carried out yet, and it is renamed once they are", e.Moves, e.Namespace))
	}
	if len(e.Runners) > 0 {
		why = append(why, fmt.Sprintf("runner %s narrows itself to namespace %s by name with AGK_RUNNER_NAMESPACES on its host, which no rename reaches: revoke it first, and join it again naming the new name", strings.Join(e.Runners, ", "), e.Namespace))
	}
	return strings.Join(why, "; and ")
}

// principalColumns are the columns outside the principals' own keys that name a principal as text,
// who did what or whose a record is: a service account of a namespace renamed is written under its
// new name in each, so that what it did and what it holds stay its own, a run it started is still
// started by somebody who exists, and a key onto principals that cascades a delete, as a
// collection's onto its owner, does not take a row with the old name. The audit log is history, and
// is not among them.
var principalColumns = []struct{ table, column string }{
	{"api_tokens", "principal"}, {"grants", "principal"}, {"notifications", "recipient"}, {"namespaces", "owner"},
	{"grants", "granted_by"}, {"workflows", "created_by"}, {"workflows", "deleted_by"},
	{"workflow_refs", "moved_by"}, {"workflow_versions", "author"}, {"image_pins", "pinned_by"},
	{"brick_manifests", "recorded_by"}, {"secret_declarations", "declared_by"}, {"triggers", "armed_by"},
	{"webhook_credentials", "written_by"}, {"runs", "triggered_by"}, {"approvals", "decided_by"},
	{"notification_events", "started_by"}, {"notifications", "acted_by"}, {"workflow_moves", "asked_by"},
	{"service_accounts", "created_by"}, {"event_deliveries", "publisher"}, {"join_tokens", "issued_by"},
	{"runner_pools", "created_by"}, {"runners", "drained_by"}, {"runners", "revoked_by"},
	{"enrolment_codes", "issued_by"}, {"collections", "principal"},
}

// namespaceColumnsUnkeyed are the columns naming a namespace that no key onto namespaces carries,
// each the namespace a row is of or the one it hears, which a rename writes itself.
var namespaceColumnsUnkeyed = []struct{ table, column string }{
	{"event_deliveries", "namespace"}, {"webhook_deliveries", "namespace"}, {"moved_objects", "namespace"},
	{"triggers", "hears"},
}

// RenameNamespace gives the namespace from the name to, and answers whether it changed anything: a
// rename to the name it has is none. "The rename is one transaction and carries everything at
// once."
//
// The namespace's row is held first, and then the lock every creation of a login or a namespace
// takes, so that a name checked free here is still free when it is written. It is refused, in this
// order: ErrNoNamespace; ErrPersonalRename; *NameHeld where a login, a namespace or another
// namespace's former name holds to; and *RenameWaits while a run of it is going, a move to it or from
// it waits, or a runner not revoked narrows itself to it.
//
// Then the principals of its service accounts are written under the new name, so that each service
// account, whose principal is made of its namespace's name and its own, finds its principal when the
// namespace's row changes; the row changes, its name and the names it held together, and every key
// naming it carries the change through (migration 0069); the columns naming one of its service
// accounts as text, or naming it with no key, are written; and the principals under the old name go,
// nothing referring to them any more. The audit log is not written: its caller records the rename.
func (w *Wide) RenameNamespace(ctx context.Context, from, to string) (bool, error) {
	var kind string
	err := w.tx.QueryRow(ctx, `select kind from namespaces where name = $1 for update`, from).Scan(&kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("%w: %s", ErrNoNamespace, from)
	case err != nil:
		return false, fmt.Errorf("db: namespace %s could not be read: %w", from, err)
	case kind == NamespacePersonal:
		return false, fmt.Errorf("%w: %s", ErrPersonalRename, from)
	case to == from:
		return false, nil
	}
	if _, err := w.tx.Exec(ctx, `select pg_advisory_xact_lock(474080961907)`); err != nil {
		return false, fmt.Errorf("db: the names could not be held: %w", err)
	}
	if held, err := heldBy(ctx, w.tx, to, from); err != nil || held != nil {
		if held != nil {
			return false, held
		}
		return false, err
	}

	waits := &RenameWaits{Namespace: from}
	if err := w.tx.QueryRow(ctx,
		`select (select count(*) from runs where namespace = $1 and state in `+unfinished+`),
		        (select count(*) from workflow_moves where namespace = $1 or target = $1)`,
		from).Scan(&waits.Runs, &waits.Moves); err != nil {
		return false, fmt.Errorf("db: what namespace %s waits for could not be read: %w", from, err)
	}
	rows, err := w.tx.Query(ctx,
		`select id from runners where revoked_at is null and accepted_namespaces @> array[$1::namespace_name] order by id`, from)
	if err != nil {
		return false, fmt.Errorf("db: the runners narrowed to namespace %s could not be read: %w", from, err)
	}
	if waits.Runners, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return false, fmt.Errorf("db: the runners narrowed to namespace %s could not be read: %w", from, err)
	}
	if waits.Runs > 0 || waits.Moves > 0 || len(waits.Runners) > 0 {
		return false, waits
	}

	// A principal of the namespace is written NS/NAME, and is one of its service accounts.
	of := `left(%[1]s, length($1) + 1) = $1 || '/'`
	renamed := `$2 || substr(%[1]s, length($1) + 1)`
	if _, err := w.tx.Exec(ctx,
		`insert into principals (id, kind, created_at)
		 select `+fmt.Sprintf(renamed, "id")+`, kind, created_at from principals
		 where kind = 'service_account' and `+fmt.Sprintf(of, "id"), from, to); err != nil {
		return false, fmt.Errorf("db: the service accounts of namespace %s could not be named under %s: %w", from, to, err)
	}
	_, err = w.tx.Exec(ctx,
		`update namespaces set name = $2, former_names = array_append(array_remove(former_names, $2), $1) where name = $1`,
		from, to)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == uniqueViolation {
		// Taken by somebody who did not wait for the lock the check took, which nothing in this
		// installation does, and said as the trigger says it: the transaction is over, and who
		// holds the name can no longer be asked in it.
		return false, &NameHeld{Name: to, Login: pg.ConstraintName == namesShared, Former: pg.ConstraintName == formerNamesHeld}
	}
	if err != nil {
		return false, fmt.Errorf("db: namespace %s could not be renamed %s: %w", from, to, err)
	}
	for _, c := range principalColumns {
		if _, err := w.tx.Exec(ctx, fmt.Sprintf(`update %s set %s = %s where %s`,
			c.table, c.column, fmt.Sprintf(renamed, c.column), fmt.Sprintf(of, c.column)), from, to); err != nil {
			return false, fmt.Errorf("db: %s.%s could not follow namespace %s to %s: %w", c.table, c.column, from, to, err)
		}
	}
	for _, c := range namespaceColumnsUnkeyed {
		if _, err := w.tx.Exec(ctx, fmt.Sprintf(`update %s set %s = $2 where %s = $1`, c.table, c.column, c.column), from, to); err != nil {
			return false, fmt.Errorf("db: %s.%s could not follow namespace %s to %s: %w", c.table, c.column, from, to, err)
		}
	}
	// The lists of namespaces, which name it among others: those a pool accepts, those a revoked
	// runner was narrowed to, and those a token reaches, a namespace or one of its workflows.
	for _, table := range []string{"runner_pools", "runners"} {
		if _, err := w.tx.Exec(ctx, fmt.Sprintf(
			`update %s set accepted_namespaces = array_replace(accepted_namespaces, $1::namespace_name, $2::namespace_name)
			 where accepted_namespaces @> array[$1::namespace_name]`, table), from, to); err != nil {
			return false, fmt.Errorf("db: the namespaces %s accepts could not follow namespace %s to %s: %w", table, from, to, err)
		}
	}
	if _, err := w.tx.Exec(ctx,
		`update api_tokens set scope_within = array(
		   select case when s.v::text = $1 then $2
		               when `+fmt.Sprintf(of, "s.v::text")+` then `+fmt.Sprintf(renamed, "s.v::text")+`
		               else s.v::text end
		   from unnest(scope_within) with ordinality as s(v, i) order by s.i)::grant_scope[]
		 where exists (select from unnest(scope_within) v where v::text = $1 or `+fmt.Sprintf(of, "v::text")+`)`,
		from, to); err != nil {
		return false, fmt.Errorf("db: the tokens narrowed to namespace %s could not follow it to %s: %w", from, to, err)
	}
	if _, err := w.tx.Exec(ctx,
		`delete from principals where kind = 'service_account' and `+fmt.Sprintf(of, "id"), from); err != nil {
		return false, fmt.Errorf("db: the service accounts of namespace %s could not let go of its old name: %w", from, err)
	}
	return true, nil
}

// heldBy answers who holds name, which the namespace self may be given otherwise: a user's login, a
// namespace's name, or another namespace's former name; and nil where nobody holds it, or self holds
// it as a former name, which it takes back.
func heldBy(ctx context.Context, tx pgx.Tx, name, self string) (*NameHeld, error) {
	var login bool
	var namespace, former string
	if err := tx.QueryRow(ctx,
		`select exists (select from users where login = $1),
		        coalesce((select name from namespaces where name = $1), ''),
		        coalesce((select name from namespaces where former_names @> array[$1::text] and name <> $2 limit 1), '')`,
		name, self).Scan(&login, &namespace, &former); err != nil {
		return nil, fmt.Errorf("db: who holds the name %s could not be read: %w", name, err)
	}
	switch {
	case login:
		return &NameHeld{Name: name, Login: true}, nil
	case namespace != "":
		return &NameHeld{Name: name, Namespace: namespace}, nil
	case former != "":
		return &NameHeld{Name: name, Namespace: former, Former: true}, nil
	}
	return nil, nil
}

// NameHolder answers who holds name, as a namespace a rename would give it is refused it: a user's
// login, a namespace's name, a namespace's former name, or nil for nobody.
func (w *Wide) NameHolder(ctx context.Context, name string) (*NameHeld, error) {
	return heldBy(ctx, w.tx, name, "")
}

// ErrNoNamespaceAvatar is a namespace that has no picture.
var ErrNoNamespaceAvatar = errors.New("db: that namespace has no picture")

// SetNamespaceAvatar sets a namespace's picture, the PNG the API encoded, set at at. A namespace that
// does not exist is ErrNoNamespace.
func (w *Wide) SetNamespaceAvatar(ctx context.Context, name string, png []byte, at time.Time) error {
	tag, err := w.tx.Exec(ctx, `update namespaces set avatar = $2, avatar_updated_at = $3 where name = $1`, name, png, at)
	if err != nil {
		return fmt.Errorf("db: the picture of namespace %s could not be written: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoNamespace, name)
	}
	return nil
}

// RemoveNamespaceAvatar removes a namespace's picture, and answers whether it had one. A namespace
// that does not exist is ErrNoNamespace, told apart from one with no picture, which removing changes
// nothing of.
func (w *Wide) RemoveNamespaceAvatar(ctx context.Context, name string) (bool, error) {
	var had bool
	err := w.tx.QueryRow(ctx, `select avatar is not null from namespaces where name = $1 for update`, name).Scan(&had)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("%w: %s", ErrNoNamespace, name)
	case err != nil:
		return false, fmt.Errorf("db: the picture of namespace %s could not be read: %w", name, err)
	case !had:
		return false, nil
	}
	if _, err := w.tx.Exec(ctx, `update namespaces set avatar = null, avatar_updated_at = null where name = $1`, name); err != nil {
		return false, fmt.Errorf("db: the picture of namespace %s could not be removed: %w", name, err)
	}
	return true, nil
}

// NamespaceAvatar reads a namespace's picture and when it was set. A namespace that does not exist is
// ErrNoNamespace, and one with no picture ErrNoNamespaceAvatar.
func (w *Wide) NamespaceAvatar(ctx context.Context, name string) ([]byte, time.Time, error) {
	var png []byte
	var at *time.Time
	err := w.tx.QueryRow(ctx, `select avatar, avatar_updated_at from namespaces where name = $1`, name).Scan(&png, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, time.Time{}, fmt.Errorf("%w: %s", ErrNoNamespace, name)
	case err != nil:
		return nil, time.Time{}, fmt.Errorf("db: the picture of namespace %s could not be read: %w", name, err)
	case png == nil || at == nil:
		return nil, time.Time{}, fmt.Errorf("%w: %s", ErrNoNamespaceAvatar, name)
	}
	return png, *at, nil
}

// FormerNames are the names the namespace held before it was renamed, in the order it left them,
// and none for one never renamed, or one that does not exist.
func (n *NS) FormerNames(ctx context.Context) ([]string, error) {
	var former []string
	err := n.tx.QueryRow(ctx, `select former_names from namespaces where name = $1`, n.namespace).Scan(&former)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("db: the names namespace %s held could not be read: %w", n.namespace, err)
	}
	return former, nil
}
