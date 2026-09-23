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
// # What v0.3.0 fills
//
// Principals, groups, grants and roles are a later milestone. What this milestone builds is the
// shape they arrive into: an Authorizer is asked whether one principal holds one permission at
// one scope, and until there is anything to ask, the answer is no. That is not a placeholder
// standing in for a decision; it is the decision. An installation with no access model refuses
// every request that needs one, which is what deny by default means when there is nothing to
// grant yet.
//
// The permissions themselves are here rather than in v0.3.0, because a route declares what it
// needs and a route is written now. They are the page's own nine, held to it by a test.
//
// # Absent and forbidden answer the same thing
//
// "An inaccessible workflow answering the same 404 as an absent one, so that probing yields
// nothing." A 403 tells a caller that something exists and they may not have it, which is an
// answer worth having if you are mapping an installation you do not belong to. So a refusal at
// a namespaced or workflow scope is a 404, and 403 is kept for the case where there is nothing
// to hide: the caller is authenticated, the resource is the installation itself, and saying no
// tells them nothing they did not already know.
package api
