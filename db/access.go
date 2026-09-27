package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Access grants: one principal, one scope, and one role or one denied permission, the rows package
// access resolves. Named access grants, as $defs/accessGrant is, because a grant elsewhere in this
// package is the bearer token a task is dispatched with.
//
// What is here is storage: which rows name a principal or its groups, at which scope, and have not
// expired. What they add up to, a union and then every deny, is access.Resolve's, which reads
// nothing, and these reads hand it its grants as its own type.

// ErrNoAccessGrant is a grant nobody wrote in that namespace, or one already revoked.
var ErrNoAccessGrant = errors.New("db: no access grant of that identifier")

// ErrNoWorkflow is a grant on a workflow its namespace does not hold.
var ErrNoWorkflow = errors.New("db: no workflow of that name in that namespace")

// GrantAccess writes a grant in this handle's namespace, which its scope names, at its GrantedAt or,
// where that is zero, the database's now. The identifier is the caller's, as a run's is. A grant
// access.Grant.Validate refuses is refused here, a principal nobody created is ErrNoPrincipal, and a
// workflow the namespace does not hold is ErrNoWorkflow.
func (n *NS) GrantAccess(ctx context.Context, g access.Grant) error {
	return grantAccess(ctx, n.tx, g, n.namespace)
}

// GrantAccess writes a grant in whichever namespace its scope names, as NS.GrantAccess does in its
// own: for the installation's acts on a namespace, the grant that makes a new namespace's owner one
// among them, which is written in the transaction that creates the namespace, before any handle on
// it could be opened; and a grant carrying a role, written in the transaction that asks whether an
// administrator can still sign in once it applies, which reads their grants in every namespace.
func (w *Wide) GrantAccess(ctx context.Context, g access.Grant) error {
	return grantAccess(ctx, w.tx, g, "")
}

// grantAccess writes g, through a handle bound to the namespace within, or to none where within is
// empty.
func grantAccess(ctx context.Context, tx pgx.Tx, g access.Grant, within string) error {
	if err := g.Validate(); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	switch {
	case g.ID == "" || g.GrantedBy == "":
		return fmt.Errorf("db: an access grant needs an identifier and somebody who granted it, and %q was given by %q", g.ID, g.GrantedBy)
	case within != "" && g.Scope.Namespace != within:
		return fmt.Errorf("db: a grant on %s was written through a handle on %s, and a namespace writes only its own", g.Scope, within)
	}
	_, err := tx.Exec(ctx,
		`insert into grants (id, namespace, workflow, principal, role, deny, expires_at, granted_by, granted_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, coalesce($9, now()))`,
		g.ID, g.Scope.Namespace, nilIfEmpty(g.Scope.Workflow), g.Principal, nilIfEmpty(string(g.Role)),
		nilIfEmpty(string(g.Deny)), g.ExpiresAt, g.GrantedBy, nilIfZero(g.GrantedAt))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		switch pg.ConstraintName {
		case "grants_principal_fkey":
			return fmt.Errorf("%w: %s", ErrNoPrincipal, g.Principal)
		case "grants_namespace_workflow_fkey":
			return fmt.Errorf("%w: %s", ErrNoWorkflow, g.Scope)
		}
		return fmt.Errorf("%w: %s", ErrNoNamespace, g.Scope.Namespace)
	}
	if err != nil {
		return fmt.Errorf("db: access grant %s could not be written: %w", g.ID, err)
	}
	return nil
}

// RevokeAccess removes the grant id written at one scope of this namespace, on the namespace where
// workflow is empty and on that workflow where it is not, and answers it as it was. One written at
// the other scope is ErrNoAccessGrant, as one nobody wrote is: a grant is revoked where it was
// written, so that whoever may share one workflow revokes what was written on it and nothing its
// namespace gives. A revoked grant is gone rather than kept: the audit log is where who granted
// what and who took it back is kept.
func (n *NS) RevokeAccess(ctx context.Context, workflow, id string) (access.Grant, error) {
	revoked, err := collectAccess(n.tx.Query(ctx,
		`delete from grants where namespace = $1 and id = $2 and workflow is not distinct from $3
		  returning `+accessColumns, n.namespace, id, nilIfEmpty(workflow)))
	if err != nil {
		return access.Grant{}, fmt.Errorf("db: access grant %s could not be revoked: %w", id, err)
	}
	if len(revoked) == 0 {
		return access.Grant{}, ErrNoAccessGrant
	}
	return revoked[0], nil
}

// Present answers nil where this namespace exists and, where workflow is not empty, holds that
// workflow, and ErrNoNamespace or ErrNoWorkflow where not. A route an administrator may reach for a
// namespace nobody created tells the absent from the empty with it.
func (n *NS) Present(ctx context.Context, workflow string) error {
	return present(ctx, n.tx, n.namespace, workflow)
}

// Present answers what NS.Present does of namespace, for an act on it the installation's handle
// writes, as a grant written with GrantAccess is.
func (w *Wide) Present(ctx context.Context, namespace, workflow string) error {
	return present(ctx, w.tx, namespace, workflow)
}

func present(ctx context.Context, tx pgx.Tx, namespace, workflow string) error {
	var there, held bool
	err := tx.QueryRow(ctx,
		`select exists (select from namespaces where name = $1),
		        exists (select from workflows where namespace = $1 and name = $2)`,
		namespace, workflow).Scan(&there, &held)
	switch {
	case err != nil:
		return fmt.Errorf("db: whether %s is there could not be read: %w", namespace, err)
	case !there:
		return fmt.Errorf("%w: %s", ErrNoNamespace, namespace)
	case workflow != "" && !held:
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, namespace, workflow)
	}
	return nil
}

const accessColumns = `id, namespace, coalesce(workflow, ''), principal, coalesce(role, ''),
	coalesce(deny, ''), expires_at, granted_by, granted_at`

func scanAccess(row pgx.CollectableRow) (access.Grant, error) {
	var g access.Grant
	err := row.Scan(&g.ID, &g.Scope.Namespace, &g.Scope.Workflow, &g.Principal, &g.Role, &g.Deny,
		&g.ExpiresAt, &g.GrantedBy, &g.GrantedAt)
	return g, err
}

func collectAccess(rows pgx.Rows, err error) ([]access.Grant, error) {
	if err != nil {
		return nil, fmt.Errorf("db: the access grants could not be read: %w", err)
	}
	grants, err := pgx.CollectRows(rows, scanAccess)
	if err != nil {
		return nil, fmt.Errorf("db: the access grants could not be read: %w", err)
	}
	return grants, nil
}

// named is every principal whose grants p holds: itself and each of its groups, as a grant names
// them.
func named(p access.Principal) []string {
	refs := []string{p.Ref}
	for _, g := range p.Groups {
		refs = append(refs, "group:"+g)
	}
	return refs
}

// AccessGrants is every grant of this namespace, expired ones included, ordered by when they were
// written: the listing a principal holding grant:manage reads.
func (n *NS) AccessGrants(ctx context.Context) ([]access.Grant, error) {
	return collectAccess(n.tx.Query(ctx,
		`select `+accessColumns+` from grants where namespace = $1 order by granted_at, id`, n.namespace))
}

// AccessGrantsAt answers the grants and denies that apply at one scope of this namespace and have
// not expired at now, each with the scope it was written at: those written on the namespace, which
// every workflow in it inherits, and where workflow is not empty, those written on that workflow;
// the namespace's first, then each in the order written. A namespace nobody created is
// ErrNoNamespace, and a workflow it does not hold ErrNoWorkflow, so that a listing tells the absent
// from the empty.
func (n *NS) AccessGrantsAt(ctx context.Context, workflow string, now time.Time) ([]access.Grant, error) {
	if err := n.Present(ctx, workflow); err != nil {
		return nil, err
	}
	return collectAccess(n.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where namespace = $1 and (workflow is null or workflow = $2)
		    and (expires_at is null or expires_at > $3)
		  order by workflow nulls first, granted_at, id`, n.namespace, workflow, now))
}

// AccessGrantsFor answers the grants of this namespace that may apply at now to p, its own and its
// groups', on workflow: those on the namespace and those on that workflow, not expired. An empty
// workflow asks about the namespace alone, and answers the grants on the namespace.
func (n *NS) AccessGrantsFor(ctx context.Context, p access.Principal, workflow string, now time.Time) ([]access.Grant, error) {
	return collectAccess(n.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where namespace = $1 and principal = any($2)
		    and (workflow is null or workflow = $3)
		    and (expires_at is null or expires_at > $4)
		  order by granted_at, id`, n.namespace, named(p), workflow, now))
}

// AccessGrantsAcross answers the grants that may apply at now to p, its own and its groups', in
// every namespace and at both scopes, for a listing that spans the namespaces its caller can read.
func (w *Wide) AccessGrantsAcross(ctx context.Context, p access.Principal, now time.Time) ([]access.Grant, error) {
	return collectAccess(w.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where principal = any($1) and (expires_at is null or expires_at > $2)
		  order by namespace, granted_at, id`, named(p), now))
}

// Attribution is the principal a run is attributed to, as it stands now in the namespace the run is
// of: who it is, whether it is still there to hold anything, and the grants of the namespace written
// for it or for one of its groups, the expired ones among them.
//
// It is what the controller asks package access about before it lets a run in, since "authorisation
// is re-evaluated when a run is created", and what it names when the answer is no: the expired
// grants are read because the one that gave the permission is the one whose expiry is the reason.
type Attribution struct {
	// Principal is the principal with the groups it is in now. A service account belongs to
	// none, since a group's members are logins.
	Principal access.Principal

	// Kind is KindUser or KindServiceAccount, and empty where no principal of that name exists:
	// one removed since, with everything it held.
	Kind string

	// Suspended is a user who holds nothing, whatever their grants, since "a suspended account
	// opens no session".
	Suspended bool

	// Grants are every grant of the namespace naming Principal or one of its groups, on the
	// namespace or on the workflow asked about, expired or not, in the order they were written.
	Grants []access.Grant
}

// Attribution reads principal as it stands now in namespace, for a question about workflow, or
// about the namespace alone where workflow is empty. principal is a login or NS/NAME, as a run's
// triggered_by writes it; operator, which names no principal, reads as one that does not exist, and
// so does anything else no principal is named.
func (w *Wide) Attribution(ctx context.Context, namespace, workflow, principal string) (Attribution, error) {
	a := Attribution{Principal: access.Principal{Ref: principal}}
	kind, err := w.PrincipalKind(ctx, principal)
	switch {
	case errors.Is(err, ErrNoPrincipal):
		return a, nil
	case err != nil:
		return Attribution{}, err
	}
	a.Kind = kind
	if kind == KindUser {
		user, err := w.User(ctx, principal)
		if err != nil {
			return Attribution{}, err
		}
		a.Suspended = user.Suspended
		if a.Principal.Groups, err = w.GroupsOf(ctx, principal); err != nil {
			return Attribution{}, err
		}
	}
	a.Grants, err = collectAccess(w.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where namespace = $1 and principal = any($2)
		    and (workflow is null or workflow = $3)
		  order by granted_at, id`, namespace, named(a.Principal), workflow))
	if err != nil {
		return Attribution{}, err
	}
	return a, nil
}

// Revocation is a grant revoked, as the audit log recorded it: the grant as it was, and when and by
// whom it was revoked.
type Revocation struct {
	Grant access.Grant
	At    time.Time
	By    string
}

// revocationsRead bounds how many revocations Revocations reads back through the log, newest first.
// The one a caller is after is the newest that gave a permission, and a principal whose grants in one
// namespace were revoked more often than this is one whose latest revocations say enough.
const revocationsRead = 100

// Revocations answers the grants of namespace revoked from p or from one of its groups, on the
// namespace or on workflow, newest first, as the audit log recorded each: RevokeAccess removes a
// grant rather than keeping it, and the log is where what it was stays. Where the principal, the
// scope or the role an entry records cannot be read as a grant, the entry is passed over, since what is asked is which grant gave something, and an entry that cannot say gave nothing
// anybody could name.
func (w *Wide) Revocations(ctx context.Context, namespace, workflow string, p access.Principal) ([]Revocation, error) {
	scopes := []string{namespace}
	if workflow != "" {
		scopes = append(scopes, namespace+"/"+workflow)
	}
	// The action is written into the statement rather than bound, since the index over the
	// revocations is partial and PostgreSQL uses one only where the statement itself proves its
	// predicate: a parameter, planned once for every value, proves nothing.
	rows, err := w.tx.Query(ctx,
		`select target, actor, at, detail from audit_log
		  where action = '`+audit.GrantDelete+`' and namespace = $1
		    and detail::jsonb->>'principal' = any($2) and detail::jsonb->>'scope' = any($3)
		  order by seq desc limit $4`,
		namespace, named(p), scopes, revocationsRead)
	if err != nil {
		return nil, fmt.Errorf("db: the grants revoked in %s could not be read: %w", namespace, err)
	}
	defer rows.Close()
	var out []Revocation
	for rows.Next() {
		var r Revocation
		var detail string
		if err := rows.Scan(&r.Grant.ID, &r.By, &r.At, &detail); err != nil {
			return nil, fmt.Errorf("db: the grants revoked in %s could not be read: %w", namespace, err)
		}
		var was struct {
			Principal string     `json:"principal"`
			Scope     string     `json:"scope"`
			Role      string     `json:"role"`
			Deny      string     `json:"deny"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal([]byte(detail), &was); err != nil {
			continue
		}
		scope, err := access.ParseScope(was.Scope)
		if err != nil {
			continue
		}
		r.Grant.Principal, r.Grant.Scope = was.Principal, scope
		r.Grant.Role, r.Grant.Deny = access.Role(was.Role), access.Permission(was.Deny)
		r.Grant.ExpiresAt = was.ExpiresAt
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the grants revoked in %s could not be read: %w", namespace, err)
	}
	return out, nil
}
