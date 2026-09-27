package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/access"
)

// What a route needs before its handler runs.
//
// A guard is not something a handler calls. It is what a route is registered with, and the
// router is what asks: a handler that forgot the check is a handler that cannot be registered.

// Guard is what stands in front of one route.
//
// The interface is closed: the only things that implement it are Needs, OnRun, OnArtifact, Across,
// OnNamespace, Public, ForRunner and Own, because its one method is unexported. A further kind of
// guard is therefore a change to this file, which is a change somebody reads, rather than a struct
// somebody writes in a handler package.
type Guard interface {
	guards() guard
}

// guard is what the router actually reads.
type guard struct {
	permission Permission
	scope      Scope
	public     bool
	runner     bool
	own        bool
	run        bool
	artifact   bool
	across     bool
	members    bool
	why        string

	// within is set by the router on a route taking OnRun, Across or OnNamespace whose pattern
	// names {namespace}, which then answers about that namespace and no other.
	within bool

	// reveals is the permission a handler may ask about to decide what its answer holds,
	// and is empty on a route that asks about none.
	reveals Permission

	// also is the permission a handler may ask about to decide whether what a request carries
	// is accepted, and is empty on a route that asks about none.
	also Permission
}

// Needs is a route that requires one permission at one scope.
type Needs struct {
	Permission Permission
	Scope      Scope

	// Reveals is a permission the route does not need and whose holder it answers more:
	// see Revealing.
	Reveals Permission

	// Also is a permission the route needs besides Permission where what a request carries
	// calls for it, which only its handler can tell: see HoldsAlso.
	Also Permission
}

func (n Needs) guards() guard {
	return guard{permission: n.Permission, scope: n.Scope, reveals: n.Reveals, also: n.Also}
}

// OnRun is a route about one run, which requires one permission over the workflow that run is of.
//
// Its path names the run, as the documentation lists every route about one:
// /api/v1/runs/{id}/cancel. "Agentiik sends the push service an identifier and a state", and
// the application a notification opens holds that identifier and nothing else. A permission such
// as workflow:run is held on a single workflow as well as on a whole namespace: "access is granted
// by binding a principal to a role, either on the whole namespace or on a single workflow". So the
// router asks which namespace and workflow the run is of, and asks the authorizer about those.
//
// A path may name the namespace as well, as /api/v1/{ns}/runs/{id} does, the path a Location names
// a run by. The router then answers a run of another namespace as absent before it asks anything,
// so that the path cannot name a namespace the caller holds beside a run of one they do not. It
// may not name a workflow: no route needs one, and every check a path can dodge is one too many.
type OnRun struct {
	Permission Permission

	// Reveals is a permission the route does not need and whose holder it answers more:
	// see Revealing.
	Reveals Permission
}

func (o OnRun) guards() guard {
	return guard{permission: o.Permission, scope: Workflow, run: true, reveals: o.Reveals}
}

// OnArtifact is a route about one artifact, named by its logical URI, which requires one permission
// over the workflow of the run that URI names.
//
// It is OnRun with the run read out of agk://run/<run>/<step>/<port>/<name> rather than out of a
// segment of its own, because GET /api/v1/artifacts/{uri} names an artifact as every envelope does
// and nothing else. The URI is one segment of the path, percent-encoded, and one that does not parse
// names no run, so it is refused exactly as a run that is not there is.
type OnArtifact struct {
	Permission Permission
}

func (o OnArtifact) guards() guard {
	return guard{permission: o.Permission, scope: Workflow, run: true, artifact: true}
}

// Across is a route answering, across the installation or one namespace, what its caller holds one
// permission over: GET /api/v1/runs, "across every namespace the caller can read".
//
// Its path names no workflow, so there is no one target to authorise before the handler runs, and
// what the caller may see is a question asked of each thing the answer could hold. The router asks
// it rather than the handler: a route taking Across is registered with HandleAcross, and its handler
// is given Holds, which asks the authorizer about this permission for this principal and nothing
// else, so a handler cannot ask about another permission or another caller. That it answers only
// what Holds let through is the handler's to keep, and its tests' to hold it to: nothing here can
// see what it answers.
//
// A path may name a namespace, as GET /api/v1/{ns}/runs does, and the route then answers across that
// namespace alone, with a Holds that answers about nothing outside it. It is still asked of each
// workflow rather than authorised over the namespace: a permission held on a single workflow is held
// there too, and "a deny wins at any scope" only where the workflow denied is in the question.
type Across struct {
	Permission Permission
}

func (a Across) guards() guard {
	return guard{permission: a.Permission, scope: Workflow, across: true}
}

// Holds answers whether the caller of a route taking Across holds its permission over one target.
// A target naming no namespace is the installation, which no such route answers about, and one
// outside the namespace the route's path names is one it was not asked about: each is an error
// rather than a refusal.
type Holds func(ctx context.Context, over Target) (bool, error)

// Revealing answers, for the route serving r, whether its caller holds the permission its guard
// names as Reveals over one target in the namespace the route was authorised in.
//
// Some routes answer one caller more than another without refusing either: GET /api/v1/runs/{id}
// is "run state, per-step state, envelope digests" to whoever holds run:read, and the inputs the
// run was started with are "envelope contents", which run:read_data alone sees. Which it answers is
// the handler's to decide, since only the handler knows which part of its answer is which, but the
// question is still the router's to ask: the handler is given this, which asks about the one
// permission its guard declared, for the caller the router identified, and about nothing outside
// the namespace it authorised. A route declaring no such permission, or a request the router did
// not serve, is answered false, so a handler that asks where nothing was declared withholds rather
// than reveals.
//
// A workflow's data is asked about over that workflow rather than over its namespace, even where
// the route was authorised over the namespace alone: "a deny wins at any scope", and a deny on one
// workflow is only in the answer where the workflow is in the question.
func Revealing(r *http.Request) Holds {
	if held, ok := r.Context().Value(revealingKey{}).(Holds); ok {
		return held
	}
	return func(context.Context, Target) (bool, error) { return false, nil }
}

// revealingKey is where the router leaves the question a route declared for its handler.
type revealingKey struct{}

// HoldsAlso answers, for the route serving r, whether its caller holds the permission its guard
// names as Also over the target the route was authorised against.
//
// Some routes need a second permission for some requests and not for others, and only the handler
// knows which request is which: a push needs workflow:write, and secret:use as well where the
// version it carries names a secret, since "secret:use is checked when a version is pushed, against
// whoever pushes it". The handler decides whether to ask, and the router is still what asks: about
// the one permission the guard declared, for the caller it identified, over the target it
// authorised, and nothing else. A route declaring none, or a request the router did not serve, is
// answered false, so a handler that asks where nothing was declared refuses rather than accepts.
func HoldsAlso(r *http.Request) func(context.Context) (bool, error) {
	if held, ok := r.Context().Value(alsoKey{}).(func(context.Context) (bool, error)); ok {
		return held
	}
	return func(context.Context) (bool, error) { return false, nil }
}

// alsoKey is where the router leaves the question HoldsAlso asks.
type alsoKey struct{}

// FindRun says which namespace and workflow a run is of, which is what a route taking OnRun is
// authorised against. A run nobody minted is ErrNoRun.
//
// It is asked before anything is authorised, across the installation since a path naming a run
// names no namespace or names one to be checked against the answer, and its answer goes to the
// router and the authorizer and nowhere else: a run that is not there and a
// run the caller may not reach are the same 404, so asking tells a caller nothing the refusal
// would not.
type FindRun interface {
	RunOf(ctx context.Context, run string) (Target, error)
}

// ErrNoRun is no run of that identifier.
var ErrNoRun = errors.New("api: no run of that identifier")

// OnNamespace is a route about namespaces' records, which an administrator reads of every namespace
// and anybody else of the namespaces it holds a grant in: "to an administrator and to a principal
// holding a grant in it; anyone else is answered the 404 of one that does not exist."
//
// It needs no permission, because reading a namespace's record, its kind, its owner and its quotas,
// is none of the nine: whoever holds a role on one workflow of a namespace reads its record as its
// owner does. So the router asks two things instead: whether the caller administers the
// installation, which the Authorizer answers as grant:manage there, through a credential that
// carries the power; and otherwise which namespaces it holds a grant in, which the Authorizer
// answers where it implements Holdings, as Principals does. A route taking OnNamespace on a router
// whose authorizer does not is refused at registration, since it could answer nobody.
//
// Where its pattern names a {namespace}, a namespace the caller does not see is refused before the
// handler runs, as the absence it is to them. Where it names none, as the listing of namespaces
// does, its handler is given Sees and answers only what it lets through.
type OnNamespace struct{}

func (OnNamespace) guards() guard { return guard{scope: Namespace, members: true} }

// Holdings says which namespaces a principal holds a grant in, its own or one of its groups', on
// the namespace or on a workflow of it: what a route taking OnNamespace answers a caller who does
// not administer the installation. An Authorizer implements it where it can say.
type Holdings interface {
	HeldIn(ctx context.Context, who Principal) ([]string, error)
}

// Sees answers, for the route serving r, whether its caller sees one namespace's record: every
// namespace for an administrator, through a credential that carries the power, and otherwise one it
// holds a grant in and its credential reaches. A route not taking OnNamespace, and a request the
// router did not serve, see nothing.
func Sees(r *http.Request) func(namespace string) bool {
	if sees, ok := r.Context().Value(seesKey{}).(func(string) bool); ok {
		return sees
	}
	return seesNothing
}

func seesNothing(string) bool { return false }

// seesKey is where the router leaves what Sees answers.
type seesKey struct{}

// Public is a route that is not authorised by a principal, and says what authorises it instead.
//
// There are four of these in the whole design and each has its own answer: registration is
// authenticated "by the join token in its body and by nothing else", an object route is
// authenticated by the signature in its own URL or in the form posted to it, a webhook is
// authenticated "per trigger", and a health check answers nothing worth having. Why is required
// and is checked for being a sentence rather than a shrug, because "public" with no reason beside
// it is how a route that should have been guarded stops being guarded.
type Public struct {
	Why string
}

func (p Public) guards() guard {
	return guard{public: true, why: p.Why}
}

// check refuses a guard that says nothing.
func (g guard) check(method, pattern string) error {
	if g.runner || g.members {
		return nil
	}
	if g.public {
		if len(g.why) < 20 {
			return fmt.Errorf("api: %s %s is public and says %q: a route outside the authorisation hook says what authorises it instead, at length, because that sentence is what a reviewer reads", method, pattern, g.why)
		}
		return nil
	}
	if !g.permission.Valid() {
		return fmt.Errorf("api: %s %s needs %q, which is not one of the permissions the documentation names", method, pattern, g.permission)
	}
	if g.reveals != "" && !g.reveals.Valid() {
		return fmt.Errorf("api: %s %s reveals more to %q, which is not one of the permissions the documentation names", method, pattern, g.reveals)
	}
	if g.also != "" && !g.also.Valid() {
		return fmt.Errorf("api: %s %s needs %q as well for some requests, which is not one of the permissions the documentation names", method, pattern, g.also)
	}
	return nil
}

// ForRunner is a route authorised by a runner credential rather than by a principal.
//
// A runner is not a principal and holds none of the permissions: "It holds no database
// credential, no secret-store credential and no standing object-store credential. Only a runner
// credential and per-task grants that expire." What it may do is a short closed list, and the
// routes that serve it are the only ones that take this.
//
// It is a third kind of guard, and adding one is deliberately a change to this file that somebody
// reads. The alternative was to mark these routes public and check the credential inside each
// handler, which is the shape of every access check that has ever been forgotten.
type ForRunner struct{}

func (ForRunner) guards() guard { return guard{runner: true} }

// Own is a route about the caller's own credentials, and those of the service accounts of the
// namespaces it owns: "an API token for the caller or a service account of a namespace it owns",
// the listing of "the caller's tokens and those of the service accounts of namespaces it owns",
// and the revocation of one of them.
//
// Any principal reaches it and it needs no permission, since what it answers is the caller's own,
// and holding a credential or owning a namespace is none of the nine. It is registered with
// HandleOwn, whose handler is given a Caller in place of a target: who asks, how the credential it
// presented narrows it, and what it owns through that credential, which the router asks the
// authorizer rather than leaving to the handler, as it asks everything else.
type Own struct{}

func (Own) guards() guard { return guard{own: true} }

// Owners says which namespaces a principal owns: those where it holds the owner role, by a grant of
// its own or of one of its groups, on the namespace rather than on one of its workflows. An
// Authorizer implements it where it can say, as Principals does, and a route taking Own is refused
// registration on a router whose authorizer does not, since it could answer nobody.
type Owners interface {
	Owned(ctx context.Context, who Principal) ([]string, error)
}

// Caller is who a route taking Own serves, as the router identified them from the credential they
// presented.
type Caller struct {
	// Principal is who asks.
	Principal Principal

	// Token is the identifier of the API token the request presented, and empty for any other
	// credential, the bootstrap token among them.
	Token string

	scope  access.TokenScope
	owners Owners
}

// Narrowed says whether the credential the caller presented carries a scope. A narrowed token
// reaches no other credential: it mints none, since "a scope can only narrow" and a token it minted
// would not be narrowed by it, and it lists and revokes itself alone, since managing credentials is
// none of the nine and "a permissions list keeps only the permissions it names", as it keeps no
// administrator's powers.
func (c Caller) Narrowed() bool { return c.scope.Narrows() }

// Owned answers the namespaces the caller owns through the credential it presented, ordered by
// name: none through a narrowed one, for the reason Narrowed gives.
func (c Caller) Owned(ctx context.Context) ([]string, error) {
	if c.Narrowed() || c.owners == nil {
		return nil, nil
	}
	return c.owners.Owned(ctx, c.Principal)
}

// Principal is who is asking, written as a grant, the API, agk and the audit log write it: a login,
// NS/NAME for a service account, and operator for the bootstrap token, as the v0.2 operator was
// written on every row it left. The empty one is nobody: an unauthenticated caller gets "Deny by
// default at the API".
//
// It is the string alone, which is what a handler records as who did something, and what the
// Authorizer is asked about. What a principal holds is its grants' to say, read when it is asked,
// and never something a request carries.
type Principal string

// Identity is who a request is from, as the credential it presented says.
//
// Beside the principal it carries what that credential narrows the principal to, which is a token's
// scope and nothing for any other credential, because "on every request its rights are the
// principal's intersected with both": the router intersects every answer it asks the Authorizer
// for with it, so that no route, and no handler asking what it may reveal, can reach past it.
//
// For nobody, it may carry the sentence a refusal says instead of that no credential came, for a
// credential that came and opens nothing: what a caller can do about it, and nothing about which
// credentials exist. That refusal is a 401, or the status RefusedAs says for a credential that came
// and is not one this request may carry: 403 for a session changing something from another origin,
// or one that may only enrol anywhere a principal is asked about, and 400 for a request carrying
// more than one.
//
// Enrolling is set for a session an enrolment code opened, which "enrols passkeys and nothing
// else": the router refuses it on every route it authorises by who asks, with the 403 the OpenAPI
// document names.
type Identity struct {
	Principal Principal
	Scope     access.TokenScope
	Enrolling bool
	RefusedAs int
	Refused   string

	// Token is the identifier of the API token presented, which a caller revokes and lists
	// itself by, and empty for any other credential.
	Token string
}

// Target is what is being asked about, resolved from the request before anything is authorised.
//
// It is what a namespaced permission is checked against, and it is filled by the router from the
// path rather than by a handler, because a handler that read its own namespace out of the path
// would be a handler that could read a different one. On a route taking OnRun both are the ones the
// run in the path is of, which the router looked up.
type Target struct {
	Namespace string
	Workflow  string
}

// Authorizer answers whether one principal holds one permission over one target.
//
// Grants, roles, groups and the union recomputed per request all live behind it, and Principals is
// the installation's. The question is asked by the router rather than anywhere else, about the
// principal alone: what its credential narrows it to is the router's to intersect, since only the
// router saw the credential. A target naming no namespace is the installation, which is what a
// route taking Needs at Installation scope is asked about whatever its path names.
//
// An error is not a refusal. A refusal is (false, nil) and means the caller may not; an error
// means the question could not be answered, which is a 500 and not a 404, because telling a
// caller they may not have something on the strength of a database being down is telling them
// something untrue.
type Authorizer interface {
	Allow(ctx context.Context, who Principal, what Permission, over Target) (bool, error)
}

// IdentifyRunner says which runner a credential belongs to, and refuses one revoked past its grace
// and one past its rotate_by.
//
// It is the runner half of Identify, separate because the two answer different questions: one
// asks who a person is and the other asks which machine this is. A runner that cannot be
// identified is not an unauthenticated principal, it is a machine that has to join again.
type IdentifyRunner interface {
	Runner(ctx context.Context, credential string) (Runner, error)
}

// Runner is the machine a credential belongs to, reduced to what a route needs.
type Runner struct {
	ID    string
	Pool  string
	State string

	// RotateBy is when the credential the request carried stops being accepted, which is as
	// long as anything minted on the strength of it may last.
	RotateBy time.Time

	// ResultsAcceptedUntil is the end of a revoked runner's grace, which it is opened until,
	// and zero for a runner nobody revoked.
	ResultsAcceptedUntil time.Time
}

// ErrNoRunner is a credential that opens no runner, one revoked past its grace, or one past its
// rotate_by. One error for all of them, because telling a caller which it was tells somebody
// guessing whether they had a real one.
var ErrNoRunner = errors.New("api: no runner of that credential")

// DenyAll refuses everything, and is what an installation with no access model has.
//
// It is the default rather than a thing to remember to replace. "Deny by default" with nothing
// to grant yet is not a placeholder standing in for a decision: it is the decision, and an
// installation that shipped with an authorizer nobody configured should refuse rather than
// admit.
type DenyAll struct{}

// Allow refuses.
func (DenyAll) Allow(context.Context, Principal, Permission, Target) (bool, error) {
	return false, nil
}

// ErrNoAuthorizer is a router built without one, which is refused at construction rather than
// discovered at the first request.
var ErrNoAuthorizer = errors.New("api: no authorizer, and every request is authorised at the API boundary")
