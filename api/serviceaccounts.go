package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// Service accounts: GET and POST /api/v1/service-accounts, and DELETE
// /api/v1/service-accounts/{ns}/{name}.
//
// A service account is "non-human, holds API tokens, cannot sign in to a client", and is written
// NS/NAME wherever a principal is written. It is created in a namespace its creator owns and
// listed and removed by whoever owns that namespace, "one on which the caller holds an unexpired
// grant of the owner role, its own or one of its groups'": no permission names that, so the routes
// take Own, as a token's do, and each asks what the caller owns through the credential it
// presented. A namespace the caller does not own is answered as one that does not exist.
//
// It holds no grant until one is written for it and no token until one is minted for it at POST
// /api/v1/auth/tokens, so creating one gives nobody anything. It holds no credential a person signs
// in with either: credentials, sessions and enrolment links belong to users, and the store refuses
// them to anybody else, which is also why the authentication policy, which says how people prove
// who they are, does not apply to it.
//
// Removing one removes what it holds with it, its tokens and its grants, from the next request
// they would have made. Each creation and removal is recorded in the audit log in the namespace it
// belongs to, where whoever reads what was done there looks for who was given a way in.
//
// Every namespace has one more, NS/agentiik, which the installation creates with the namespace and
// which nobody may create or remove: "scheduled, webhook and event runs are attributed" to it.

// ServiceAccountOptions are what the service account routes are given.
type ServiceAccountOptions struct {
	Pool *db.Pool
}

// ServiceAccountAPI serves the service account routes.
type ServiceAccountAPI struct {
	pool *db.Pool
}

// NewServiceAccounts registers the service account routes on a router, each a route about what
// its caller owns: see Own.
//
// The removal's path names a namespace, {ns}, which the router does not authorise, since what
// authorises it is owning the namespace and not a permission there: the handler answers one the
// caller does not own through the credential it presented as the absence it is, before anything
// else is read.
func NewServiceAccounts(rt *Router, o ServiceAccountOptions) (*ServiceAccountAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a service account is a row")
	}
	s := &ServiceAccountAPI{pool: o.Pool}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"GET", "/api/v1/service-accounts", s.list},
		{"POST", "/api/v1/service-accounts", s.create},
		{"DELETE", "/api/v1/service-accounts/{ns}/{name}", s.remove},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ServiceAccountName refuses a name no service account can be created under: one outside the
// grammar and the reserved words a namespace is named with, which $defs/serviceAccount holds each
// half of NS/NAME to, and agentiik, "the built-in identity's name in every namespace and no other
// service account's".
func ServiceAccountName(name string) error { return serviceAccountName(name, agk.IsReservedNamespace) }

// serviceAccountRef refuses a name no service account can carry: ServiceAccountName's refusals,
// save a word reserved late, as NamespaceRef reads one.
func serviceAccountRef(name string) error { return serviceAccountName(name, agk.NamesNoNamespace) }

func serviceAccountName(name string, reserved func(string) bool) error {
	switch {
	case name == "":
		return errors.New("name: a service account has a name, lowercase words joined by hyphens, such as nightly-sync")
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("name: a service account's name is at most %d characters and this one is %d", agk.IdentifierMaxBytes, len(name))
	case !givenName.MatchString(name):
		return fmt.Errorf("name: %.64q is not a service account's name: one is named in lowercase words joined by hyphens, such as nightly-sync, as a namespace is", name)
	case reserved(name):
		return fmt.Errorf("name: %s is reserved: it is %s, which names no namespace and so neither half of a service account", name, routesOn(name))
	case name == db.BuiltIn:
		return fmt.Errorf("name: %s is the name of every namespace's built-in identity, which the installation creates with the namespace, and of no other service account", db.BuiltIn)
	}
	return nil
}

// NewServiceAccount is a service account to create, openapi.json's serviceAccountCreate.
type NewServiceAccount struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (n *NewServiceAccount) field(b *body, name string) error {
	switch name {
	case "namespace":
		return text(b, &n.Namespace)
	case "name":
		return text(b, &n.Name)
	}
	return unknown(name)
}

// serviceAccountOf is a stored service account as the routes answer one, ServiceAccount, which GET
// /api/v1/me answers too.
func serviceAccountOf(s db.ServiceAccount) ServiceAccount {
	return ServiceAccount{
		Kind: db.KindServiceAccount, Namespace: s.Namespace, Name: s.Name, CreatedBy: s.CreatedBy,
		CreatedAt: s.CreatedAt.UTC(),
	}
}

// ServiceAccountList is openapi.json's serviceAccountList: by namespace, then by name.
type ServiceAccountList struct {
	ServiceAccounts []ServiceAccount `json:"service_accounts"`
}

// noServiceAccount is the refusal of a service account the caller may not remove, whether it
// exists or not: one sentence for both, so that asking teaches nothing.
const noServiceAccount = "no such service account, or not of a namespace you own"

// unowned says that a namespace is not the caller's, as one that does not exist would be said,
// and why a narrowed token owns none, which is the caller's own credential to know about.
func unowned(caller Caller, namespace string) string {
	said := fmt.Sprintf("namespace %s does not exist, or is not one you own", namespace)
	if caller.Narrowed() {
		said += ", and a token narrowed by a scope owns none"
	}
	return said
}

// list is GET /api/v1/service-accounts: the service accounts of the namespaces the caller owns, the
// built-in identity of each among them.
func (s *ServiceAccountAPI) list(w http.ResponseWriter, r *http.Request, caller Caller) {
	owned, err := caller.Owned(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
		return
	}
	var found []db.ServiceAccount
	if len(owned) > 0 {
		err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
			var err error
			found, err = wide.ServiceAccountsIn(ctx, owned)
			return err
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, "the service accounts could not be read")
			return
		}
	}
	listed := ServiceAccountList{ServiceAccounts: make([]ServiceAccount, 0, len(found))}
	for _, sa := range found {
		listed.ServiceAccounts = append(listed.ServiceAccounts, serviceAccountOf(sa))
	}
	write(w, http.StatusOK, listed)
}

// create is POST /api/v1/service-accounts: a service account in a namespace the caller owns,
// created by the caller, holding no grant and no token.
func (s *ServiceAccountAPI) create(w http.ResponseWriter, r *http.Request, caller Caller) {
	var ask NewServiceAccount
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := NamespaceRef(ask.Namespace); err != nil {
		fail(w, http.StatusBadRequest, "namespace: "+err.Error())
		return
	}
	if err := ServiceAccountName(ask.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	owned, err := caller.Owned(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
		return
	}
	if !slices.Contains(owned, ask.Namespace) {
		fail(w, http.StatusUnprocessableEntity, unowned(caller, ask.Namespace))
		return
	}

	var made db.ServiceAccount
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.CreateServiceAccount(ctx, db.ServiceAccount{
			Namespace: ask.Namespace, Name: ask.Name, CreatedBy: string(caller.Principal),
		}); err != nil {
			return err
		}
		// Read back, since when it was created is the database's to say.
		var err error
		if made, err = wide.ServiceAccount(ctx, ask.Namespace, ask.Name); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, caller.Principal); err != nil {
			return err
		}
		return wide.AuditIn(ctx, ask.Namespace, audit.Record{
			Actor: string(caller.Principal), Action: audit.ServiceAccountCreate, Target: made.Principal(), Result: audit.Done,
		})
	})
	switch {
	case errors.Is(err, db.ErrPrincipalExists):
		fail(w, http.StatusConflict, fmt.Sprintf("namespace %s already has a service account %s", ask.Namespace, ask.Name))
	case errors.Is(err, db.ErrNoNamespace):
		// Removed between the question of what the caller owns and the insert.
		fail(w, http.StatusUnprocessableEntity, unowned(caller, ask.Namespace))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the service account could not be created")
	default:
		write(w, http.StatusCreated, serviceAccountOf(made))
	}
}

// remove is DELETE /api/v1/service-accounts/{ns}/{name}: a service account of a namespace the
// caller owns, with its tokens and its grants. The built-in identity is refused.
func (s *ServiceAccountAPI) remove(w http.ResponseWriter, r *http.Request, caller Caller) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	namespace, name := r.PathValue("ns"), r.PathValue("name")
	// A name no service account can have is one nobody could have created, answered as absent
	// before anything is asked, agentiik aside, which is refused as what it is once the
	// namespace is known to be the caller's.
	if NamespaceRef(namespace) != nil || (name != db.BuiltIn && serviceAccountRef(name) != nil) {
		fail(w, http.StatusNotFound, noServiceAccount)
		return
	}
	owned, err := caller.Owned(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
		return
	}
	if !slices.Contains(owned, namespace) {
		fail(w, http.StatusNotFound, noServiceAccount)
		return
	}
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		gone, err := wide.RemoveServiceAccount(ctx, namespace, name)
		if err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, caller.Principal); err != nil {
			return err
		}
		return wide.AuditIn(ctx, namespace, audit.Record{
			Actor: string(caller.Principal), Action: audit.ServiceAccountDelete, Target: gone.Principal(), Result: audit.Done,
			Detail: map[string]any{"created_by": gone.CreatedBy},
		})
	})
	switch {
	case errors.Is(err, db.ErrBuiltIn):
		fail(w, http.StatusConflict, fmt.Sprintf("%s/%s is the namespace's built-in identity, which the runs nobody started there are attributed to, and it goes only with its namespace", namespace, db.BuiltIn))
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noServiceAccount)
	case errors.Is(err, db.ErrOwnsNamespace):
		fail(w, http.StatusConflict, fmt.Sprintf("%s/%s owns a namespace, and is not removed while a namespace's record names it as owner, so that none is left owned by somebody who is gone: give it another owner first", namespace, name))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the service account could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
