package api

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The namespaces, as an administrator creates, bounds and removes them, and as whoever holds a
// grant in one reads it: /api/v1/namespaces.
//
// "A platform administrator manages users, groups, namespaces, quotas, runners, runner policies and
// the authentication policy", so every change here is an administrator's, asked as grant:manage at
// the installation as the runner pools are, and answered 403 to anybody else before the namespace
// is looked up. Reading is wider: a namespace's record is "to an administrator and to a principal
// holding a grant in it; anyone else is answered the 404 of one that does not exist", which the
// router answers through OnNamespace.
//
// Everything is asked and answered in the shapes wire.schema.json gives it, $defs/namespaceRecord
// and $defs/quotas, which a console, the Terraform provider and agk are written against.

// NamespaceName refuses a name no namespace can be created under: one outside $defs/namespace,
// lowercase words joined by hyphens, one longer than a name is, or one of the words the API
// routes on.
//
// It is the check POST /api/v1/namespaces and agentiik-api namespace create both make, so that the
// route and the verb refuse the same names.
func NamespaceName(name string) error {
	switch {
	case name == "":
		return errors.New("a namespace has a name, lowercase words joined by hyphens, such as finance or team-ops")
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("a namespace's name is at most %d characters and this one is %d: it is written in every path of the API and every key of the object store, and no filesystem holds a longer name", agk.IdentifierMaxBytes, len(name))
	case !givenName.MatchString(name):
		return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens, such as finance or team-ops", name)
	case agk.IsReservedNamespace(name):
		return fmt.Errorf("%s is a word the API routes on: the first path segment after /api/v1/ decides the route, so %s cannot name a namespace", name, strings.Join(agk.ReservedNamespaces, ", "))
	}
	return nil
}

// NamespaceRecord is a namespace as the wire writes it, $defs/namespaceRecord: its name, whether it
// is somebody's personal namespace or a shared one, its owner where it has one, and its quotas.
//
// It is also what POST /api/v1/namespaces reads, the openapi's namespaceCreate: a name, an owner,
// and quotas where they are not the defaults. Kind is shared there, and shared where it is left
// out, since "every user owns a personal namespace named after their login, created on first
// sign-in", and never by an administrator.
type NamespaceRecord struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind,omitempty"`
	Owner  string  `json:"owner,omitempty"`
	Quotas *Quotas `json:"quotas,omitempty"`
}

func (n *NamespaceRecord) field(b *body, name string) error {
	switch name {
	case "name":
		return text(b, &n.Name)
	case "kind":
		if err := notNullHere(b, "shared, the kind of namespace created here"); err != nil {
			return err
		}
		if err := text(b, &n.Kind); err != nil {
			return err
		}
		switch n.Kind {
		case db.NamespaceShared:
			return nil
		case db.NamespacePersonal:
			return errors.New("a personal namespace is created at its user's first sign-in, named after their login, and never by an administrator: a namespace created here is shared")
		}
		return fmt.Errorf("%.64q is not a kind of namespace: one is shared or personal, and one created here is shared", n.Kind)
	case "owner":
		return text(b, &n.Owner)
	case "quotas":
		if err := notNullHere(b, "the namespace's quotas, an object"); err != nil {
			return err
		}
		var q Quotas
		if err := b.fields(&q); err != nil {
			return err
		}
		n.Quotas = &q
		return nil
	}
	return unknown(name)
}

// Quotas are what one namespace may consume, as the wire writes them, $defs/quotas.
//
// Each is left out where it is not set, and none is ever written as zero or empty: each count
// "starts at one", and an empty allowed_runner_pools is refused rather than read. An answer always
// writes max_concurrent_tasks and max_retention_days, which "always hold a value, 20 and 90 until an
// administrator sets another", and the other four where they are set.
type Quotas struct {
	MaxConcurrentTasks int      `json:"max_concurrent_tasks,omitempty"`
	MaxRunsPerHour     int      `json:"max_runs_per_hour,omitempty"`
	MaxArtifactBytes   int64    `json:"max_artifact_bytes,omitempty"`
	MaxRetentionDays   int      `json:"max_retention_days,omitempty"`
	MaxRunDuration     string   `json:"max_run_duration,omitempty"`
	AllowedRunnerPools []string `json:"allowed_runner_pools,omitempty"`
}

// quotaCountMax is the largest count a quota holds, other than max_artifact_bytes: the column is a
// PostgreSQL integer, and one past it would be the database's refusal rather than this request's.
// Two thousand million runs an hour, days of retention or concurrent tasks bounds nothing a
// namespace could reach, so a larger one is a mistake worth saying.
const quotaCountMax = math.MaxInt32

// durationForm is a timeout as the wire writes one, $defs/taskMessage/properties/timeout, which
// max_run_duration is written on: "one number and one unit taken from ms, s, m, h and d".
var durationForm = regexp.MustCompile(`^[1-9][0-9]*(ms|s|m|h|d)$`)

func (q *Quotas) field(b *body, name string) error {
	count := func(into *int) error {
		if err := notNullHere(b, "a whole number from 1"); err != nil {
			return err
		}
		if err := integer(b, into); err != nil {
			return err
		}
		if *into < 1 || *into > quotaCountMax {
			return fmt.Errorf("%s is %d, and it is a whole number from 1 to %d: a quota bounds something a namespace does, zero would forbid it outright, and a quota left out is how a namespace goes without one", name, *into, quotaCountMax)
		}
		return nil
	}
	switch name {
	case "max_concurrent_tasks":
		return count(&q.MaxConcurrentTasks)
	case "max_runs_per_hour":
		return count(&q.MaxRunsPerHour)
	case "max_retention_days":
		return count(&q.MaxRetentionDays)
	case "max_artifact_bytes":
		if err := notNullHere(b, "a whole number of bytes from 1"); err != nil {
			return err
		}
		if err := integer(b, &q.MaxArtifactBytes); err != nil {
			return err
		}
		if q.MaxArtifactBytes < 1 {
			return fmt.Errorf("max_artifact_bytes is %d, and it is a whole number of bytes from 1: a quota left out is how a namespace goes without one", q.MaxArtifactBytes)
		}
		return nil
	case "max_run_duration":
		if err := notNullHere(b, "a duration written as a step's timeout is"); err != nil {
			return err
		}
		if err := text(b, &q.MaxRunDuration); err != nil {
			return err
		}
		return runDuration(q.MaxRunDuration)
	case "allowed_runner_pools":
		if err := notNullHere(b, "the names of runner pools"); err != nil {
			return err
		}
		if err := texts(b, &q.AllowedRunnerPools, namesMax, fmt.Sprintf("allowed_runner_pools names at most %d runner pools", namesMax)); err != nil {
			return err
		}
		if len(q.AllowedRunnerPools) == 0 {
			return errors.New("allowed_runner_pools names no pool: a namespace allowed every pool that accepts it leaves the quota out, since a pool's own empty list of namespaces means every one, and the same spelling here would be read as its opposite")
		}
		return distinct(q.AllowedRunnerPools, "runner pool", func(pool string) error {
			if len(pool) > poolNameMax || !givenName.MatchString(pool) {
				return fmt.Errorf("%.64q is not a runner pool's name: a pool is named in lowercase words joined by hyphens, at most %d characters", pool, poolNameMax)
			}
			return nil
		})
	}
	return unknown(name)
}

// runDuration refuses a max_run_duration off the timeout grammar, and one no clock measures: past
// what a duration of 64 bits holds, it would bound nothing and read as a negative bound to whoever
// compared a run's timeout against it.
func runDuration(written string) error {
	grammar := fmt.Errorf("max_run_duration is %.64q: it is written as a step's timeout is, one number and one unit taken from ms, s, m, h and d, such as 24h", written)
	match := durationForm.FindStringSubmatch(written)
	if match == nil {
		return grammar
	}
	unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[match[1]]
	n, err := strconv.ParseInt(strings.TrimSuffix(written, match[1]), 10, 64)
	if err != nil || n > math.MaxInt64/int64(unit) {
		return fmt.Errorf("max_run_duration is %.64q, longer than a clock measures: a bound on how long a run may take is at most a few hundred years", written)
	}
	return nil
}

// notNullHere refuses null where a namespace writes its kind, its quotas or one quota, and reads
// nothing otherwise. The wire allows null for none of them, and read as left out, a null quota would
// lift a bound, or keep one, from a value somebody meant to set, which is what a client sends for a
// variable it left unset.
func notNullHere(b *body, want string) error {
	if b.d.PeekKind() != jsontext.KindNull {
		return nil
	}
	if _, err := b.d.ReadToken(); err != nil {
		return malformed(err)
	}
	return fmt.Errorf("the request body holds null at %.100q, where it holds %s: what a namespace does not set is left out, and null is refused rather than read as left out", b.d.StackPointer(), want)
}

// NamespaceList is what GET /api/v1/namespaces answers: the openapi's namespaceList.
type NamespaceList struct {
	Namespaces []NamespaceRecord `json:"namespaces"`
}

// recordOf is a namespace as it is answered.
func recordOf(n db.Namespace) NamespaceRecord {
	q := quotasOf(n.Quotas)
	return NamespaceRecord{Name: n.Name, Kind: n.Kind, Owner: n.Owner, Quotas: &q}
}

func quotasOf(q db.Quotas) Quotas {
	return Quotas{
		MaxConcurrentTasks: q.MaxConcurrentTasks, MaxRunsPerHour: q.MaxRunsPerHour,
		MaxArtifactBytes: q.MaxArtifactBytes, MaxRetentionDays: q.MaxRetentionDays,
		MaxRunDuration: q.MaxRunDuration, AllowedRunnerPools: q.AllowedRunnerPools,
	}
}

// stored is the quotas as the store writes them: a zero count, or no pool, where one is left out.
func (q Quotas) stored() db.Quotas {
	return db.Quotas{
		MaxConcurrentTasks: q.MaxConcurrentTasks, MaxRunsPerHour: q.MaxRunsPerHour,
		MaxArtifactBytes: q.MaxArtifactBytes, MaxRetentionDays: q.MaxRetentionDays,
		MaxRunDuration: q.MaxRunDuration, AllowedRunnerPools: q.AllowedRunnerPools,
	}
}

// NamespaceOptions are what the namespace routes are given.
type NamespaceOptions struct {
	Pool *db.Pool
}

// NamespaceAPI serves /api/v1/namespaces.
type NamespaceAPI struct {
	pool *db.Pool
}

// NewNamespaces registers the namespace routes on a router. The router's authorizer has to say
// which namespaces a principal holds a grant in, as Principals does, since reading a namespace is
// answered to them: see OnNamespace.
func NewNamespaces(rt *Router, o NamespaceOptions) (*NamespaceAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a namespace is a row")
	}
	s := &NamespaceAPI{pool: o.Pool}

	admin := Needs{Permission: GrantManage, Scope: Installation}
	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"POST", "/api/v1/namespaces", admin, s.create},
		{"GET", "/api/v1/namespaces", OnNamespace{}, s.list},
		{"GET", "/api/v1/namespaces/{namespace}", OnNamespace{}, s.one},
		{"DELETE", "/api/v1/namespaces/{namespace}", admin, s.remove},
		{"GET", "/api/v1/namespaces/{namespace}/quotas", OnNamespace{}, s.quotas},
		{"PUT", "/api/v1/namespaces/{namespace}/quotas", admin, s.setQuotas},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// The refusals a namespace's creation and quotas meet in the transaction that writes them.
var (
	errNamespaceExists = errors.New("api: that namespace exists already")
	errNoOwner         = errors.New("api: the owner names no user or group")
)

// noSuchPools is a quota naming runner pools the installation does not have.
type noSuchPools []string

func (p noSuchPools) Error() string {
	which := "is no runner pool"
	if len(p) > 1 {
		which = "are no runner pools"
	}
	return fmt.Sprintf("allowed_runner_pools names %s, which %s of this installation: a namespace is allowed the pools that exist, which GET /api/v1/runner-pools lists", strings.Join(p, ", "), which)
}

// poolsExist refuses a list of pools naming one the installation does not have, with noSuchPools.
func poolsExist(ctx context.Context, w *db.Wide, names []string) error {
	if len(names) == 0 {
		return nil
	}
	pools, err := w.RunnerPools(ctx)
	if err != nil {
		return err
	}
	var missing noSuchPools
	for _, name := range names {
		if !slices.ContainsFunc(pools, func(p db.RunnerPool) bool { return p.Name == name }) {
			missing = append(missing, name)
		}
	}
	if missing != nil {
		return missing
	}
	return nil
}

// create is POST /api/v1/namespaces: a shared namespace, with its owner and its quotas.
//
// The owner is given the owner role on it in the same transaction, granted by whoever created it,
// so that it can do what an owner does from the moment the namespace exists: share it, and act in
// it. Without that grant an owner would own a namespace in its record alone, and be refused
// everything in it.
func (s *NamespaceAPI) create(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask NamespaceRecord
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := NamespaceName(ask.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ask.Owner == "" {
		fail(w, http.StatusBadRequest, "the request names no owner: a namespace is created with one, a user or group:NAME, who is told when an administrator widens their own access in it")
		return
	}
	quotas := Quotas{}
	if ask.Quotas != nil {
		quotas = *ask.Quotas
	}
	grant := access.Grant{
		ID: ulid.New(), Principal: ask.Owner, Scope: access.Scope{Namespace: ask.Name},
		Role: access.Owner, GrantedBy: string(who),
	}
	if err := grant.Validate(); err != nil {
		fail(w, http.StatusBadRequest, "the owner: "+err.Error())
		return
	}

	var created db.Namespace
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		kind, err := wide.PrincipalKind(ctx, ask.Owner)
		switch {
		case errors.Is(err, db.ErrNoPrincipal):
			return errNoOwner
		case err != nil:
			return err
		case kind != db.KindUser && kind != db.KindGroup:
			return errNoOwner
		}
		if err := poolsExist(ctx, wide, quotas.AllowedRunnerPools); err != nil {
			return err
		}
		made, err := wide.CreateNamespace(ctx, db.Namespace{
			Name: ask.Name, Kind: db.NamespaceShared, Owner: ask.Owner, Quotas: quotas.stored(),
		})
		if err != nil {
			return err
		}
		if !made {
			return errNamespaceExists
		}
		if err := wide.GrantAccess(ctx, grant); err != nil {
			return err
		}
		if created, err = wide.NamespaceNamed(ctx, ask.Name); err != nil {
			return err
		}
		// Recorded with the namespace as it was created and the grant that made its owner one,
		// so that who could act in it from the start stays in the log after the namespace and
		// its grants are gone.
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceCreate, Target: ask.Name, Result: audit.Done,
			Detail: map[string]any{"namespace": recordOf(created), "owner_grant": grant.ID},
		})
	})
	var missing noSuchPools
	switch {
	case errors.Is(err, errNamespaceExists):
		fail(w, http.StatusConflict, ask.Name+" is already a namespace")
		return
	case errors.Is(err, db.ErrNameTaken):
		fail(w, http.StatusConflict, ask.Name+" is a user's login, and logins and namespace names share one name space: a user's personal namespace is named after their login, and a namespace created first would take it from them")
		return
	case errors.Is(err, errNoOwner):
		fail(w, http.StatusUnprocessableEntity, "the owner "+ask.Owner+" names no user or group of this installation")
		return
	case errors.As(err, &missing):
		fail(w, http.StatusUnprocessableEntity, missing.Error())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace could not be created")
		return
	}
	write(w, http.StatusCreated, recordOf(created))
}

// list is GET /api/v1/namespaces: every namespace to an administrator, and to anybody else those
// it holds a grant in, as Sees says, by name.
func (s *NamespaceAPI) list(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	sees := Sees(r)
	var all []db.Namespace
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		var err error
		all, err = wide.Namespaces(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the namespaces could not be read")
		return
	}
	seen := NamespaceList{Namespaces: []NamespaceRecord{}}
	for _, n := range all {
		if sees(n.Name) {
			seen.Namespaces = append(seen.Namespaces, recordOf(n))
		}
	}
	write(w, http.StatusOK, seen)
}

// one is GET /api/v1/namespaces/{namespace}, which the router let through to a caller who sees it.
func (s *NamespaceAPI) one(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	if n, ok := s.read(w, r, over.Namespace); ok {
		write(w, http.StatusOK, recordOf(n))
	}
}

// quotas is GET /api/v1/namespaces/{namespace}/quotas.
func (s *NamespaceAPI) quotas(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	if n, ok := s.read(w, r, over.Namespace); ok {
		write(w, http.StatusOK, quotasOf(n.Quotas))
	}
}

// read answers one namespace, or answers the request with why it could not.
//
// A namespace that is not there is the 404 of one the caller cannot see, which is the router's
// answer too: an administrator sees every namespace, and what is not there is the one thing left
// for the 404 to say. A name no namespace could have is one of those, and is answered so before
// the database is asked about it, since a path can carry bytes PostgreSQL refuses to hold as text.
func (s *NamespaceAPI) read(w http.ResponseWriter, r *http.Request, name string) (db.Namespace, bool) {
	if NamespaceName(name) != nil {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return db.Namespace{}, false
	}
	var n db.Namespace
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		var err error
		n, err = wide.NamespaceNamed(ctx, name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return db.Namespace{}, false
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace could not be read")
		return db.Namespace{}, false
	}
	return n, true
}

// remove is DELETE /api/v1/namespaces/{namespace}.
func (s *NamespaceAPI) remove(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := over.Namespace
	if NamespaceName(name) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	err := RemoveNamespace(r.Context(), s.pool, name, who)
	var holds *db.NamespaceHolds
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	case errors.Is(err, db.ErrPersonalNamespace):
		fail(w, http.StatusConflict, "namespace "+name+" is "+name+"'s personal namespace, which is its user's and is never removed on its own")
		return
	case errors.As(err, &holds):
		fail(w, http.StatusConflict, holds.Held())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace could not be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveNamespace removes the namespace name as who, with its built-in identity, its grants and its
// authentication policy, and records it in the audit log in the same transaction. It is what DELETE
// /api/v1/namespaces/{ns} does, and what agentiik-api namespace remove does on the server, so that the
// two refuse the same namespaces.
//
// A namespace that is not there is db.ErrNoNamespace, a user's personal namespace
// db.ErrPersonalNamespace, and one that holds anything a *db.NamespaceHolds saying what.
func RemoveNamespace(ctx context.Context, pool *db.Pool, name string, who Principal) error {
	return pool.Installation(ctx, db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		if err := w.RemoveNamespace(ctx, name); err != nil {
			return err
		}
		return w.Audit(ctx, audit.Record{Actor: string(who), Action: audit.NamespaceDelete, Target: name, Result: audit.Done})
	})
}

// setQuotas is PUT /api/v1/namespaces/{namespace}/quotas: the namespace's quotas, whole.
//
// "max_concurrent_tasks and max_retention_days always hold a value, 20 and 90 until an administrator
// sets another, and a write that leaves either out keeps the value it has. The other four bound
// nothing until they are set, and a write that leaves one out removes its bound." So the body is the
// whole of what the four bound, and what a Terraform apply sends is what the namespace holds after it.
func (s *NamespaceAPI) setQuotas(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := over.Namespace
	if NamespaceName(name) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	var q Quotas
	if err := readObject(r, &q, smallMaxBytes, "the namespace's quotas"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	var set db.Namespace
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.SetQuotas(ctx, name, q.stored()); err != nil {
			return err
		}
		if err := poolsExist(ctx, wide, q.AllowedRunnerPools); err != nil {
			return err
		}
		var err error
		if set, err = wide.NamespaceNamed(ctx, name); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceUpdate, Target: name, Result: audit.Done,
			Detail: map[string]any{"quotas": quotasOf(set.Quotas)},
		})
	})
	var missing noSuchPools
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	case errors.As(err, &missing):
		fail(w, http.StatusUnprocessableEntity, missing.Error())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace's quotas could not be set")
		return
	}
	write(w, http.StatusOK, quotasOf(set.Quotas))
}
