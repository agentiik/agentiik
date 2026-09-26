package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Access grants: one principal, one scope, and one role or one denied permission. Named access
// grants, as $defs/accessGrant is, because a grant elsewhere in this package is the bearer token a
// task is dispatched with.
//
// What is here is storage. Which grants apply is a question of which principals and which scope,
// answered by the reads below; what they add up to, a union and then every deny, is package
// access's, which reads nothing.

// ErrNoAccessGrant is a grant nobody wrote in that namespace, or one already revoked.
var ErrNoAccessGrant = errors.New("db: no access grant of that identifier")

// ErrNoWorkflow is a grant on a workflow its namespace does not hold.
var ErrNoWorkflow = errors.New("db: no workflow of that name in that namespace")

// AccessGrant is one grant. Exactly one of Role and Deny is set.
type AccessGrant struct {
	ID        string
	Namespace string

	// Workflow is the one workflow of the namespace the grant is on, and empty on the namespace.
	Workflow string

	Principal string
	Role      string
	Deny      string

	// ExpiresAt ends the grant by itself, and zero lasts until it is revoked.
	ExpiresAt time.Time

	GrantedBy string
	GrantedAt time.Time
}

// GrantAccess writes a grant in this handle's namespace, at GrantedAt or, where that is zero, the
// database's now. The identifier is the caller's, as a run's is. A principal nobody created is ErrNoPrincipal, and a workflow the namespace does not
// hold is ErrNoWorkflow.
func (n *NS) GrantAccess(ctx context.Context, g AccessGrant) error {
	if g.ID == "" || g.GrantedBy == "" {
		return fmt.Errorf("db: an access grant needs an identifier and somebody who granted it, and %q was given by %q", g.ID, g.GrantedBy)
	}
	_, err := n.tx.Exec(ctx,
		`insert into grants (id, namespace, workflow, principal, role, deny, expires_at, granted_by, granted_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, coalesce($9, now()))`,
		g.ID, n.namespace, nilIfEmpty(g.Workflow), g.Principal, nilIfEmpty(g.Role), nilIfEmpty(g.Deny),
		nilIfZero(g.ExpiresAt), g.GrantedBy, nilIfZero(g.GrantedAt))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		switch pg.ConstraintName {
		case "grants_principal_fkey":
			return fmt.Errorf("%w: %s", ErrNoPrincipal, g.Principal)
		case "grants_namespace_workflow_fkey":
			return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, g.Workflow)
		}
		return fmt.Errorf("%w: %s", ErrNoNamespace, n.namespace)
	}
	if err != nil {
		return fmt.Errorf("db: access grant %s could not be written: %w", g.ID, err)
	}
	return nil
}

// RevokeAccess removes a grant of this namespace. A revoked grant is gone rather than kept: the
// audit log is where who granted what and who took it back is kept.
func (n *NS) RevokeAccess(ctx context.Context, id string) error {
	tag, err := n.tx.Exec(ctx, `delete from grants where namespace = $1 and id = $2`, n.namespace, id)
	if err != nil {
		return fmt.Errorf("db: access grant %s could not be revoked: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAccessGrant
	}
	return nil
}

const accessColumns = `id, namespace, coalesce(workflow, ''), principal, coalesce(role, ''), coalesce(deny, ''),
	expires_at, granted_by, granted_at`

func scanAccess(row pgx.CollectableRow) (AccessGrant, error) {
	var g AccessGrant
	var expires *time.Time
	err := row.Scan(&g.ID, &g.Namespace, &g.Workflow, &g.Principal, &g.Role, &g.Deny, &expires,
		&g.GrantedBy, &g.GrantedAt)
	if expires != nil {
		g.ExpiresAt = *expires
	}
	return g, err
}

func collectAccess(rows pgx.Rows, err error) ([]AccessGrant, error) {
	if err != nil {
		return nil, fmt.Errorf("db: the access grants could not be read: %w", err)
	}
	grants, err := pgx.CollectRows(rows, scanAccess)
	if err != nil {
		return nil, fmt.Errorf("db: the access grants could not be read: %w", err)
	}
	return grants, nil
}

// AccessGrants is every grant of this namespace, expired ones included, ordered by when they were
// written: the listing a principal holding grant:manage reads.
func (n *NS) AccessGrants(ctx context.Context) ([]AccessGrant, error) {
	return collectAccess(n.tx.Query(ctx,
		`select `+accessColumns+` from grants where namespace = $1 order by granted_at, id`, n.namespace))
}

// AccessGrantsFor answers the grants of this namespace that apply at now to any of principals on
// workflow: those on the namespace and those on that workflow, not expired. An empty workflow is
// a question about the namespace alone, and answers the grants on the namespace. The principals
// are a user's login and its groups, or a service account.
func (n *NS) AccessGrantsFor(ctx context.Context, principals []string, workflow string, now time.Time) ([]AccessGrant, error) {
	return collectAccess(n.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where namespace = $1 and principal = any($2)
		    and (workflow is null or workflow = $3)
		    and (expires_at is null or expires_at > $4)
		  order by granted_at, id`, n.namespace, principals, workflow, now))
}

// AccessGrantsAcross answers the grants that apply at now to any of principals, in every
// namespace and at both scopes, for a listing that spans the namespaces its caller can read.
func (w *Wide) AccessGrantsAcross(ctx context.Context, principals []string, now time.Time) ([]AccessGrant, error) {
	return collectAccess(w.tx.Query(ctx,
		`select `+accessColumns+` from grants
		  where principal = any($1) and (expires_at is null or expires_at > $2)
		  order by namespace, granted_at, id`, principals, now))
}
