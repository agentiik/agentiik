package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/db"
)

// Who the caller is: GET /api/v1/me, "identity, group memberships, effective permissions per
// namespace, and notifications", and DELETE /api/v1/me/notifications/{id}, which dismisses one; and
// what a user says of themself, PATCH /api/v1/me, and their photo, at /api/v1/me/avatar (profile.go,
// avatar.go).
//
// What it answers is the caller's own, so every route takes Own. The permissions are what the
// caller's grants resolve to at this moment, narrowed by the credential it presented, which is what
// agk whoami prints and what a console reads to hide what the caller does not hold rather than
// disable it.

// Me is openapi.json's me.
type Me struct {
	// Principal is the caller as a grant and the audit log write it: a login, NS/NAME for a
	// service account, and operator for the bootstrap token.
	Principal string `json:"principal"`

	// Admin is whether the caller administers the installation through the credential it
	// presented.
	Admin bool `json:"admin"`

	// User and ServiceAccount are the record behind the caller, whichever it is, and neither for
	// the bootstrap token.
	User           *User           `json:"user,omitempty"`
	ServiceAccount *ServiceAccount `json:"service_account,omitempty"`

	// Groups are the groups the caller belongs to, as a grant names them, group:NAME.
	Groups []string `json:"groups"`

	// Permissions are what the caller holds, keyed by scope as a grant writes it: see Effective.
	Permissions map[string][]Permission `json:"permissions"`

	// Notifications are what the installation tells the caller, newest first.
	Notifications []Notification `json:"notifications"`
}

// ServiceAccount is a service account as the routes answer one, $defs/serviceAccount.
type ServiceAccount struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`

	// CreatedBy is who created it, and empty for a namespace's built-in identity, NS/agentiik,
	// which the installation creates with the namespace.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Notification is one thing the installation tells the caller, $defs/notification: an
// administrator having widened access in a namespace, with where, the grant, which act and who did
// it, and the user put in a group where the act was that; a sign-in refused for a passkey's
// signature counter, with the passkey; or the break-glass path having issued an administrator a
// recovery code, with whose account.
type Notification struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	At         time.Time     `json:"at"`
	Act        string        `json:"act,omitempty"`
	By         string        `json:"by,omitempty"`
	Namespace  string        `json:"namespace,omitempty"`
	Grant      *access.Grant `json:"grant,omitempty"`
	Credential string        `json:"credential,omitempty"`
	Login      string        `json:"login,omitempty"`
}

// MeOptions are what the routes about the caller are given.
type MeOptions struct {
	Pool *db.Pool

	// Now is the clock notifications are kept by, the wall clock where it is nil.
	Now func() time.Time
}

// MeAPI serves GET /api/v1/me, the dismissal of a notification, and the caller's profile and photo.
type MeAPI struct {
	pool *db.Pool
	now  func() time.Time
}

// NewMe registers the routes about the caller on a router, each taking Own. The router's authorizer
// has to say what a principal is granted, as Principals does, as Standings.
func NewMe(rt *Router, o MeOptions) (*MeAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and who a caller is is written there")
	}
	if _, ok := rt.auth.(Standings); !ok {
		return nil, errors.New("api: the authorizer does not say what a principal is granted, which GET /api/v1/me answers")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	m := &MeAPI{pool: o.Pool, now: o.Now}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"GET", "/api/v1/me", m.me},
		{"PATCH", "/api/v1/me", m.updateProfile},
		{"DELETE", "/api/v1/me/notifications/{id}", m.dismiss},
		{"GET", "/api/v1/me/avatar", m.avatar},
		{"PUT", "/api/v1/me/avatar", m.setAvatar},
		{"DELETE", "/api/v1/me/avatar", m.removeAvatar},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// errGone is a caller whose record went between the request being identified and being answered.
var errGone = errors.New("api: the caller's record is gone")

// me is GET /api/v1/me.
func (m *MeAPI) me(w http.ResponseWriter, r *http.Request, caller Caller) { m.answer(w, r, caller) }

// answer writes who the caller is, as GET /api/v1/me answers it, and as PATCH /api/v1/me answers it
// once the profile is written, so that a client holds the one document it reads either way.
//
// A credential narrowed by a scope is answered no notification: what is told to a principal is its
// own, of the installation's accord, and reading it, as dismissing it, is none of the nine a scope
// keeps some of, as managing credentials is not. So a script holding such a token neither reads
// that an administrator widened their access nor makes the notice go away before its owner reads
// it.
func (m *MeAPI) answer(w http.ResponseWriter, r *http.Request, caller Caller) {
	held, err := caller.Effective(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller holds could not be read")
		return
	}
	answer := Me{
		Principal: string(caller.Principal), Admin: held.Admin,
		Groups: []string{}, Permissions: map[string][]Permission{}, Notifications: []Notification{},
	}
	for _, g := range held.Groups {
		answer.Groups = append(answer.Groups, "group:"+g)
	}
	for at, set := range held.Permissions {
		answer.Permissions[at.String()] = append([]Permission{}, set.Permissions()...)
	}
	err = m.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if caller.Principal == BootstrapOperator {
			return nil
		}
		if namespace, name, account := strings.Cut(string(caller.Principal), "/"); account {
			accounts, err := wide.ServiceAccounts(ctx, namespace)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(accounts, func(a db.ServiceAccount) bool { return a.Name == name })
			if i < 0 {
				return errGone
			}
			answer.ServiceAccount = &ServiceAccount{
				Kind: db.KindServiceAccount, Namespace: namespace, Name: name,
				CreatedBy: accounts[i].CreatedBy, CreatedAt: accounts[i].CreatedAt.UTC(),
			}
		} else {
			user, err := wide.User(ctx, string(caller.Principal))
			if errors.Is(err, db.ErrNoPrincipal) {
				return errGone
			}
			if err != nil {
				return err
			}
			answered := userOf(user)
			answer.User = &answered
		}
		if caller.Narrowed() {
			return nil
		}
		told, err := wide.NotificationsOf(ctx, string(caller.Principal), m.now())
		for _, t := range told {
			answer.Notifications = append(answer.Notifications, notificationOf(t))
		}
		return err
	})
	switch {
	case errors.Is(err, errGone):
		// Removed since its credential was looked up: the credential opens nothing now.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noToken)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "who the caller is could not be read")
		return
	}
	write(w, http.StatusOK, answer)
}

// dismiss is DELETE /api/v1/me/notifications/{id}: one of the caller's notifications, which GET
// /api/v1/me lists no more. It changes nothing of what happened: the grant.create or the
// signin.fail the audit log recorded stays. One that is not the caller's, one dismissed already and
// one past its 90 days are the same absence, and a credential narrowed by a scope dismisses none,
// as it reads none.
func (m *MeAPI) dismiss(w http.ResponseWriter, r *http.Request, caller Caller) {
	id := r.PathValue("id")
	if !ulidForm.MatchString(id) || caller.Narrowed() || caller.Principal == BootstrapOperator {
		fail(w, http.StatusNotFound, noSuchNotification)
		return
	}
	err := m.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		return wide.DismissNotification(ctx, string(caller.Principal), id, m.now())
	})
	switch {
	case errors.Is(err, db.ErrNoNotification):
		fail(w, http.StatusNotFound, noSuchNotification)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the notification could not be dismissed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// noSuchNotification is a notification the caller is not told, whichever reason it is.
const noSuchNotification = "no such notification, or not yours"

// notificationOf is a notification as the wire writes it, its instants in UTC.
func notificationOf(t db.Notification) Notification {
	told := Notification{
		ID: t.ID, Kind: t.Kind, At: t.At.UTC(), Act: t.Act, By: t.By,
		Namespace: t.Namespace, Credential: t.Credential, Login: t.Login,
	}
	if t.Grant != nil {
		g := answeredGrant(*t.Grant)
		told.Grant = &g
	}
	return told
}

// Standing is the router's Standings: what who is granted, read now. The grants are those of who
// and of its groups, in every namespace and at both scopes, not expired, as HeldIn and Owned read
// them. The bootstrap operator, while it has not ended, holds the owner role on every namespace, as
// Allow answers it, which is written here as the grants it would be; a suspended user, and a login
// removed since, are granted nothing.
func (p *Principals) Standing(ctx context.Context, who Principal) (Standing, error) {
	now := p.now()
	if who == "" {
		return Standing{At: now}, nil
	}
	principal, _, bootstrapped, err := p.resolve(ctx, who)
	if err != nil {
		return Standing{}, err
	}
	standing := Standing{Groups: principal.Groups, At: now}
	err = p.pool.Installation(ctx, db.Authorisation, func(ctx context.Context, w *db.Wide) error {
		if who == BootstrapOperator {
			if !bootstrapped {
				return nil
			}
			all, err := w.Namespaces(ctx)
			for _, n := range all {
				standing.Grants = append(standing.Grants, access.Grant{
					Principal: string(BootstrapOperator), Scope: access.Scope{Namespace: n.Name}, Role: access.Owner,
				})
			}
			return err
		}
		if principal.Ref == "" {
			return nil
		}
		var err error
		standing.Grants, err = w.AccessGrantsAcross(ctx, principal, now)
		return err
	})
	if err != nil {
		return Standing{}, err
	}
	return standing, nil
}
