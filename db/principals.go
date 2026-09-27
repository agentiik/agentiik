package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The principals: users, groups and service accounts, each named by the one string a grant, the
// API, agk and the audit log write it with. A user is its login, a group group:NAME, a service
// account NS/NAME. What each may do is in its grants, which are namespaced and read through In;
// who each is, is the installation's, and read through Installation for Identity.

// The kinds of principal, as principals.kind and $defs/principal write them.
const (
	KindUser           = "user"
	KindGroup          = "group"
	KindServiceAccount = "service_account"
)

// ErrNoPrincipal is a principal nobody created.
var ErrNoPrincipal = errors.New("db: no principal of that name")

// ErrPrincipalExists is a principal created under a name another already has.
var ErrPrincipalExists = errors.New("db: a principal of that name exists")

// ErrNameTaken is a login where a namespace of that name exists, or a namespace where a login
// does: the two share one name space, since a user's personal namespace is named after their login.
var ErrNameTaken = errors.New("db: that name is a login or a namespace already")

// ErrOwnsNamespace is a principal refused removal while it owns a namespace, which would be left
// owned by somebody who is gone.
var ErrOwnsNamespace = errors.New("db: that principal owns a namespace")

// User is a local account, and nothing it proves itself with.
type User struct {
	Login       string
	DisplayName string

	// Admin is a platform administrator, which grants no run:read_data anywhere.
	Admin bool

	// Suspended is an account that opens no session.
	Suspended bool

	CreatedAt    time.Time
	LastSignInAt time.Time
}

// CreateUser writes a user and the principal it is, in one statement.
func (w *Wide) CreateUser(ctx context.Context, u User) error {
	_, err := w.tx.Exec(ctx,
		`with p as (insert into principals (id, kind) values ($1, 'user') returning id)
		 insert into users (login, display_name, admin, suspended)
		 select id, $2, $3, $4 from p`,
		u.Login, u.DisplayName, u.Admin, u.Suspended)
	return principalCreated(err, "user", u.Login)
}

// namesShared is what refuses a login or a namespace whose name the other already has.
const namesShared = "logins_and_namespaces"

// principalCreated answers what writing a principal answered, in this package's words.
func principalCreated(err error, what, name string) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == uniqueViolation && pg.ConstraintName == "principals_pkey" {
		return fmt.Errorf("%w: %s", ErrPrincipalExists, name)
	}
	if errors.As(err, &pg) && pg.ConstraintName == namesShared {
		return fmt.Errorf("%w: %s", ErrNameTaken, name)
	}
	if err != nil {
		return fmt.Errorf("db: %s %s could not be created: %w", what, name, err)
	}
	return nil
}

const userColumns = `login, display_name, admin, suspended, created_at, last_sign_in_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	var signedIn *time.Time
	err := row.Scan(&u.Login, &u.DisplayName, &u.Admin, &u.Suspended, &u.CreatedAt, &signedIn)
	if signedIn != nil {
		u.LastSignInAt = *signedIn
	}
	return u, err
}

// User reads one by login.
func (w *Wide) User(ctx context.Context, login string) (User, error) {
	u, err := scanUser(w.tx.QueryRow(ctx, `select `+userColumns+` from users where login = $1`, login))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("%w: %s", ErrNoPrincipal, login)
	}
	if err != nil {
		return User{}, fmt.Errorf("db: user %s could not be read: %w", login, err)
	}
	return u, nil
}

// Users is the listing, ordered by login.
func (w *Wide) Users(ctx context.Context) ([]User, error) {
	rows, err := w.tx.Query(ctx, `select `+userColumns+` from users order by login`)
	if err != nil {
		return nil, fmt.Errorf("db: the users could not be read: %w", err)
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (User, error) { return scanUser(row) })
	if err != nil {
		return nil, fmt.Errorf("db: the users could not be read: %w", err)
	}
	return users, nil
}

// UpdateUser writes what may change of a user: the display name, whether they administer, and
// whether they are suspended. The login never changes, since it is also the name of their
// personal namespace.
func (w *Wide) UpdateUser(ctx context.Context, u User) error {
	tag, err := w.tx.Exec(ctx,
		`update users set display_name = $2, admin = $3, suspended = $4 where login = $1`,
		u.Login, u.DisplayName, u.Admin, u.Suspended)
	if err != nil {
		return fmt.Errorf("db: user %s could not be written: %w", u.Login, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, u.Login)
	}
	return nil
}

// SignedIn records a user's sign-in.
func (w *Wide) SignedIn(ctx context.Context, login string, at time.Time) error {
	tag, err := w.tx.Exec(ctx, `update users set last_sign_in_at = $2 where login = $1`, login, at)
	if err != nil {
		return fmt.Errorf("db: the sign-in of %s could not be recorded: %w", login, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, login)
	}
	return nil
}

// Administrator is one of the installation's administrators, and whether they can sign in: not
// suspended, and holding a credential.
type Administrator struct {
	Login   string
	SignsIn bool
}

// Administrators answers the installation's administrators, ordered by login, and locks each of
// their rows until the transaction ends, so that two removals of administrators at once take turns
// in one order and the second counts what the first left.
func (w *Wide) Administrators(ctx context.Context) ([]Administrator, error) {
	rows, err := w.tx.Query(ctx,
		`select u.login, not u.suspended and exists (select from credentials c where c.login = u.login)
		   from users u where u.admin order by u.login for update of u`)
	if err != nil {
		return nil, fmt.Errorf("db: the administrators could not be read: %w", err)
	}
	admins, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Administrator])
	if err != nil {
		return nil, fmt.Errorf("db: the administrators could not be read: %w", err)
	}
	return admins, nil
}

// Group is a named set of users. Its principal is group:Name.
type Group struct {
	Name      string
	Members   []string
	CreatedAt time.Time
}

// CreateGroup writes a group with no member, and the principal it is.
func (w *Wide) CreateGroup(ctx context.Context, name string) error {
	_, err := w.tx.Exec(ctx,
		`with p as (insert into principals (id, kind) values ('group:' || $1, 'group') returning id)
		 insert into groups (name) select $1 from p`, name)
	return principalCreated(err, "group", name)
}

// Group reads one with its members, ordered by login.
func (w *Wide) Group(ctx context.Context, name string) (Group, error) {
	g := Group{Name: name}
	err := w.tx.QueryRow(ctx,
		`select g.created_at, coalesce(array_agg(m.login order by m.login) filter (where m.login is not null), '{}')
		   from groups g left join group_members m on m.group_name = g.name
		  where g.name = $1 group by g.name`, name).Scan(&g.CreatedAt, &g.Members)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, fmt.Errorf("%w: group:%s", ErrNoPrincipal, name)
	}
	if err != nil {
		return Group{}, fmt.Errorf("db: group %s could not be read: %w", name, err)
	}
	return g, nil
}

// Groups is the listing, ordered by name, each with its members ordered by login.
func (w *Wide) Groups(ctx context.Context) ([]Group, error) {
	rows, err := w.tx.Query(ctx,
		`select g.name, g.created_at, coalesce(array_agg(m.login order by m.login) filter (where m.login is not null), '{}')
		   from groups g left join group_members m on m.group_name = g.name
		  group by g.name order by g.name`)
	if err != nil {
		return nil, fmt.Errorf("db: the groups could not be read: %w", err)
	}
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Group, error) {
		var g Group
		err := row.Scan(&g.Name, &g.CreatedAt, &g.Members)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the groups could not be read: %w", err)
	}
	return groups, nil
}

// AddMember puts a user in a group, and answers whether they were not in it already. Membership
// touches no grant: the group's grants are what its members gain.
func (w *Wide) AddMember(ctx context.Context, group, login string) (bool, error) {
	tag, err := w.tx.Exec(ctx,
		`insert into group_members (group_name, login) values ($1, $2) on conflict do nothing`, group, login)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		missing := login
		if pg.ConstraintName == "group_members_group_name_fkey" {
			missing = "group:" + group
		}
		return false, fmt.Errorf("%w: %s", ErrNoPrincipal, missing)
	}
	if err != nil {
		return false, fmt.Errorf("db: %s could not be put in group %s: %w", login, group, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveMember takes a user out of a group, and answers whether they were in it.
func (w *Wide) RemoveMember(ctx context.Context, group, login string) (bool, error) {
	tag, err := w.tx.Exec(ctx, `delete from group_members where group_name = $1 and login = $2`, group, login)
	if err != nil {
		return false, fmt.Errorf("db: %s could not be taken out of group %s: %w", login, group, err)
	}
	return tag.RowsAffected() == 1, nil
}

// GroupsOf answers the names of the groups a user is in, ordered, which with the login are the
// access.Principal a question about the user is asked with.
func (w *Wide) GroupsOf(ctx context.Context, login string) ([]string, error) {
	rows, err := w.tx.Query(ctx,
		`select group_name from group_members where login = $1 order by 1`, login)
	if err != nil {
		return nil, fmt.Errorf("db: the groups of %s could not be read: %w", login, err)
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the groups of %s could not be read: %w", login, err)
	}
	return groups, nil
}

// ServiceAccount is a non-human principal of one namespace. Its principal is Namespace/Name.
type ServiceAccount struct {
	Namespace string
	Name      string

	// CreatedBy is who created it, and empty on the namespace's built-in identity, NS/agentiik,
	// which the installation creates with the namespace.
	CreatedBy string
	CreatedAt time.Time
}

// BuiltIn is the name of every namespace's built-in identity, and of no other service account.
const BuiltIn = "agentiik"

// Principal is the string a grant names it by.
func (s ServiceAccount) Principal() string { return s.Namespace + "/" + s.Name }

// CreateServiceAccount writes a service account and the principal it is. One in a namespace
// nobody created is ErrNoNamespace. The built-in identity is written with no CreatedBy, and every
// other with one, which the table holds.
func (w *Wide) CreateServiceAccount(ctx context.Context, s ServiceAccount) error {
	_, err := w.tx.Exec(ctx,
		`with p as (insert into principals (id, kind) values ($1 || '/' || $2, 'service_account') returning id)
		 insert into service_accounts (namespace, name, created_by) select $1, $2, $3 from p`,
		s.Namespace, s.Name, nilIfEmpty(s.CreatedBy))
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == foreignKeyViolation:
		return fmt.Errorf("%w: %s", ErrNoNamespace, s.Namespace)
	case errors.As(err, &pg) && pg.ConstraintName == "service_accounts_built_in":
		return fmt.Errorf("db: %s is written with a creator where it is not the built-in identity, %s/%s, and with none where it is", s.Principal(), s.Namespace, BuiltIn)
	}
	return principalCreated(err, "service account", s.Principal())
}

// ServiceAccounts answers a namespace's, ordered by name.
func (w *Wide) ServiceAccounts(ctx context.Context, namespace string) ([]ServiceAccount, error) {
	return w.ServiceAccountsIn(ctx, []string{namespace})
}

const serviceAccountColumns = `namespace, name, coalesce(created_by, ''), created_at`

// ServiceAccountsIn answers the service accounts of the namespaces named, the built-in identity of
// each among them, ordered by namespace and then by name: what an owner of those namespaces lists.
func (w *Wide) ServiceAccountsIn(ctx context.Context, namespaces []string) ([]ServiceAccount, error) {
	rows, err := w.tx.Query(ctx,
		`select `+serviceAccountColumns+` from service_accounts
		  where namespace = any($1) order by namespace, name`, namespaces)
	if err != nil {
		return nil, fmt.Errorf("db: the service accounts of %v could not be read: %w", namespaces, err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ServiceAccount])
	if err != nil {
		return nil, fmt.Errorf("db: the service accounts of %v could not be read: %w", namespaces, err)
	}
	return accounts, nil
}

// ServiceAccount reads one, or answers ErrNoPrincipal.
func (w *Wide) ServiceAccount(ctx context.Context, namespace, name string) (ServiceAccount, error) {
	var s ServiceAccount
	err := w.tx.QueryRow(ctx, `select `+serviceAccountColumns+` from service_accounts where namespace = $1 and name = $2`,
		namespace, name).Scan(&s.Namespace, &s.Name, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceAccount{}, fmt.Errorf("%w: %s/%s", ErrNoPrincipal, namespace, name)
	}
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("db: service account %s/%s could not be read: %w", namespace, name, err)
	}
	return s, nil
}

// ErrBuiltIn is a namespace's built-in identity where a service account somebody created is meant:
// it is created with its namespace and goes with it, since the runs nobody started there are
// attributed to it, and removing it would leave them attributed to nobody.
var ErrBuiltIn = errors.New("db: that is a namespace's built-in identity")

// RemoveServiceAccount removes a service account and everything it holds, its tokens and its grants
// among them, and answers what it was. The built-in identity is ErrBuiltIn, one nobody created
// ErrNoPrincipal, and one a namespace's record names as owner ErrOwnsNamespace.
func (w *Wide) RemoveServiceAccount(ctx context.Context, namespace, name string) (ServiceAccount, error) {
	if name == BuiltIn {
		return ServiceAccount{}, fmt.Errorf("%w: %s/%s", ErrBuiltIn, namespace, name)
	}
	s, err := w.ServiceAccount(ctx, namespace, name)
	if err != nil {
		return ServiceAccount{}, err
	}
	return s, w.RemovePrincipal(ctx, s.Principal())
}

// GiveBuiltInIdentities creates the built-in identity, NS/agentiik, of every namespace that has
// none, and answers the namespaces it gave one, ordered by name. Nothing is granted to any: it
// "holds no grant until an owner gives it one".
//
// A namespace is created with its built-in identity from v0.3.0, so the ones that lack it are those
// v0.2 made, which an upgrade keeps: init and migrate give it them at every run, and a run that
// finds none lacking gives none. Two runs at once give each namespace one, the second finding the
// first's rows and leaving them. A namespace whose name no principal can be written with, one past
// the 255 characters v0.2 did not bound, is left without one rather than failing the upgrade, and
// is not in the answer.
func (w *Wide) GiveBuiltInIdentities(ctx context.Context) ([]string, error) {
	rows, err := w.tx.Query(ctx,
		`with missing as (
		   select n.name from namespaces n
		    where not exists (select from service_accounts s where s.namespace = n.name and s.name = $1)
		      and agentiik_given_name(n.name)
		 ), principal as (
		   insert into principals (id, kind) select name || '/' || $1, 'service_account' from missing
		   on conflict (id) do nothing
		 )
		 insert into service_accounts (namespace, name) select name, $1 from missing
		 on conflict (namespace, name) do nothing
		 returning namespace`, BuiltIn)
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces could not be given their built-in identities: %w", err)
	}
	given, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces could not be given their built-in identities: %w", err)
	}
	slices.Sort(given)
	return given, nil
}

// PrincipalKind answers what kind of principal a reference names, or ErrNoPrincipal. operator,
// which v0.2 wrote on the rows it left, names none.
func (w *Wide) PrincipalKind(ctx context.Context, principal string) (string, error) {
	var kind string
	err := w.tx.QueryRow(ctx, `select kind from principals where id = $1`, principal).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoPrincipal, principal)
	}
	if err != nil {
		return "", fmt.Errorf("db: principal %s could not be read: %w", principal, err)
	}
	return kind, nil
}

// NamespacesOwnedBy answers the namespaces whose record names principal as owner, ordered by name,
// which is what refuses its removal.
func (w *Wide) NamespacesOwnedBy(ctx context.Context, principal string) ([]Namespace, error) {
	rows, err := w.tx.Query(ctx, `select `+namespaceColumns+` from namespaces where owner = $1 order by name`, principal)
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces %s owns could not be read: %w", principal, err)
	}
	owned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Namespace, error) { return scanNamespace(row) })
	if err != nil {
		return nil, fmt.Errorf("db: the namespaces %s owns could not be read: %w", principal, err)
	}
	return owned, nil
}

// RemovePrincipal removes a user, a group or a service account, and everything it holds with it:
// its credentials, tokens, sessions, enrolment codes, memberships and grants. A principal that
// owns a namespace is refused with ErrOwnsNamespace.
func (w *Wide) RemovePrincipal(ctx context.Context, principal string) error {
	tag, err := w.tx.Exec(ctx, `delete from principals where id = $1`, principal)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation && pg.ConstraintName == "namespaces_owner_fkey" {
		return fmt.Errorf("%w: %s", ErrOwnsNamespace, principal)
	}
	if err != nil {
		return fmt.Errorf("db: principal %s could not be removed: %w", principal, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoPrincipal, principal)
	}
	return nil
}
