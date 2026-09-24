package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// What a route needs before its handler runs.
//
// A guard is not something a handler calls. It is what a route is registered with, and the
// router is what asks: a handler that forgot the check is a handler that cannot be registered.

// Guard is what stands in front of one route.
//
// The interface is closed: the only things that implement it are Needs, OnRun, OnArtifact, Across,
// Public and ForRunner, because its one method is unexported. A further kind of guard is therefore a change
// to this file, which is a change somebody reads, rather than a struct somebody writes in a
// handler package.
type Guard interface {
	guards() guard
}

// guard is what the router actually reads.
type guard struct {
	permission Permission
	scope      Scope
	public     bool
	runner     bool
	run        bool
	artifact   bool
	across     bool
	why        string

	// reveals is the permission a handler may ask about to decide what its answer holds,
	// and is empty on a route that asks about none.
	reveals Permission
}

// Needs is a route that requires one permission at one scope.
type Needs struct {
	Permission Permission
	Scope      Scope

	// Reveals is a permission the route does not need and whose holder it answers more:
	// see Revealing.
	Reveals Permission
}

func (n Needs) guards() guard {
	return guard{permission: n.Permission, scope: n.Scope, reveals: n.Reveals}
}

// OnRun is a route about one run, which requires one permission over the workflow that run is of.
//
// Its path names the run and nothing the run is of, as the documentation lists every route about
// one: /api/v1/runs/{id}/cancel. "Agentiik sends the push service an identifier and a state", and
// the application a notification opens holds that identifier and nothing else. A permission such
// as workflow:run is held on a single workflow as well as on a whole namespace: "access is granted
// by binding a principal to a role, either on the whole namespace or on a single workflow". So the
// router asks which namespace and workflow the run is of, and asks the authorizer about those. A
// path naming either as well could name one the caller holds beside a run of another, which is
// why it may not.
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

// Across is a route answering, across the installation, what its caller holds one permission over:
// GET /api/v1/runs, "across every namespace the caller can read".
//
// Its path names no namespace, so there is no one target to authorise before the handler runs, and
// what the caller may see is a question asked of each thing the answer could hold. The router asks
// it rather than the handler: a route taking Across is registered with HandleAcross, and its handler
// is given Holds, which asks the authorizer about this permission for this principal and nothing
// else, so a handler cannot ask about another permission or another caller. That it answers only
// what Holds let through is the handler's to keep, and its tests' to hold it to: nothing here can
// see what it answers.
type Across struct {
	Permission Permission
}

func (a Across) guards() guard {
	return guard{permission: a.Permission, scope: Workflow, across: true}
}

// Holds answers whether the caller of a route taking Across holds its permission over one target.
// A target naming no namespace is the installation, which no such route answers about, and is an
// error rather than a refusal.
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

// FindRun says which namespace and workflow a run is of, which is what a route taking OnRun is
// authorised against. A run nobody minted is ErrNoRun.
//
// It is asked before anything is authorised, across the installation since the path names no
// namespace, and its answer goes to the authorizer and nowhere else: a run that is not there and a
// run the caller may not reach are the same 404, so asking tells a caller nothing the refusal
// would not.
type FindRun interface {
	RunOf(ctx context.Context, run string) (Target, error)
}

// ErrNoRun is no run of that identifier.
var ErrNoRun = errors.New("api: no run of that identifier")

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
	if g.runner {
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

// Principal is who is asking.
//
// It is a string here and a row in v0.3.0. What matters at this milestone is that every route
// has one or is explicitly public, and that the empty one is nobody: an unauthenticated caller
// gets "Deny by default at the API".
type Principal string

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
// This is the seam v0.3.0 fills: grants, roles, groups and the union recomputed per request all
// live behind it. What this milestone fixes is the question, and that the answer is asked once
// per request by the router rather than anywhere else.
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
