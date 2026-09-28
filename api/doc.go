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
// which saw the credential, with every answer it asks for. DenyAll stays the answer of an
// installation with no access model, and what a test of the routes alone is given: deny by default
// when there is nothing to grant.
//
// The permissions themselves arrived before v0.3.0, because a route declares what it needs and a
// route was written then. They are the page's own nine, held to it by a test, and they live in
// package access, which resolves them from grants, with their names kept here for the routes.
//
// # The console's session
//
// A browser presents a session rather than a token: an opaque identifier in the __Host- cookie
// OpenSession sets, which Principals reads beside the bearer token once AcceptSessions has named
// the public URL. It ends twelve hours idle and thirty days after it opened. A request changing
// something that a session carries comes from the public URL's origin or is a 403, since
// SameSite=Lax leaves the other hosts of the same site free to send the cookie. A session that may
// only enrol reaches the registration ceremony and the setting of the password that opened it, and
// nothing else, and the router refuses it on every route it authorises by who asks with a 403, which
// hides nothing: what is refused is the credential, whatever the route names. A request carrying a
// bearer token and a session is a 400.
//
// The sessions are opened on the API's own sign-in and enrolment page, GET /auth/sign-in and GET
// /auth/enrol, which NewSignIn serves on the public URL's origin, the one a ceremony is accepted
// from: static HTML, a stylesheet and three scripts embedded in the program, under a
// Content-Security-Policy that lets them load nothing from anywhere else. POST
// /api/v1/auth/sign-out, which the page offers, ends the session a request carries and clears its
// cookie.
//
// Where the policy allows passwords, which on an installation addressed by an IP address it always
// does, the page sets one from an enrolment code, POST /api/v1/auth/password/enrol, beside the
// passkey or in its place where no ceremony runs, and from a signed-in browser's session, PUT and
// DELETE /api/v1/me/password, with a TOTP generator beside it, POST /api/v1/me/totp, its
// confirmation and DELETE /api/v1/me/totp. Each is a browser's: a bearer token sets and removes
// nothing, since a token that leaked would otherwise be a way to a credential that outlives it.
//
// agk login signs in on the same page: it opens it with a loopback address and a challenge, a sign-in
// completed there mints a one-time code the page hands that address, and agk trades the code and
// its verifier for an API token at POST /api/v1/auth/exchange, which NewExchange serves (exchange.go).
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
// A push refused for naming a secret its pusher holds no secret:use for is a 403 too: the route let
// the pusher through on workflow:write, so the workflow is one they already reach. So is a token
// refused by the token routes for what the credential presenting it is, below. Only a pusher holding
// secret:use is told, with 422, that the namespace declares no secret of a name the version names,
// so whoever may not write a secret's name into a workflow learns nothing of what is declared.
//
// # A caller's own credentials
//
// The API token routes answer about their caller: "an API token for the caller or a service account
// of a namespace it owns", and the listing and revocation of those; and so do the service account
// routes, "the service accounts of the namespaces the caller owns, and a new one in one of them",
// and the removal of one. No permission names that, since holding a credential or owning a
// namespace is none of the nine, so each takes Own, and its handler is given a Caller: who asks,
// the token presented, whether its scope narrows it, and the namespaces it owns, which Principals
// says as Owners. A token narrowed by a scope mints none and reaches no credential but itself, the
// bootstrap token mints none, its one lasting use being the first administrator, and a service
// account's token mints none for that service account, whose next token someone who still means it
// mints: each is refused with 403, since the caller is known and nothing about the installation is
// hidden from them by saying so.
//
// GET /api/v1/me is the caller's own as well: who it is, its groups, what its grants resolve to at
// each scope, narrowed by its credential, which the router computes as Caller.Effective from what
// the authorizer says as Standings, and the notifications the installation tells it.
//
// # Sharing
//
// The grant routes take grant:manage at the scope they name. An administrator writes a grant as
// well, holding nothing there: "an administrator may create a grant in any namespace, for anybody,
// as a power of the installation rather than through grant:manage there", which a route declares as
// Needs.OrAdministrator, the router asks as it asks every administrator's route, and its handler
// learns of through Administering, since "the namespace's owner is told of each". Listing and
// revoking are not the power's: "in a namespace, an administrator holds what their grants give". An
// administrator widening their own access, by a role given or a deny taken away, is told the same
// way however they came to share. A grant may name a service account of another namespace only
// where its writer sees that namespace, which the router hands the handler as Sees on a route
// declaring Needs.Seeing, so that the answer never says whether another namespace exists.
package api
