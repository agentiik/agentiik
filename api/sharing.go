package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Sharing: the grants and denies of a namespace and of one workflow of it, listed, written and
// revoked. GET and POST /api/v1/{ns}/grants and DELETE /api/v1/{ns}/grants/{id}, and the same under
// /api/v1/{ns}/workflows/{name}/grants.
//
// "Grants live in the platform database and are managed through the API and its clients, never in
// the workflow YAML. In the file, workflow:write would equal grant:manage: an editor could make
// themselves owner in the same commit." So a grant is written here and nowhere else, behind
// grant:manage at the scope the route names, and the scope is the route's and never the body's, so
// that nobody writes one where they do not hold grant:manage. An administrator writes one as well,
// since "an administrator may create a grant in any namespace, for anybody, as a power of the
// installation rather than through grant:manage there", and "the namespace's owner is told of
// each, as of a grant an administrator gives themselves"; an administrator widening their own
// access is told the same way, however they came to share: see ownAccess. Listing and revoking are
// grant:manage's alone, since "in a namespace, an administrator holds what their grants give".
//
// Revoking is effective "from the next request and the next run creation", which nothing here has
// to do: Principals reads a principal's grants at every request.
//
// A grant carrying a role brings who it names under the namespace's authentication policy, so it is
// written in a transaction of the installation's, which reads who can sign in across every
// namespace, and refused where it would leave no administrator able to, as a policy changed is
// (keepAnAdministrator): the only administrator given a role where passwords are forbidden would
// find the one they sign in with refused at their next sign-in.

// GrantRequest is a grant or a deny to write at the scope the route names: openapi.json's
// grantCreate, one principal and exactly one of a role and a denied permission, with an optional
// expiry.
type GrantRequest struct {
	// Principal is who it is for: a login, group:NAME or NS/NAME.
	Principal string `json:"principal"`

	// Role is the role granted, for an allow, and Deny the one permission taken away, for a
	// deny.
	Role string `json:"role,omitempty"`
	Deny string `json:"deny,omitempty"`

	// ExpiresAt is when it ends by itself, and nil lasts until it is revoked.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (q *GrantRequest) field(b *body, name string) error {
	switch name {
	case "principal":
		return text(b, &q.Principal)
	case "role":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		return text(b, &q.Role)
	case "deny":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		return text(b, &q.Deny)
	case "expires_at":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		var written string
		if err := text(b, &written); err != nil {
			return err
		}
		at, err := instantOf(written)
		if err != nil {
			return fmt.Errorf("expires_at: %w", err)
		}
		q.ExpiresAt = &at
		return nil
	}
	return unknown(name)
}

// GrantList is openapi.json's grantList: grants and denies, each with the scope it was written at.
type GrantList struct {
	Grants []access.Grant `json:"grants"`
}

// SharingOptions are what the grant routes are given.
type SharingOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: a host that is an IP address
	// is an installation where no passkey signs anybody in and passwords are always allowed, which
	// whether an administrator can sign in once a grant applies depends on. Empty is a name.
	PublicURL string

	// Now is the clock grants are written and expire by, the wall clock where it is nil.
	Now func() time.Time
}

// SharingAPI serves the grant routes.
type SharingAPI struct {
	pool        *db.Pool
	now         func() time.Time
	ipAddressed bool
}

// NewSharing registers the grant routes on a router. The router's authorizer has to say where a
// principal holds a grant, as Principals does, since a grant naming a service account of another
// namespace is answered by what its writer sees of that namespace.
func NewSharing(rt *Router, o SharingOptions) (*SharingAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a grant is a row")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &SharingAPI{pool: o.Pool, now: o.Now}
	if u, err := url.Parse(o.PublicURL); err == nil {
		s.ipAddressed = net.ParseIP(u.Hostname()) != nil
	}
	manage := func(at Scope, writes bool) Needs {
		return Needs{Permission: GrantManage, Scope: at, OrAdministrator: writes, Seeing: writes}
	}
	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"GET", "/api/v1/{namespace}/grants", manage(Namespace, false), s.list},
		{"POST", "/api/v1/{namespace}/grants", manage(Namespace, true), s.create},
		{"DELETE", "/api/v1/{namespace}/grants/{id}", manage(Namespace, false), s.revoke},
		{"GET", "/api/v1/{namespace}/workflows/{workflow}/grants", manage(Workflow, false), s.list},
		{"POST", "/api/v1/{namespace}/workflows/{workflow}/grants", manage(Workflow, true), s.create},
		{"DELETE", "/api/v1/{namespace}/workflows/{workflow}/grants/{id}", manage(Workflow, false), s.revoke},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// The refusals the grant routes say in their own words.
const (
	// A namespace or a workflow that is not there answers what one the caller may not share
	// answers, which is the router's sentence.
	noSuchScope = "no such thing, or not yours"

	// A grant that is not there, or not at this scope, or not the caller's to revoke.
	noSuchGrant = "no such grant here, or not yours"
)

// grantLocksOut is a grant that would leave no administrator able to sign in, in namespace.
func grantLocksOut(namespace string) string {
	return fmt.Sprintf("this grant would leave no administrator able to sign in: it brings those who can now under the authentication policy of %s, which accepts none of the passkeys or passwords they hold, and nobody would be left to administer this installation. Enrol a passkey that policy accepts first", namespace)
}

// namesNobody is the refusal of a grant to a principal that does not exist, or to a service account
// of a namespace its writer does not see: one sentence for the absent and the hidden.
func namesNobody(principal string) string {
	return fmt.Sprintf("principal %s names nobody", principal)
}

// scopeAt is the scope a route names, or false once the request is answered with the absence of a
// scope no grant could name, which an administrator is let through to as the router lets them.
func scopeAt(w http.ResponseWriter, over Target) (access.Scope, bool) {
	at := access.Scope{Namespace: over.Namespace, Workflow: over.Workflow}
	if _, err := access.ParseScope(at.String()); err != nil || over.Namespace == "" {
		fail(w, http.StatusNotFound, noSuchScope)
		return access.Scope{}, false
	}
	return at, true
}

// list is GET .../grants: the grants and denies that apply at the route's scope and have not
// expired, each with the scope it was written at, so that a workflow's list tells what it inherits
// from its namespace from what is written on it.
func (s *SharingAPI) list(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	at, ok := scopeAt(w, over)
	if !ok {
		return
	}
	var grants []access.Grant
	err := s.pool.In(r.Context(), at.Namespace, func(ctx context.Context, n *db.NS) error {
		var err error
		grants, err = n.AccessGrantsAt(ctx, at.Workflow, s.now())
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace), errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, noSuchScope)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the grants could not be read")
		return
	}
	listed := GrantList{Grants: []access.Grant{}}
	for _, g := range grants {
		listed.Grants = append(listed.Grants, answeredGrant(g))
	}
	write(w, http.StatusOK, listed)
}

// create is POST .../grants: one grant or one deny at the route's scope, audited as grant.create in
// the transaction that writes it, and told to the namespace's owners where an administrator wrote
// it by the installation's power, or where it gives a role to an administrator's own access. A role
// that would leave no administrator able to sign in, once the bootstrap token has ended, is a 409
// naming the setting that takes their way in, and nothing is written.
func (s *SharingAPI) create(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	at, ok := scopeAt(w, over)
	if !ok {
		return
	}
	var q GrantRequest
	if err := readAtMost(r, &q, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	// Read off one clock, to the microsecond PostgreSQL keeps, so that the grant answered is the
	// one stored.
	now := s.now().UTC().Truncate(time.Microsecond)
	g := access.Grant{
		ID: ulid.New(), Principal: q.Principal, Scope: at,
		Role: access.Role(q.Role), Deny: access.Permission(q.Deny),
		GrantedBy: string(who), GrantedAt: now,
	}
	if err := g.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.ExpiresAt != nil {
		ends := q.ExpiresAt.UTC().Truncate(time.Microsecond)
		if !ends.After(now) {
			fail(w, http.StatusUnprocessableEntity, "expires_at has already passed: a grant ends after it is written, and one that never began grants nothing")
			return
		}
		g.ExpiresAt = &ends
	}
	// A service account of another namespace is named only where its writer sees that namespace,
	// and answered as nobody otherwise, whether it is there or not: every namespace has its
	// built-in identity, NS/agentiik, so a grant that told the two apart would tell anybody who may
	// share one namespace which others exist.
	if namespace, _, account := strings.Cut(g.Principal, "/"); account && namespace != at.Namespace && !Sees(r)(namespace) {
		fail(w, http.StatusUnprocessableEntity, namesNobody(g.Principal))
		return
	}
	own, err := s.ownAccess(r.Context(), who)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the grant could not be written")
		return
	}

	// The installation's handle rather than the namespace's, since who can sign in once a role
	// applies is read from their grants in every namespace; what is written is the namespace's
	// alone, the grant, its notifications and its entry, as through In. A deny brings nobody
	// under a policy, which reads roles alone, and is written unguarded.
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.Present(ctx, at.Namespace, at.Workflow); err != nil {
			return err
		}
		write := func() error { return wide.GrantAccess(ctx, g) }
		if g.Role != "" {
			if err := keepAnAdministrator(ctx, wide, now, s.ipAddressed, write); err != nil {
				return err
			}
		} else if err := write(); err != nil {
			return err
		}
		detail := grantDetail(g)
		if Administering(r) || (g.Role != "" && own(g.Principal)) {
			told, err := wide.TellOwners(ctx, g, string(who), now)
			if err != nil {
				return err
			}
			detail["notified"] = told
		}
		return wide.AuditIn(ctx, at.Namespace, audit.Record{
			Actor: string(who), Action: audit.GrantCreate, Target: g.ID, Result: audit.Done, Detail: detail,
		})
	})
	var locked *errLockedOut
	switch {
	case errors.Is(err, db.ErrNoNamespace), errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, noSuchScope)
		return
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusUnprocessableEntity, namesNobody(g.Principal))
		return
	case errors.As(err, &locked):
		failSetting(w, http.StatusConflict, grantLocksOut(at.Namespace), locked.setting)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the grant could not be written")
		return
	}
	write(w, http.StatusCreated, answeredGrant(g))
}

// ownAccess answers which principals' access is who's own, where who is an administrator: its own,
// that of a group it belongs to, and that of a service account of a namespace it owns, whose tokens
// it may mint. A role given to one of them, or a deny taken from one, is an administrator widening
// their own access, which "notifies the namespace's owner", since "reading another namespace's
// payloads means granting themselves access first". Whether the route let who through as an
// administrator or by a grant of its own is not asked: an owner who also administers the
// installation widens their own access all the same, and the other owners are the ones to hear of
// it. For anybody who does not administer, it answers no principal.
func (s *SharingAPI) ownAccess(ctx context.Context, who Principal) (func(principal string) bool, error) {
	none := func(string) bool { return false }
	if who == BootstrapOperator || strings.Contains(string(who), "/") {
		return none, nil
	}
	var admin bool
	var groups []string
	err := s.pool.Installation(ctx, db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.User(ctx, string(who))
		if errors.Is(err, db.ErrNoPrincipal) || (err == nil && !user.Admin) {
			return nil
		}
		if err != nil {
			return err
		}
		admin = true
		groups, err = wide.GroupsOf(ctx, user.Login)
		return err
	})
	if err != nil || !admin {
		return none, err
	}
	principal := access.Principal{Ref: string(who), Groups: groups}
	now := s.now()
	var grants []access.Grant
	err = s.pool.Installation(ctx, db.Authorisation, func(ctx context.Context, wide *db.Wide) error {
		var err error
		grants, err = wide.AccessGrantsAcross(ctx, principal, now)
		return err
	})
	if err != nil {
		return none, err
	}
	return func(ref string) bool {
		if ref == string(who) {
			return true
		}
		if group, ok := strings.CutPrefix(ref, "group:"); ok {
			return slices.Contains(groups, group)
		}
		namespace, _, account := strings.Cut(ref, "/")
		return account && access.Owns(principal, grants, namespace, now)
	}, nil
}

// revoke is DELETE .../grants/{id}: one grant or deny written at the route's scope, audited as
// grant.delete in the transaction that removes it. One written at the other scope is answered as
// one that is not there: a namespace's grant is revoked at the namespace's route. A deny taken from
// an administrator's own access widens it as a role given does, and is told to the namespace's
// owners the same way, with the deny as it was.
func (s *SharingAPI) revoke(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	at, ok := scopeAt(w, over)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !ulidForm.MatchString(id) {
		fail(w, http.StatusNotFound, noSuchGrant)
		return
	}
	own, err := s.ownAccess(r.Context(), who)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the grant could not be revoked")
		return
	}
	err = s.pool.In(r.Context(), at.Namespace, func(ctx context.Context, n *db.NS) error {
		gone, err := n.RevokeAccess(ctx, at.Workflow, id)
		if err != nil {
			return err
		}
		detail := grantDetail(gone)
		if gone.Deny != "" && own(gone.Principal) {
			told, err := n.TellOwners(ctx, gone, string(who), s.now().UTC().Truncate(time.Microsecond))
			if err != nil {
				return err
			}
			detail["notified"] = told
		}
		return n.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.GrantDelete, Target: id, Result: audit.Done, Detail: detail,
		})
	})
	switch {
	case errors.Is(err, db.ErrNoAccessGrant):
		fail(w, http.StatusNotFound, noSuchGrant)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the grant could not be revoked")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grantDetail is what the audit log records of a grant besides its identifier, as the owner's grant
// at a namespace's creation is recorded: whom, where, and what, with when it ends where it does.
func grantDetail(g access.Grant) map[string]any {
	detail := map[string]any{"principal": g.Principal, "scope": g.Scope.String()}
	if g.Role != "" {
		detail["role"] = string(g.Role)
	} else {
		detail["deny"] = string(g.Deny)
	}
	if g.ExpiresAt != nil {
		detail["expires_at"] = g.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return detail
}

// answeredGrant is a grant as the wire writes it, its instants in UTC.
func answeredGrant(g access.Grant) access.Grant {
	g.GrantedAt = g.GrantedAt.UTC()
	if g.ExpiresAt != nil {
		ends := g.ExpiresAt.UTC()
		g.ExpiresAt = &ends
	}
	return g
}
