package db

import (
	"context"
	"errors"
	"fmt"
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
	rows, err := w.tx.Query(ctx,
		`select namespace, name, coalesce(created_by, ''), created_at from service_accounts
		  where namespace = $1 order by name`, namespace)
	if err != nil {
		return nil, fmt.Errorf("db: the service accounts of %s could not be read: %w", namespace, err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ServiceAccount])
	if err != nil {
		return nil, fmt.Errorf("db: the service accounts of %s could not be read: %w", namespace, err)
	}
	return accounts, nil
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
