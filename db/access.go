package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/access"
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
// it could be opened.
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
	var namespace, held bool
	err := n.tx.QueryRow(ctx,
		`select exists (select from namespaces where name = $1),
		        exists (select from workflows where namespace = $1 and name = $2)`,
		n.namespace, workflow).Scan(&namespace, &held)
	switch {
	case err != nil:
		return fmt.Errorf("db: whether %s is there could not be read: %w", n.namespace, err)
	case !namespace:
		return fmt.Errorf("%w: %s", ErrNoNamespace, n.namespace)
	case workflow != "" && !held:
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
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
