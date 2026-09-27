// Package api is the HTTP boundary, and the one place a request is authorised.
//
// "Every request is authorised at the API boundary, deny by default: the API decides explicitly
// to permit or refuse, and no check is ever delegated to a client."
//
// Deny by default is easy to say and easy to lose. It is lost the first time somebody adds a
// route and forgets the check, and it is lost quietly, because a route with no check works
// perfectly for whoever is testing it. So the check is not something a handler calls: a handler
// is registered with what it needs, the router is what asks, and there is no way to register a
// handler that skips it. A route that needs nothing has to say so out loud and say why, and a
// test reads the list back.
//
// # Who asks, and what they hold
//
// An Authorizer is asked whether one principal holds one permission at one scope, and Principals
// is the installation's: it identifies a bearer token from the database, an API token or the
// bootstrap token, and answers from the grants of the principal and its groups, which package
// access resolves, with the installation an administrator's alone. It also says which namespaces a
// principal holds a grant in, as Holdings, since a namespace's record is shown to them and to an
// administrator, and to nobody else. What a token's scope narrows is intersected by the router,
// which saw the credential, with every answer it asks for. DenyAll
// stays the answer of an installation with no access model, and what a test of the routes
// alone is given: deny by default when there is nothing to grant.
//
// The permissions themselves arrived before v0.3.0, because a route declares what it needs and a
// route was written then. They are the page's own nine, held to it by a test, and they live in
// package access, which resolves them from grants, with their names kept here for the routes.
//
// # Absent and forbidden answer the same thing
//
// "An inaccessible workflow answering the same 404 as an absent one, so that probing yields
// nothing." A 403 tells a caller that something exists and they may not have it, which is an
// answer worth having if you are mapping an installation you do not belong to. So a refusal at
// a namespaced or workflow scope is a 404, and 403 is kept for the case where there is nothing
// to hide: the caller is authenticated, the resource is the installation itself, and saying no
// tells them nothing they did not already know.
//
// A push refused for naming a secret its pusher holds no secret:use for is the other 403: the
// route let the pusher through on workflow:write, so the workflow is one they already reach.
package api
