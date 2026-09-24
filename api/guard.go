package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// What a route needs before its handler runs.
//
// A guard is not something a handler calls. It is what a route is registered with, and the
// router is what asks: a handler that forgot the check is a handler that cannot be registered.

// Guard is what stands in front of one route.
//
// The interface is closed: the only things that implement it are Needs, OnRun, Public and
// ForRunner, because its one method is unexported. A further kind of guard is therefore a change
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
	why        string
}

// Needs is a route that requires one permission at one scope.
type Needs struct {
	Permission Permission
	Scope      Scope
}

func (n Needs) guards() guard {
	return guard{permission: n.Permission, scope: n.Scope}
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
}

func (o OnRun) guards() guard {
	return guard{permission: o.Permission, scope: Workflow, run: true}
}

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

// IdentifyRunner says which runner a credential belongs to, and refuses a revoked one and one past
// its rotate_by.
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
}

// ErrNoRunner is a credential that opens no runner, one that was revoked, or one past its
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
