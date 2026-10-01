package accesstest

import (
	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// Case is one route of the API: how the documentation's table of routes guards it, written as the
// router's table writes a route, and what the fixture needs to ask it without changing what it
// holds.
type Case struct {
	api.Route

	// Refused is set where the route reads a body and acts on one it takes: an asker it lets
	// through is sent one it refuses as it reads it, with 400, or before it reads it for a reason
	// of its own, so that asking changes nothing.
	Refused bool

	// Makes is set where the route removes what it names and reads no body: each asking names a
	// thing made for it by the owner of the namespace it is in, a grant, a secret, a token or a
	// service account, removed again by its owner where the route did not remove it.
	Makes string

	// Owned is set where the route is answered to the owners of the namespace it names, in its
	// path or its body, through a credential narrowing nothing, and refuses anybody else with
	// Refusal rather than with the router's refusal: the routes about service accounts and their
	// tokens.
	Owned   bool
	Refusal int

	// FindsNothing is set where the route answers what it finds nothing under with the absence the
	// router refuses with, and the fixture holds nothing it would find: an artifact, which only a
	// task's result writes. Its asking proves nothing here, since whoever it lets through and
	// whoever it refuses are answered alike; package api's tests, which write an artifact, hold its
	// authorisation.
	FindsNothing bool
}

// Cases are every route the API serves, as the documentation guards them. A test holds the router's
// table to them, so that a route added without a case here, or guarded otherwise than it says, fails
// until the case is written.
//
// The page's words, route by route: the administration routes are "Administrator only", a
// namespace's record is read "to an administrator and to a principal holding a grant in it", and
// drain and revoke "require grant:manage at installation scope", which is what an administrator
// holds there; a secret's declarations take workflow:read at namespace scope and writing one
// secret:write there; a push workflow:write, and secret:use where it names a secret; recording a
// repository's image pins and brick manifests workflow:write, and reading them workflow:read;
// reading what a workflow has armed workflow:read, and writing a webhook's credential workflow:write;
// starting a run and cancelling one workflow:run; reading runs, one run and a step's log run:read, a run's inputs
// being envelope contents that run:read_data alone reveals; outputs, a step's inputs and outputs and
// an artifact run:read_data. Registration is authenticated by the join token in its body, the
// runner's own routes by the runner credential alone, and the object store by the signature in the
// URL or the form. The API tokens are the caller's own, "for the caller or a service account of a
// namespace it owns", which no permission names, and so are the service accounts, "of the namespaces
// the caller owns", and GET /api/v1/me with the caller's notifications, and so are the caller's
// credentials, profile and photo; another user's photo is an administrator's to read, as the user
// is. Sharing takes grant:manage at its scope, and writing a grant an administrator too.
// The installation's authentication policy is read by whoever is signed in and a namespace's by
// whoever reads its record, and "PUT is an administrator's" for both.
var Cases = []Case{
	// Sign-in and identity, reached before anybody is signed in, or reading a session themselves.
	public("POST", "/api/v1/auth/passkey/options"),
	public("POST", "/api/v1/auth/passkey/verify"),
	public("GET", "/auth/sign-in"),
	public("GET", "/auth/enrol"),
	public("GET", "/auth/assets/{name}"),
	public("POST", "/api/v1/auth/sign-out"),
	public("POST", "/api/v1/auth/exchange"),
	public("POST", "/api/v1/auth/login"),
	public("POST", "/api/v1/auth/password/enrol"),
	public("PUT", "/api/v1/me/password"),

	// About the caller itself, which any caller reaches and is answered about alone.
	own("GET", "/api/v1/auth/policy"),
	own("GET", "/api/v1/auth/tokens"),
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/auth/tokens", Own: true}, Refused: true},
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/auth/tokens/{id}", Own: true}, Makes: "token", Owned: true, Refusal: 404},
	own("GET", "/api/v1/me"),
	// What a user says of themself, and their photo: refused before its body is read to whoever has
	// none, the bootstrap token, a service account and a narrowed token, and sent one it refuses by
	// anybody else. Removing a photo nobody in the fixture holds changes nothing.
	{Route: api.Route{Method: "PATCH", Pattern: "/api/v1/me", Own: true}, Refused: true},
	own("DELETE", "/api/v1/me/notifications/{id}"),
	own("GET", "/api/v1/me/avatar"),
	{Route: api.Route{Method: "PUT", Pattern: "/api/v1/me/avatar", Own: true}, Refused: true},
	own("DELETE", "/api/v1/me/avatar"),
	own("GET", "/api/v1/me/credentials"),
	own("DELETE", "/api/v1/me/credentials/{id}"),
	own("DELETE", "/api/v1/me/password"),
	own("POST", "/api/v1/me/totp"),
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/me/totp/confirm", Own: true}, Refused: true},
	own("DELETE", "/api/v1/me/totp"),
	own("GET", "/api/v1/service-accounts"),
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/service-accounts", Own: true}, Owned: true, Refusal: 422},
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/service-accounts/{ns}/{name}", Own: true}, Makes: "service account", Owned: true, Refusal: 404},

	// Administration: grant:manage at the installation, which an administrator holds.
	{Route: administer("PUT", "/api/v1/auth/policy"), Refused: true},
	{Route: administer("PUT", "/api/v1/{namespace}/auth/policy"), Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/auth/policy", Scope: api.Namespace, Members: true}},
	{Route: administer("POST", "/api/v1/namespaces"), Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/namespaces", Scope: api.Namespace, Members: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/namespaces/{namespace}", Scope: api.Namespace, Members: true}},
	{Route: administer("DELETE", "/api/v1/namespaces/{namespace}")},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/namespaces/{namespace}/quotas", Scope: api.Namespace, Members: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/stats/quotas", Scope: api.Namespace, Members: true}},
	{Route: administer("PUT", "/api/v1/namespaces/{namespace}/quotas"), Refused: true},
	{Route: administer("GET", "/api/v1/users")},
	{Route: administer("POST", "/api/v1/users"), Refused: true},
	{Route: administer("GET", "/api/v1/users/{login}")},
	{Route: administer("DELETE", "/api/v1/users/{login}")},
	{Route: administer("POST", "/api/v1/users/{login}/enrolment")},
	{Route: administer("POST", "/api/v1/users/{login}/recovery")},
	{Route: administer("GET", "/api/v1/users/{login}/avatar")},
	{Route: administer("DELETE", "/api/v1/users/{login}/avatar")},
	{Route: administer("GET", "/api/v1/groups")},
	{Route: administer("POST", "/api/v1/groups"), Refused: true},
	{Route: administer("GET", "/api/v1/groups/{group}")},
	{Route: administer("DELETE", "/api/v1/groups/{group}")},
	{Route: administer("PUT", "/api/v1/groups/{group}/members/{login}")},
	{Route: administer("DELETE", "/api/v1/groups/{group}/members/{login}")},
	{Route: administer("GET", "/api/v1/runners")},
	{Route: administer("POST", "/api/v1/runners/{runner}/drain"), Refused: true},
	{Route: administer("POST", "/api/v1/runners/{runner}/revoke"), Refused: true},
	{Route: administer("GET", "/api/v1/runner-pools")},
	{Route: administer("GET", "/api/v1/stats/pools")},
	{Route: administer("POST", "/api/v1/runner-pools"), Refused: true},
	{Route: administer("POST", "/api/v1/runner-pools/{pool}/join-tokens"), Refused: true},

	// A runner's own traffic, under its credential, and the join, under a join token in its body.
	public("POST", "/api/v1/runners"),
	runner("POST", "/api/v1/runners/rotate"),
	runner("POST", "/api/v1/runners/heartbeat"),
	runner("POST", "/api/v1/bus/token"),
	runner("POST", "/api/v1/tasks/redeem"),
	runner("POST", "/api/v1/tasks/logs"),

	// The built-in object store, where a presigned URL or a signed policy is the whole
	// authorisation.
	public("GET", "/objects/{key...}"),
	public("PUT", "/objects/{key...}"),
	public("POST", "/objects/{namespace}"),

	// Namespaces and workflows.
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/secrets", Permission: api.WorkflowRead, Scope: api.Namespace}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/secrets/{name}", Permission: api.WorkflowRead, Scope: api.Namespace}},
	{Route: api.Route{Method: "PUT", Pattern: "/api/v1/{namespace}/secrets/{name}", Permission: api.SecretWrite, Scope: api.Namespace}, Refused: true},
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/{namespace}/secrets/{name}", Permission: api.SecretWrite, Scope: api.Namespace}, Makes: "secret"},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/grants", Permission: api.GrantManage, Scope: api.Namespace}},
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/grants", Permission: api.GrantManage, Scope: api.Namespace, OrAdministrator: true, Seeing: true}, Refused: true},
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/{namespace}/grants/{id}", Permission: api.GrantManage, Scope: api.Namespace}, Makes: "grant"},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}/grants", Permission: api.GrantManage, Scope: api.Workflow}},
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/workflows/{workflow}/grants", Permission: api.GrantManage, Scope: api.Workflow, OrAdministrator: true, Seeing: true}, Refused: true},
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/{namespace}/workflows/{workflow}/grants/{id}", Permission: api.GrantManage, Scope: api.Workflow}, Makes: "grant"},
	{Route: api.Route{Method: "PUT", Pattern: "/api/v1/{namespace}/workflows/{workflow}/versions/{commit}", Permission: api.WorkflowWrite, Scope: api.Workflow, Also: api.SecretUse}, Refused: true},
	// A repository created under what registering a version takes, at the namespace; read, and its
	// branches, tags and tree read, under workflow:read; renamed under workflow:write, and its default branch and
	// protection changed under grant:manage, besides, which only the handler asks, since which a
	// change needs is in its body.
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/workflows", Permission: api.WorkflowWrite, Scope: api.Namespace}, Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}", Permission: api.WorkflowRead, Scope: api.Workflow}},
	{Route: api.Route{Method: "PATCH", Pattern: "/api/v1/{namespace}/workflows/{workflow}", Permission: api.WorkflowRead, Scope: api.Workflow, Also: api.GrantManage, Asks: access.SetOf(api.WorkflowWrite)}, Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}/refs", Permission: api.WorkflowRead, Scope: api.Workflow}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}/tree/{ref...}", Permission: api.WorkflowRead, Scope: api.Workflow}},
	// Deleted under workflow:delete, which only an owner holds; a body is refused before anything
	// is deleted, which is what the probe sends.
	{Route: api.Route{Method: "DELETE", Pattern: "/api/v1/{namespace}/workflows/{workflow}", Permission: api.WorkflowDelete, Scope: api.Workflow}, Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}/images", Permission: api.WorkflowRead, Scope: api.Workflow}},
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/workflows/{workflow}/images", Permission: api.WorkflowWrite, Scope: api.Workflow}, Refused: true},
	// Git's smart HTTP, answered to the command line's token alone: clone and fetch take
	// workflow:read, a push workflow:write besides, grant:manage for what only an owner does to a
	// ref, and secret:use where a commit names a secret, which only the handler can tell.
	{Route: api.Route{Method: "GET", Pattern: "/{namespace}/{repository}/info/refs", Permission: api.WorkflowRead, Scope: api.Workflow, Repository: true, Asks: access.SetOf(api.WorkflowWrite)}},
	{Route: api.Route{Method: "POST", Pattern: "/{namespace}/{repository}/git-upload-pack", Permission: api.WorkflowRead, Scope: api.Workflow, Repository: true}, Refused: true},
	{Route: api.Route{Method: "POST", Pattern: "/{namespace}/{repository}/git-receive-pack", Permission: api.WorkflowRead, Scope: api.Workflow, Repository: true, Asks: access.SetOf(api.WorkflowWrite, api.GrantManage, api.SecretUse)}, Refused: true},

	// Runs and their data.
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/workflows/{workflow}/runs", Permission: api.WorkflowRun, Scope: api.Workflow}, Refused: true},
	// What the default branch's head has armed, "to whoever holds workflow:read".
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/workflows/{workflow}/triggers", Permission: api.WorkflowRead, Scope: api.Workflow}},
	// What a webhook checks its caller against, written by whoever may change the workflow, and the
	// webhook itself, authenticated per trigger on whichever method its file declares.
	{Route: api.Route{Method: "PUT", Pattern: "/api/v1/{namespace}/workflows/{workflow}/webhooks/{method}/{path...}", Permission: api.WorkflowWrite, Scope: api.Workflow}, Refused: true},
	public("", "/hooks/{namespace}/{path...}"),
	// An event published into a namespace, under workflow:run held there, "since an event reaches
	// every workflow listening to the namespace".
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/{namespace}/events", Permission: api.WorkflowRun, Scope: api.Namespace}, Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs", Permission: api.RunRead, Scope: api.Workflow, Across: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/runs", Permission: api.RunRead, Scope: api.Workflow, Across: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/stats/runs", Permission: api.RunRead, Scope: api.Workflow, Across: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/stats/steps", Permission: api.RunRead, Scope: api.Workflow, Across: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/stats/ports", Permission: api.RunRead, Scope: api.Workflow, Across: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs/{run}", Permission: api.RunRead, Scope: api.Workflow, OfRun: true, Reveals: api.RunReadData}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/{namespace}/runs/{run}", Permission: api.RunRead, Scope: api.Workflow, OfRun: true, Reveals: api.RunReadData}},
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/runs/{run}/cancel", Permission: api.WorkflowRun, Scope: api.Workflow, OfRun: true}, Refused: true},
	{Route: api.Route{Method: "POST", Pattern: "/api/v1/runs/{run}/replay", Permission: api.WorkflowRun, Scope: api.Workflow, OfRun: true}, Refused: true},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs/{run}/steps/{step}/logs", Permission: api.RunRead, Scope: api.Workflow, OfRun: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs/{run}/outputs/{name}", Permission: api.RunReadData, Scope: api.Workflow, OfRun: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs/{run}/steps/{step}/outputs/{port}", Permission: api.RunReadData, Scope: api.Workflow, OfRun: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/runs/{run}/steps/{step}/inputs/{port}", Permission: api.RunReadData, Scope: api.Workflow, OfRun: true}},
	{Route: api.Route{Method: "GET", Pattern: "/api/v1/artifacts/{uri}", Permission: api.RunReadData, Scope: api.Workflow, OfRun: true}, FindsNothing: true},
}

// public is a route outside the principals' authorisation, whose handler stands in its place.
func public(method, pattern string) Case {
	return Case{Route: api.Route{Method: method, Pattern: pattern, Public: true}}
}

// own is a route about its caller alone, which any caller reaches.
func own(method, pattern string) Case {
	return Case{Route: api.Route{Method: method, Pattern: pattern, Own: true}}
}

// administer is a route an administrator alone reaches: grant:manage at the installation.
func administer(method, pattern string) api.Route {
	return api.Route{Method: method, Pattern: pattern, Permission: api.GrantManage, Scope: api.Installation}
}

// runner is a route a runner reaches with its credential, and nobody else.
func runner(method, pattern string) Case {
	return Case{Route: api.Route{Method: method, Pattern: pattern, Runner: true}}
}
