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

// The namespaces, as a user or an administrator creates them, their owners rename, picture and
// remove them, an administrator bounds them, and whoever holds a grant in one reads it:
// /api/v1/namespaces.
//
// "Any user creates a shared namespace, and owns it ... An administrator alone names another owner,
// a user or a group, or sets quotas at its creation", so the creation is a route about its caller,
// which asks whether the caller administers the installation for what only an administrator may
// write. "Its owner, whoever holds grant:manage at its scope, which the owner role carries, renames
// it, gives it a picture and removes it once it holds nothing, and so may an administrator; its
// quotas stay an administrator's to change": those routes need grant:manage at the namespace, or the
// administrator's power, and its quotas grant:manage at the installation, answered 403 to anybody
// else before the namespace is looked up. Reading is wider: a namespace's record and its picture are
// "to an administrator and to a principal holding a grant in it; anyone else is answered the 404 of
// one that does not exist", which the router answers through OnNamespace.
//
// Everything is asked and answered in the shapes wire.schema.json gives it, $defs/namespaceRecord
// and $defs/quotas, which a console, the Terraform provider and agk are written against.

// NamespaceName refuses a name no namespace can be created under: one outside $defs/namespace,
// lowercase words joined by hyphens, one longer than a name is, or one of the words the API
// routes on.
//
// It is the check POST /api/v1/namespaces and agentiik-api namespace create both make, so that the
// route and the verb refuse the same names.
func NamespaceName(name string) error { return namespaceName(name, agk.IsReservedNamespace) }

// NamespaceRef refuses a name no namespace can carry, which a route naming one answers as absent
// before the database is asked: NamespaceName's refusals, save a word reserved after namespaces
// could be created under it, since one created before keeps its name and is served as before
// (agk.LateReservations).
func NamespaceRef(name string) error { return namespaceName(name, agk.NamesNoNamespace) }

func namespaceName(name string, reserved func(string) bool) error {
	switch {
	case name == "":
		return errors.New("a namespace has a name, lowercase words joined by hyphens, such as finance or team-ops")
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("a namespace's name is at most %d characters and this one is %d: it is written in every path of the API and every key of the object store, and no filesystem holds a longer name", agk.IdentifierMaxBytes, len(name))
	case !givenName.MatchString(name):
		return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens, such as finance or team-ops", name)
	case reserved(name):
		return fmt.Errorf("%s is %s: the first path segment after /api/v1/ decides the route, so %s cannot name a namespace", name, routesOn(name), strings.Join(agk.ReservedNamespaces, ", "))
	}
	return nil
}

// routesOn says how the API routes on a reserved word, for a refusal to name: now, or from the
// release that serves the route a word reserved late is reserved for, so that nobody is told the
// API routes on stats before it does.
func routesOn(word string) string {
	if r, late := agk.ReservedLate(word); late {
		return fmt.Sprintf("a word the API routes on from %s, for %s", r.Served, r.Route)
	}
	return "a word the API routes on"
}

// NamespaceRecord is a namespace as the wire writes it, $defs/namespaceRecord: its name, whether it
// is somebody's personal namespace or a shared one, its owner where it has one, its quotas, the names
// it held before a rename and when its picture was set.
type NamespaceRecord struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind,omitempty"`
	Owner  string  `json:"owner,omitempty"`
	Quotas *Quotas `json:"quotas,omitempty"`

	// FormerNames are the names it held before a rename, in the order it left them, each still
	// reaching it, and empty where it was never renamed.
	FormerNames []string `json:"former_names"`

	// AvatarUpdatedAt is when its picture was set, and null where it has none: a client adds it to
	// the picture's address, so that a picture set again is never taken from a cache.
	AvatarUpdatedAt *time.Time `json:"avatar_updated_at"`
}

// NamespaceCreate is what POST /api/v1/namespaces reads, the openapi's namespaceCreate: a name, and
// an owner and quotas where an administrator writes them. Kind is shared, and shared where it is
// left out, since "every user owns a personal namespace named after their login, created on first
// sign-in", and never here.
type NamespaceCreate struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind,omitempty"`
	Owner  string  `json:"owner,omitempty"`
	Quotas *Quotas `json:"quotas,omitempty"`
}

func (n *NamespaceCreate) field(b *body, name string) error {
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
// writes max_concurrent_tasks and max_retention_days, which "always hold a value, 20 and 90 until
// an administrator sets another", and the other four where they are set.
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
// nothing otherwise. The wire allows null for none of them, and read as left out, a null quota
// would lift a bound, or keep one, from a value somebody meant to set, which is what a client sends
// for a variable it left unset.
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

// recordOf is a namespace as it is answered: its former names always written, empty where it was
// never renamed, and when its picture was set always written, null where it has none, so that a
// client reads one spelling of nothing.
func recordOf(n db.Namespace) NamespaceRecord {
	q := quotasOf(n.Quotas)
	record := NamespaceRecord{Name: n.Name, Kind: n.Kind, Owner: n.Owner, Quotas: &q, FormerNames: []string{}}
	record.FormerNames = append(record.FormerNames, n.FormerNames...)
	if !n.AvatarUpdatedAt.IsZero() {
		at := n.AvatarUpdatedAt.UTC()
		record.AvatarUpdatedAt = &at
	}
	return record
}

// NamespaceUpdate is what PATCH /api/v1/namespaces/{ns} changes, the openapi's namespaceUpdate: the
// namespace's settings as a partial object, each field it names set and each it leaves out kept.
// Name alone today; "the route takes more fields as the settings grow", each a pointer here so that
// a field left out is told from one written, and null is refused rather than read as either.
type NamespaceUpdate struct {
	Name *string `json:"name,omitempty"`
}

func (u *NamespaceUpdate) field(b *body, name string) error {
	switch name {
	case "name":
		if err := notNullHere(b, "the namespace's new name"); err != nil {
			return err
		}
		var to string
		if err := text(b, &to); err != nil {
			return err
		}
		u.Name = &to
		return nil
	}
	return unknown(name)
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

	// Now is the clock a statistics range defaults to, an argument so that a test has one.
	Now func() time.Time
}

// NamespaceAPI serves /api/v1/namespaces, and a namespace's load against its quotas.
type NamespaceAPI struct {
	pool *db.Pool
	now  func() time.Time
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
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &NamespaceAPI{pool: o.Pool, now: o.Now}
	// A namespace renamed answers to its former names on every route naming it, these among them.
	rt.ServeNamespaces(o.Pool)

	// Any user creates a namespace, owned by them, and an administrator one for another owner: a
	// route about its caller, which asks whether the caller administers what only an administrator
	// may write.
	if err := rt.HandleOwn("POST", "/api/v1/namespaces", Own{}, s.create); err != nil {
		return nil, err
	}
	admin := Needs{Permission: GrantManage, Scope: Installation}
	// The namespace's owner, whoever holds grant:manage at its scope, "which the owner role
	// carries", or an administrator, by the installation's power over every namespace.
	owner := Needs{Permission: GrantManage, Scope: Namespace, OrAdministrator: true}
	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"GET", "/api/v1/namespaces", OnNamespace{}, s.list},
		{"GET", "/api/v1/namespaces/{namespace}", OnNamespace{}, s.one},
		{"PATCH", "/api/v1/namespaces/{namespace}", owner, s.update},
		{"DELETE", "/api/v1/namespaces/{namespace}", owner, s.remove},
		{"GET", "/api/v1/namespaces/{namespace}/avatar", OnNamespace{}, s.avatar},
		{"PUT", "/api/v1/namespaces/{namespace}/avatar", owner, s.setAvatar},
		{"DELETE", "/api/v1/namespaces/{namespace}/avatar", owner, s.removeAvatar},
		{"GET", "/api/v1/namespaces/{namespace}/quotas", OnNamespace{}, s.quotas},
		{"PUT", "/api/v1/namespaces/{namespace}/quotas", admin, s.setQuotas},
		// A namespace's load against its quotas, to whoever reads the quotas, "since a quota
		// bounds every workflow of it alike".
		{"GET", "/api/v1/{namespace}/stats/quotas", OnNamespace{}, s.statistics},
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

// The refusals of a caller who may not create the namespace it asks for, each a 403.
const (
	accountCreatesNoNamespace  = "a service account creates no namespace: a namespace is a person's, who owns it and answers for what is shared in it, and an administrator creates one for a team"
	narrowedCreatesNoNamespace = "a token narrowed by a scope creates no namespace, since a scope keeps only the permissions it names and creating a namespace is none of them: use a credential that carries no scope"
	ownerIsAnAdministrators    = "a namespace you create is owned by you: naming another owner, a user or a group, is an administrator's, since it hands somebody a namespace they did not ask for"
	quotasAreAnAdministrators  = "quotas are an administrator's to set: a namespace you create takes the installation's defaults, and an administrator changes them at PUT /api/v1/namespaces/{ns}/quotas"
)

// create is POST /api/v1/namespaces: a shared namespace, owned by whoever creates it, or by the
// owner an administrator names, with the quotas an administrator sets.
//
// "Any user creates a shared namespace, and owns it ... An administrator alone names another owner,
// a user or a group, or sets quotas at its creation, and a namespace a user creates takes the
// installation's defaults." A service account and a token narrowed by a scope create none, since a
// namespace is a person's; the bootstrap token, which administers and is nobody, names its owner.
//
// The owner is given the owner role on it in the same transaction, granted by whoever created it,
// so that it can do what an owner does from the moment the namespace exists: share it, and act in
// it. Without that grant an owner would own a namespace in its record alone, and be refused
// everything in it.
func (s *NamespaceAPI) create(w http.ResponseWriter, r *http.Request, caller Caller) {
	var ask NamespaceCreate
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := NamespaceName(ask.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	who := caller.Principal
	admin, err := caller.Administers(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "the request could not be authorised")
		return
	}
	switch {
	case admin && ask.Owner == "" && who == BootstrapOperator:
		fail(w, http.StatusBadRequest, "the request names no owner, and the bootstrap token is nobody: a namespace it creates names its owner, a user or group:NAME, who is told when an administrator widens their own access in it")
		return
	case admin:
	case who == BootstrapOperator, strings.Contains(string(who), "/"):
		fail(w, http.StatusForbidden, accountCreatesNoNamespace)
		return
	case caller.Narrowed():
		fail(w, http.StatusForbidden, narrowedCreatesNoNamespace)
		return
	case ask.Owner != "" && ask.Owner != string(who):
		fail(w, http.StatusForbidden, ownerIsAnAdministrators)
		return
	case ask.Quotas != nil:
		fail(w, http.StatusForbidden, quotasAreAnAdministrators)
		return
	}
	if ask.Owner == "" {
		ask.Owner = string(who)
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
	err = s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
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
		// The owner's grant is a grant written like any other, and recorded as one, in the
		// namespace it was done in, so that whoever reads the log for who was given what there
		// finds it; the namespace's entry names it too, so that who could act in it from the start
		// reads off one entry.
		if err := wide.AuditIn(ctx, ask.Name, audit.Record{
			Actor: string(who), Action: audit.GrantCreate, Target: grant.ID, Result: audit.Done,
			Detail: map[string]any{"principal": grant.Principal, "scope": grant.Scope.String(), "role": string(grant.Role)},
		}); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceCreate, Target: ask.Name, Result: audit.Done,
			Detail: map[string]any{"namespace": recordOf(created), "owner_grant": grant.ID},
		})
	})
	var missing noSuchPools
	var held *db.NameHeld
	switch {
	case errors.Is(err, errNamespaceExists):
		fail(w, http.StatusConflict, ask.Name+" is already a namespace")
		return
	case errors.As(err, &held) && held.Former:
		fail(w, http.StatusConflict, heldRefusal(held))
		return
	case errors.Is(err, db.ErrNameTaken):
		fail(w, http.StatusConflict, ask.Name+" is a user's login, and logins and namespace names share one name space: a user's personal namespace is named after their login, and a namespace created first would take it from them")
		return
	case errors.Is(err, errNoOwner), errors.Is(err, db.ErrNoPrincipal):
		// The second is an owner removed between its check and the namespace's insert, which
		// the insert's reference to it refuses: the same absence, found later.
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
	if NamespaceRef(name) != nil {
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
	if NamespaceRef(name) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	err := RemoveNamespace(r.Context(), s.pool, name, who)
	var holds *db.NamespaceHolds
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	case errors.Is(err, ErrPersonalNamespace):
		fail(w, http.StatusConflict, PersonalRefusal(name))
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

// ErrPersonalNamespace is a user's personal namespace, which RemoveNamespace refuses: it is named
// after its user's login, and "every user owns a personal namespace named after their login ...
// which they cannot delete or rename". Removing the user is what removes it.
var ErrPersonalNamespace = errors.New("api: that namespace is a user's personal namespace")

// PersonalRefusal is the refusal of a personal namespace's removal, in the words the route and the
// server verb both say it in.
func PersonalRefusal(name string) string {
	return "namespace " + name + " is the personal namespace of the user " + name + ", named after their login, and is removed with its user rather than as a namespace"
}

// RemoveNamespace removes the namespace name as who, with its built-in identity, its grants and its
// authentication policy, and records it in the audit log in the same transaction. It is what DELETE
// /api/v1/namespaces/{ns} does, and what agentiik-api namespace remove does on the server, so that
// the two refuse the same namespaces.
//
// A namespace that is not there is db.ErrNoNamespace, a user's personal namespace
// ErrPersonalNamespace, and one that holds anything a *db.NamespaceHolds saying what. The kind is
// read before the row is locked, which is sound because a namespace's kind never changes.
func RemoveNamespace(ctx context.Context, pool *db.Pool, name string, who Principal) error {
	return pool.Installation(ctx, db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		n, err := w.NamespaceNamed(ctx, name)
		if err != nil {
			return err
		}
		if n.Kind == db.NamespacePersonal {
			return fmt.Errorf("%w: %s", ErrPersonalNamespace, name)
		}
		if err := w.RemoveNamespace(ctx, name); err != nil {
			return err
		}
		return w.Audit(ctx, audit.Record{Actor: string(who), Action: audit.NamespaceDelete, Target: name, Result: audit.Done})
	})
}

// setQuotas is PUT /api/v1/namespaces/{namespace}/quotas: the namespace's quotas, whole.
//
// "max_concurrent_tasks and max_retention_days always hold a value, 20 and 90 until an
// administrator sets another, and a write that leaves either out keeps the value it has. The other
// four bound nothing until they are set, and a write that leaves one out removes its bound." So the
// body is the whole of what the four bound, and what a Terraform apply sends is what the namespace
// holds after it.
func (s *NamespaceAPI) setQuotas(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := over.Namespace
	if NamespaceRef(name) != nil {
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

// heldRefusal is the refusal of a name somebody holds, in the words the creation and the rename of
// a namespace both say it in.
func heldRefusal(held *db.NameHeld) string {
	switch {
	case held.Login:
		return held.Name + " is a user's login, and logins and namespace names share one name space: a user's personal namespace is named after their login, and a namespace given it first would take it from them"
	case held.Former && held.Namespace != "":
		return held.Name + " is a name namespace " + held.Namespace + " held before it was renamed, and a name a namespace held stays its own until that namespace is removed, so that an address written with it reaches nobody else"
	case held.Former:
		return held.Name + " is a name another namespace held before it was renamed, and a name a namespace held stays its own until that namespace is removed, so that an address written with it reaches nobody else"
	}
	return held.Name + " is already a namespace"
}

// update is PATCH /api/v1/namespaces/{namespace}: the namespace's settings, each the body names set
// and each it leaves out kept. name is the one it takes today, which renames a shared namespace.
//
// "The rename is one transaction and carries everything at once", made by db.Wide.RenameNamespace
// beside its audit entry, and answered with the namespace's record under its new name. A name off
// the grammar is the body's fault, 400; a name held, by a login, a namespace, a word the API routes
// on or another namespace's former name, and a namespace that cannot be renamed now, are 409, each
// saying which.
func (s *NamespaceAPI) update(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := over.Namespace
	if NamespaceRef(name) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	var ask NamespaceUpdate
	if err := readObject(r, &ask, smallMaxBytes, "the namespace's settings"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if ask == (NamespaceUpdate{}) {
		fail(w, http.StatusBadRequest, "the request names nothing to change: name, the one setting a namespace takes today")
		return
	}
	to := *ask.Name
	if to != name {
		if agk.IsReservedNamespace(to) {
			fail(w, http.StatusConflict, fmt.Sprintf("%s is %s: the first path segment after /api/v1/ decides the route, so no namespace is renamed to it", to, routesOn(to)))
			return
		}
		if err := NamespaceName(to); err != nil {
			fail(w, http.StatusBadRequest, "name: "+err.Error())
			return
		}
	}
	var renamed db.Namespace
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		changed, err := wide.RenameNamespace(ctx, name, to)
		if err != nil {
			return err
		}
		if renamed, err = wide.NamespaceNamed(ctx, to); err != nil {
			return err
		}
		result := audit.Done
		if !changed {
			result = audit.Unchanged
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceRename, Target: to, Result: result,
			Detail: map[string]any{"from": name, "to": to},
		})
	})
	var held *db.NameHeld
	var waits *db.RenameWaits
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	case errors.Is(err, db.ErrPersonalRename):
		fail(w, http.StatusConflict, "namespace "+name+" is the personal namespace of the user "+name+", named after their login, which never changes, and is never renamed")
		return
	case errors.As(err, &held):
		fail(w, http.StatusConflict, heldRefusal(held))
		return
	case errors.As(err, &waits):
		fail(w, http.StatusConflict, waits.Held())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace could not be renamed")
		return
	}
	write(w, http.StatusOK, recordOf(renamed))
}

// The absence a namespace's picture is answered with, a 404 whether the namespace or its picture is
// not there, since a namespace the caller cannot see is refused before it is looked up and one it
// can see is all it learns of.
const noNamespaceAvatar = "no such namespace, or it has no picture"

// avatar is GET /api/v1/namespaces/{namespace}/avatar: the namespace's picture, to whoever reads its
// record, which the router let through. Served as a user's photo is, with avatar_updated_at as its
// tag.
func (s *NamespaceAPI) avatar(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	if NamespaceRef(over.Namespace) != nil {
		fail(w, http.StatusNotFound, noNamespaceAvatar)
		return
	}
	var picture []byte
	var at time.Time
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		var err error
		picture, at, err = wide.NamespaceAvatar(ctx, over.Namespace)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace), errors.Is(err, db.ErrNoNamespaceAvatar):
		fail(w, http.StatusNotFound, noNamespaceAvatar)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the picture could not be read")
		return
	}
	servePicture(w, r, picture, at)
}

// setAvatar is PUT /api/v1/namespaces/{namespace}/avatar: the namespace's picture, "held to a user's
// photo's rules", read as pictureSent reads one and stored as reencode makes it in place of any
// before it, and recorded as namespace.avatar with the size it was stored at.
func (s *NamespaceAPI) setAvatar(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	if NamespaceRef(over.Namespace) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	stored, width, height, ok := pictureSent(w, r, "picture")
	if !ok {
		return
	}
	// To the microsecond the database keeps, so that the instant answered is the one stored.
	at := s.now().Truncate(time.Microsecond)
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.SetNamespaceAvatar(ctx, over.Namespace, stored, at); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceAvatar, Target: over.Namespace, Result: audit.Done,
			Detail: map[string]any{"removed": false, "width": width, "height": height},
		})
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
	case err != nil:
		fail(w, http.StatusInternalServerError, "the picture could not be stored")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// removeAvatar is DELETE /api/v1/namespaces/{namespace}/avatar: the namespace's picture removed,
// which the console draws as its initial from then on. Removing none is the same answer, and
// recorded as unchanged.
func (s *NamespaceAPI) removeAvatar(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if NamespaceRef(over.Namespace) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	err := s.pool.Installation(r.Context(), db.NamespaceAdministration, func(ctx context.Context, wide *db.Wide) error {
		had, err := wide.RemoveNamespaceAvatar(ctx, over.Namespace)
		if err != nil {
			return err
		}
		result := audit.Done
		if !had {
			result = audit.Unchanged
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.NamespaceAvatar, Target: over.Namespace, Result: result,
			Detail: map[string]any{"removed": true},
		})
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
	case err != nil:
		fail(w, http.StatusInternalServerError, "the picture could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// formerNames are the names the namespace held before it was renamed, which a version pushed to it
// may still write as its metadata.namespace.
func formerNames(ctx context.Context, pool *db.Pool, namespace string) ([]string, error) {
	var former []string
	err := pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		former, err = ns.FormerNames(ctx)
		return err
	})
	return former, err
}
