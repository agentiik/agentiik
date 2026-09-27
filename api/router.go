package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/agk"
)

// The router, which is the thing that makes the rule structural.
//
// Handle takes a guard and a handler and wraps the one in the other. There is no method that
// takes a handler alone, and the ServeMux inside is not reachable, so a route that skips the
// check is not something somebody forgets to write: it is something they cannot write.

// Handler is a route's own work, run only after the guard let it through.
//
// It is given the principal and the target the router already resolved, so a handler never reads
// a namespace out of a path. A handler that did could read a different one from the one that was
// authorised, which is the oldest way to lose an access check that is otherwise correct.
type Handler func(w http.ResponseWriter, r *http.Request, who Principal, over Target)

// Identify says who is asking, from the request alone, and what the credential they presented
// narrows them to.
//
// It answers the empty principal for a caller it does not recognise rather than an error, because
// an unauthenticated caller is not a failure: it is a caller who gets what an unauthenticated
// caller gets, which is "Deny by default at the API". An error is for a credential that could not
// be checked, which is a 500.
type Identify func(r *http.Request) (Identity, error)

// Router is the API's surface.
//
// Two muxes rather than one, because the documentation's surface is not one net/http can hold.
// GET /api/v1/runs/{id} and GET /api/v1/{ns}/runs both match /api/v1/runs/runs, neither is the more
// specific, and the mux refuses to serve the two together; so does GET /api/v1/artifacts/{uri} beside
// either. So a route whose path goes on from /api/v1/ with a word of its own, runs, artifacts,
// runners and the like, is held apart from those under /api/v1/{namespace}/, and a request whose
// first segment there is one of those words is answered by the first and never by the second. The
// words are the API's: a namespace of that name is one whose routes under /api/v1/ nothing reaches,
// so NamespaceName refuses them to a namespace created through the API or on the server, and the
// personal namespace a login is given has to refuse them too.
type Router struct {
	mux      *http.ServeMux
	auth     Authorizer
	identify Identify

	// holdings is what a route taking OnNamespace asks which namespaces its caller holds a grant
	// in: the authorizer, where it says, and nil where it does not, which such a route is refused
	// registration on.
	holdings Holdings

	// namespaced holds every route under /api/v1/{namespace}/, and words are the first
	// segments after /api/v1/ of the routes mux holds, which a request is routed to mux by.
	namespaced *http.ServeMux
	words      map[string]bool

	// runners is what a runner-guarded route is checked against, and is nil on an
	// installation that serves no runner routes. A route taking ForRunner without it is
	// refused at registration rather than at the first heartbeat.
	runners IdentifyRunner

	// runs is where a route taking OnRun finds the namespace and workflow its run is of, and a
	// route taking OnRun without it is refused at registration, as a runner route is.
	runs FindRun

	// routes is what was registered, in registration order, for the test that reads the
	// list back and for an installation that wants to print its own surface.
	routes []Route
}

// Route is one registered route and what stands in front of it.
type Route struct {
	Method  string
	Pattern string

	Permission Permission
	Scope      Scope

	// Public and Why are set where the route is outside the authorisation hook, and Runner
	// where it is authorised by a runner credential instead. OfRun is set where the namespace and
	// workflow the route is authorised against are the ones the run in its path is of, named by
	// its identifier or by an artifact's URI. Across is set where the route answers what its
	// caller holds Permission over, across the installation or across the namespace its pattern
	// names. Members is set where the route is answered to an administrator and to whoever holds
	// a grant in the namespace, and needs no permission: see OnNamespace.
	Public  bool
	Runner  bool
	OfRun   bool
	Across  bool
	Members bool
	Why     string

	// Reveals is the permission whose holder the route answers more than Permission alone
	// is answered, where it declares one.
	Reveals Permission

	// Also is the permission the route needs besides Permission where what a request carries
	// calls for it, where it declares one.
	Also Permission

	// OrAdministrator is set where an administrator reaches the route as well, whatever they hold
	// at its scope, and Seeing where its handler is given Sees: see Needs.
	OrAdministrator bool
	Seeing          bool

	// Own is set where the route answers about its caller's own credentials and those of the
	// service accounts of the namespaces it owns, and needs no permission: see Own.
	Own bool
}

// RunnerHandler is a route a runner reaches, given the machine the credential named.
type RunnerHandler func(w http.ResponseWriter, r *http.Request, runner Runner)

// NewRouter builds one. An authorizer is required, and the deny-everything one is a legitimate
// answer rather than a stand-in: see DenyAll.
func NewRouter(auth Authorizer, identify Identify) (*Router, error) {
	if auth == nil {
		return nil, ErrNoAuthorizer
	}
	if identify == nil {
		return nil, errors.New("api: no way to say who is asking, and a request with no principal is not the same as a request from nobody")
	}
	holdings, _ := auth.(Holdings)
	return &Router{
		mux: http.NewServeMux(), namespaced: http.NewServeMux(), words: map[string]bool{},
		auth: auth, identify: confined(identify), holdings: holdings,
	}, nil
}

// confined is identify with a session that may only enrol a passkey refused, as a 403 saying so:
// such a session "enrols passkeys and nothing else: it cannot read a workflow, start a run or mint
// a token". The router identifies every caller it authorises through it, whatever the route and
// whichever hook serves it, so that no route reaches such a session by being written without the
// check; the registration ceremony, the one thing it may do, is not a route the router authorises
// by who asks.
func confined(identify Identify) Identify {
	return func(r *http.Request) (Identity, error) {
		as, err := identify(r)
		if err == nil && as.Enrolling {
			return Identity{Refused: enrolsOnly, RefusedAs: http.StatusForbidden}, nil
		}
		return as, err
	}
}

// ServeRunners says what a runner credential is checked against. Without it, a route taking
// ForRunner is refused at registration.
func (rt *Router) ServeRunners(runners IdentifyRunner) { rt.runners = runners }

// ServeRuns says where the namespace and workflow of a run are found. Without it, a route taking
// OnRun is refused at registration.
func (rt *Router) ServeRuns(runs FindRun) { rt.runs = runs }

// HandleRunner registers one route a runner reaches.
//
// Separate from Handle because the handler is given a machine rather than a principal and a
// target, and because the two hooks answer different questions. What they have in common is that
// neither is something a handler calls.
func (rt *Router) HandleRunner(method, pattern string, g ForRunner, h RunnerHandler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	if rt.runners == nil {
		return fmt.Errorf("api: %s %s is authorised by a runner credential and nothing was given to check one against", method, pattern)
	}
	if err := rt.register(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		rt.serveRunner(w, r, h)
	}); err != nil {
		return err
	}
	rt.routes = append(rt.routes, Route{Method: method, Pattern: pattern, Runner: true})
	return nil
}

// MustHandleRunner is HandleRunner for a caller that builds its routes at start-up.
func (rt *Router) MustHandleRunner(method, pattern string, g ForRunner, h RunnerHandler) {
	if err := rt.HandleRunner(method, pattern, g, h); err != nil {
		panic(err.Error())
	}
}

// serveRunner is the hook every runner route passes through.
func (rt *Router) serveRunner(w http.ResponseWriter, r *http.Request, h RunnerHandler) {
	credential, ok := bearerOf(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "this request carries no credential")
		return
	}
	runner, err := rt.runners.Runner(r.Context(), credential)
	switch {
	case errors.Is(err, ErrNoRunner):
		// A revoked credential and one that never existed answer the same thing, and a
		// runner that gets this joins again rather than retrying.
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "that credential opens nothing")
		return
	case err != nil:
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	h(w, r, runner)
}

// bearerOf reads a bearer credential, and answers false for anything else: a runner presents one
// and there is no second way in.
func bearerOf(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// Handle registers one route behind one guard.
//
// The pattern is net/http's own, so a path parameter is written {namespace} and read back by
// name. The two the router reads are namespace and workflow, and a route whose scope needs one it
// does not carry is refused here rather than at the first request: a workflow-scoped route with
// no {workflow} in its pattern would be a route authorised against an empty workflow, which any
// authorizer would either always allow or always refuse. A route taking OnRun carries {run} and no
// {workflow}, since the router finds both from the run, and a {namespace} only as one the run has to
// be in.
func (rt *Router) Handle(method, pattern string, g Guard, h Handler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	if g == nil {
		return fmt.Errorf("api: %s %s has no guard, and every request is authorised at the API boundary", method, pattern)
	}
	guard := g.guards()
	if guard.own {
		return fmt.Errorf("api: %s %s answers about its caller's own credentials, and is registered with HandleOwn, whose handler is given who asks and what they own", method, pattern)
	}
	if err := guard.check(method, pattern); err != nil {
		return err
	}
	if guard.across {
		return fmt.Errorf("api: %s %s answers across what its caller holds, and is registered with HandleAcross, whose handler is given what it may ask", method, pattern)
	}
	if guard.members {
		if rt.holdings == nil {
			return fmt.Errorf("api: %s %s is answered to whoever holds a grant in a namespace, and the authorizer does not say who does", method, pattern)
		}
		for _, named := range []string{"{workflow}", "{run}", "{uri}"} {
			if strings.Contains(pattern, named) {
				return fmt.Errorf("api: %s %s is about a namespace's record and names %s, which the record is not authorised against", method, pattern, named)
			}
		}
		guard.within = strings.Contains(pattern, "{namespace}")
	}
	if guard.seeing && rt.holdings == nil {
		return fmt.Errorf("api: %s %s is handed what its caller sees of the namespaces, and the authorizer does not say who holds a grant where", method, pattern)
	}
	if guard.administered && guard.scope == Installation {
		return fmt.Errorf("api: %s %s is an administrator's already, at the installation, and says an administrator reaches it as well", method, pattern)
	}
	if !guard.public && !guard.run && !guard.members {
		if guard.scope >= Namespace && !strings.Contains(pattern, "{namespace}") {
			return fmt.Errorf("api: %s %s is scoped to a %s and its pattern names no {namespace}", method, pattern, guard.scope)
		}
		if guard.scope == Workflow && !strings.Contains(pattern, "{workflow}") {
			return fmt.Errorf("api: %s %s is scoped to a workflow and its pattern names no {workflow}", method, pattern)
		}
	}
	if guard.artifact {
		switch {
		case !strings.Contains(pattern, "{uri}"):
			return fmt.Errorf("api: %s %s is authorised against the workflow of an artifact's run and its pattern names no {uri}", method, pattern)
		case strings.Contains(pattern, "{namespace}") || strings.Contains(pattern, "{workflow}") || strings.Contains(pattern, "{run}"):
			return fmt.Errorf("api: %s %s is authorised against the run its artifact's URI names and names a namespace, a workflow or a run as well, which a request could make another", method, pattern)
		case rt.runs == nil:
			return fmt.Errorf("api: %s %s is authorised against the workflow of an artifact's run and nothing was given to find one in", method, pattern)
		}
	}
	if guard.run && !guard.artifact {
		switch {
		case !strings.Contains(pattern, "{run}"):
			return fmt.Errorf("api: %s %s is authorised against the workflow of its run and its pattern names no {run}", method, pattern)
		case strings.Contains(pattern, "{workflow}"):
			return fmt.Errorf("api: %s %s is authorised against the workflow of its run and names a workflow as well, which a request could make another", method, pattern)
		case rt.runs == nil:
			return fmt.Errorf("api: %s %s is authorised against the workflow of its run and nothing was given to find one in", method, pattern)
		}
		guard.within = strings.Contains(pattern, "{namespace}")
	}

	if err := rt.register(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		rt.serve(w, r, guard, h)
	}); err != nil {
		return err
	}
	rt.routes = append(rt.routes, Route{
		Method: method, Pattern: pattern,
		Permission: guard.permission, Scope: guard.scope,
		Public: guard.public, OfRun: guard.run, Members: guard.members, Why: guard.why,
		Reveals: guard.reveals, Also: guard.also,
		OrAdministrator: guard.administered, Seeing: guard.seeing,
	})
	return nil
}

// HandleAcross registers one route answering, across the installation or one namespace, what its
// caller holds one permission over.
//
// Separate from Handle for the reason HandleRunner is: the handler is given something else, here
// Holds in place of a target to authorise, since there is no one target to give it. A pattern may
// name a {namespace}, which the route then answers across alone, and nothing narrower.
func (rt *Router) HandleAcross(method, pattern string, g Across, h AcrossHandler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	guard := g.guards()
	if err := guard.check(method, pattern); err != nil {
		return err
	}
	for _, named := range []string{"{workflow}", "{run}", "{uri}"} {
		if strings.Contains(pattern, named) {
			return fmt.Errorf("api: %s %s answers across a namespace or the installation and names %s, which is a target to authorise before the handler runs rather than one to ask about", method, pattern, named)
		}
	}
	guard.within = strings.Contains(pattern, "{namespace}")
	if err := rt.register(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		rt.serveAcross(w, r, guard, h)
	}); err != nil {
		return err
	}
	rt.routes = append(rt.routes, Route{
		Method: method, Pattern: pattern,
		Permission: guard.permission, Scope: guard.scope, Across: true,
	})
	return nil
}

// MustHandleAcross is HandleAcross for a caller that builds its routes at start-up.
func (rt *Router) MustHandleAcross(method, pattern string, g Across, h AcrossHandler) {
	if err := rt.HandleAcross(method, pattern, g, h); err != nil {
		panic(err.Error())
	}
}

// HandleOwn registers one route about its caller's own credentials and what it owns.
//
// Separate from Handle for the reason HandleAcross is: the handler is given a Caller in place of a
// target, since what it answers is the caller's own and there is nothing in its path to authorise.
// Its pattern names no namespace, workflow, run or artifact, each of which is a target a handler
// could be handed unauthorised, and the authorizer has to say what a principal owns.
func (rt *Router) HandleOwn(method, pattern string, g Own, h OwnHandler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	for _, named := range []string{"{namespace}", "{workflow}", "{run}", "{uri}"} {
		if strings.Contains(pattern, named) {
			return fmt.Errorf("api: %s %s answers about its caller's own credentials and names %s, which is a target to authorise before the handler runs rather than something the caller owns", method, pattern, named)
		}
	}
	owners, ok := rt.auth.(Owners)
	if !ok {
		return fmt.Errorf("api: %s %s answers about the service accounts of the namespaces its caller owns, and the authorizer does not say who owns what", method, pattern)
	}
	standings, _ := rt.auth.(Standings)
	if err := rt.register(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		rt.serveOwn(w, r, owners, standings, h)
	}); err != nil {
		return err
	}
	rt.routes = append(rt.routes, Route{Method: method, Pattern: pattern, Scope: Installation, Own: true})
	return nil
}

// MustHandleOwn is HandleOwn for a caller that builds its routes at start-up.
func (rt *Router) MustHandleOwn(method, pattern string, g Own, h OwnHandler) {
	if err := rt.HandleOwn(method, pattern, g, h); err != nil {
		panic(err.Error())
	}
}

// OwnHandler is a route about its caller's own credentials, given who asks, as Caller says.
type OwnHandler func(w http.ResponseWriter, r *http.Request, caller Caller)

// serveOwn is the hook every route taking Own passes through: the caller is identified as on any
// other route, a request with no credential is refused, and the handler is given who asks.
//
// It asks the authorizer nothing, so a refusal added to allow reaches none of these routes: an
// enrolment-only session "enrols passkeys and nothing else", and is refused by rt.identify, which
// confined makes of the router's Identify, with the 403 openapi.json answers it on each of them.
func (rt *Router) serveOwn(w http.ResponseWriter, r *http.Request, owners Owners, standings Standings, h OwnHandler) {
	as, err := rt.identify(r)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	if as.Principal == "" {
		unauthenticated(w, as)
		return
	}
	h(w, r, Caller{
		Principal: as.Principal, Token: as.Token, scope: as.Scope, owners: owners, standings: standings,
		allow: func(ctx context.Context, what Permission, over Target) (bool, error) {
			return rt.allow(ctx, as, what, over)
		},
	})
}

// AcrossHandler is a route answering across the installation or one namespace, given who asks,
// the namespace its path names, if any, and what it may ask about them.
//
// The namespace is handed over rather than read from the path by the handler, for the reason a
// Handler is given its target: it is the one Holds answers about, and a handler reading its own
// could read another.
type AcrossHandler func(w http.ResponseWriter, r *http.Request, who Principal, within Target, holds Holds)

// apiPrefix is where the routes under a namespace and the routes under a word of their own part.
const apiPrefix = "/api/v1/"

// register puts one route on the mux, and answers the mux's refusal of it as an error.
//
// net/http refuses two patterns that each match a path the other does when neither is the more
// specific, /api/v1/objects/{key...} and /api/v1/{namespace}/runs for one, and it refuses by
// panicking. Handle promises an error for a route it cannot serve, and a panic from inside it
// would take down whatever was registering routes at start-up, with nothing to say which set of
// routes the other one was. The mux checks before it adds anything, so a refused pattern leaves
// it as it was.
func (rt *Router) register(method, pattern string, h http.HandlerFunc) (err error) {
	defer func() {
		if refused := recover(); refused != nil {
			err = fmt.Errorf("api: %s %s cannot be served beside the routes already registered: %v", method, pattern, refused)
		}
	}()
	mux, word := rt.mux, ""
	if rest, under := strings.CutPrefix(pattern, apiPrefix); under {
		first, _, _ := strings.Cut(rest, "/")
		if strings.HasPrefix(first, "{") {
			mux = rt.namespaced
		} else {
			word = first
		}
	}
	if word != "" && !agk.IsReservedNamespace(word) {
		return fmt.Errorf("api: %s %s routes on %q, which is not a reserved namespace name: a namespace of that name would lose its routes to this one, so the word joins agk.ReservedNamespaces, and the schemas' list, first", method, pattern, word)
	}
	mux.HandleFunc(method+" "+pattern, h)
	if word != "" {
		rt.words[word] = true
	}
	return nil
}

// MustHandle is Handle for a caller that builds its routes at start-up, where a refusal is a
// programming fault rather than a condition to recover from.
func (rt *Router) MustHandle(method, pattern string, g Guard, h Handler) {
	if err := rt.Handle(method, pattern, g, h); err != nil {
		panic(err.Error())
	}
}

// Routes is what was registered. The slice is a copy: a caller reading the surface cannot edit it.
func (rt *Router) Routes() []Route {
	out := append([]Route(nil), rt.routes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// ServeHTTP answers a request, or refuses it.
//
// A route nobody registered is a 404 from the mux, which is the same answer an inaccessible one
// gets, and that is the right accident: an installation's surface is not a thing to enumerate by
// asking.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.muxFor(r).ServeHTTP(w, r) }

// muxFor is the mux that answers a request: the one holding the API's own words where the path's
// first segment after /api/v1/ is one of them, and the one holding the namespaced routes for any
// other path under /api/v1/.
//
// The segment is read as the mux reads it, from the escaped path and unescaped on its own, so that
// run%73 is the word runs to both. A path the mux would clean first, /api/v1//runs or
// /api/v1/finance/../runs, is answered by whichever mux with the redirect to its clean form, which
// then comes back here and is routed on that.
func (rt *Router) muxFor(r *http.Request) *http.ServeMux {
	rest, under := strings.CutPrefix(r.URL.EscapedPath(), apiPrefix)
	if !under {
		return rt.mux
	}
	first, _, _ := strings.Cut(rest, "/")
	if word, err := url.PathUnescape(first); err == nil && rt.words[word] {
		return rt.mux
	}
	return rt.namespaced
}

// serve is the hook every guarded route passes through.
func (rt *Router) serve(w http.ResponseWriter, r *http.Request, g guard, h Handler) {
	target := Target{
		Namespace: r.PathValue("namespace"),
		Workflow:  r.PathValue("workflow"),
	}

	if g.public {
		h(w, r, "", target)
		return
	}

	as, err := rt.identify(r)
	if err != nil {
		// A credential that could not be checked is not a credential that failed. Saying
		// no here would tell a caller their token is bad when the database is down.
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	who := as.Principal
	if who == "" {
		// "An unauthenticated caller: Deny by default at the API." It is a 401 rather than
		// the scope's own answer, because a caller with no credential has learned nothing
		// about what exists by being told to present one.
		unauthenticated(w, as)
		return
	}

	if g.run {
		run := r.PathValue("run")
		if g.artifact {
			// A URI that does not parse names no run, and is refused as the absence it is.
			u, err := agk.ParseURI(r.PathValue("uri"))
			if err != nil {
				rt.deny(w, g.scope)
				return
			}
			run = string(u.Run)
		}
		// Looked up across the installation, since the path names no namespace or names one
		// the run has to be in, before anything is authorised, and answered to nobody but the
		// router and the authorizer: a run that is not there is refused exactly as one the
		// caller may not reach is.
		of, err := rt.runs.RunOf(r.Context(), run)
		switch {
		case errors.Is(err, ErrNoRun):
			rt.deny(w, g.scope)
			return
		case err != nil:
			refuse(w, http.StatusInternalServerError, "the request could not be authorised")
			return
		}
		if g.within && of.Namespace != target.Namespace {
			// A run asked for under a namespace it is not in is not there, and is refused
			// before anything is asked about the namespace it is in.
			rt.deny(w, g.scope)
			return
		}
		target = of
	}

	// What the authorizer is asked about: the installation for a route at that scope, whatever
	// its path names, so that "installation" is one question with one answer, and the target
	// the path resolved to at any other.
	asked := target
	if g.scope == Installation {
		asked = Target{}
	}
	// A route taking OnNamespace asks who sees which namespace instead of a permission, and
	// refuses one naming a namespace its caller does not see.
	var allowed bool
	sees := seesNothing
	if g.members {
		sees, err = rt.seeing(r.Context(), as)
		allowed = err == nil && (!g.within || sees(target.Namespace))
	} else {
		allowed, err = rt.admits(r.Context(), as, g, asked)
	}
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authorised")
		return
	}
	if !allowed {
		rt.deny(w, g.scope)
		return
	}
	if g.seeing {
		if sees, err = rt.seeing(r.Context(), as); err != nil {
			refuse(w, http.StatusInternalServerError, "the request could not be authorised")
			return
		}
	}
	// Set on every route, seeing nothing where the route does not take OnNamespace, for the
	// reason the questions below are.
	r = r.WithContext(context.WithValue(r.Context(), seesKey{}, sees))
	// Set on every route, to a question answered false where the route declares none, so that a
	// request built from this one and served again, as a facade over the API would serve one,
	// asks what its own route declared rather than what this one did.
	authorised := target.Namespace
	r = r.WithContext(context.WithValue(r.Context(), revealingKey{}, Holds(func(ctx context.Context, over Target) (bool, error) {
		if g.reveals == "" {
			return false, nil
		}
		if over.Namespace != authorised {
			return false, fmt.Errorf("api: a route authorised in namespace %q asked what it may reveal in %q, which it was not authorised in", authorised, over.Namespace)
		}
		return rt.allow(ctx, as, g.reveals, over)
	})))
	// Set on every route for the same reason, and asked over the target the route was
	// authorised against and no other, since what a request carries is judged where it goes.
	r = r.WithContext(context.WithValue(r.Context(), alsoKey{}, func(ctx context.Context) (bool, error) {
		if g.also == "" {
			return false, nil
		}
		return rt.allow(ctx, as, g.also, asked)
	}))
	request := r
	r = r.WithContext(context.WithValue(r.Context(), stillKey{}, func(ctx context.Context) (bool, error) {
		// The credential first, since a token revoked or a session ended while its holder's
		// grants remain is access lost too, then the permission, through what the credential
		// narrows it to as it reads now.
		again, err := rt.identify(request.WithContext(ctx))
		if err != nil || again.Principal != who {
			return false, err
		}
		if g.members {
			sees, err := rt.seeing(ctx, again)
			return err == nil && (!g.within || sees(target.Namespace)), err
		}
		return rt.admits(ctx, again, g, asked)
	}))
	h(w, r, who, target)
}

// serveAcross is the hook every route taking Across passes through: the caller is identified as on
// any other route, and the handler is given what it may ask about them rather than an answer.
func (rt *Router) serveAcross(w http.ResponseWriter, r *http.Request, g guard, h AcrossHandler) {
	as, err := rt.identify(r)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	who := as.Principal
	if who == "" {
		unauthenticated(w, as)
		return
	}
	var within Target
	if g.within {
		// The mux matches no empty segment, so this is never empty; it is refused all the
		// same, since a route naming a namespace and handed none would answer across the
		// installation.
		if within.Namespace = r.PathValue("namespace"); within.Namespace == "" {
			rt.deny(w, Namespace)
			return
		}
	}
	h(w, r, who, within, func(ctx context.Context, over Target) (bool, error) {
		switch {
		case over.Namespace == "":
			return false, errors.New("api: a route answering across the installation asked about a target naming no namespace, which is the installation itself")
		case within.Namespace != "" && over.Namespace != within.Namespace:
			return false, fmt.Errorf("api: a route answering across namespace %q asked about %q, which its path does not name", within.Namespace, over.Namespace)
		}
		return rt.allow(ctx, as, g.permission, over)
	})
}

// allow is the one place the router asks the authorizer: what the principal holds over the target,
// intersected with what its credential narrows it to. The narrowing is asked first, and a token
// that does not keep the permission there is refused without a question the principal's grants
// would have answered yes to, since "a scope can only narrow".
func (rt *Router) allow(ctx context.Context, as Identity, what Permission, over Target) (bool, error) {
	if !as.Scope.Keeps(what, access.Scope{Namespace: over.Namespace, Workflow: over.Workflow}) {
		return false, nil
	}
	return rt.auth.Allow(ctx, as.Principal, what, over)
}

// admits is whether a route taking Needs lets the caller through: the permission it needs over the
// target, and for a route an administrator reaches as well, the administrator's power where the
// permission is not held, asked as every administrator's route asks it. The power reaches a target
// a grant could name and nothing else, since a path naming a namespace no grant could name is the
// absence it is to everybody, and its handler would be handed a name the database cannot hold.
func (rt *Router) admits(ctx context.Context, as Identity, g guard, over Target) (bool, error) {
	allowed, err := rt.allow(ctx, as, g.permission, over)
	if err != nil || allowed || !g.administered || !grantable(over) {
		return allowed, err
	}
	return rt.allow(ctx, as, GrantManage, Target{})
}

// grantable says whether a grant's scope could name the target: a namespace, or a workflow of one,
// each on its grammar.
func grantable(over Target) bool {
	_, err := access.ParseScope(access.Scope{Namespace: over.Namespace, Workflow: over.Workflow}.String())
	return over.Namespace != "" && err == nil
}

// seeing is what a caller sees of the namespaces' records: every one where it administers the
// installation through a credential that carries the power, which is asked as grant:manage there is
// for any other administrator's route, and otherwise the namespaces it holds a grant in that its
// credential reaches. Whether it administers is asked first, and the namespaces only where it does
// not, since an administrator sees them all.
func (rt *Router) seeing(ctx context.Context, as Identity) (func(string) bool, error) {
	administers, err := rt.allow(ctx, as, GrantManage, Target{})
	if err != nil {
		return nil, err
	}
	if administers {
		return func(namespace string) bool { return namespace != "" }, nil
	}
	held, err := rt.holdings.HeldIn(ctx, as.Principal)
	if err != nil {
		return nil, err
	}
	held = slices.Clone(held)
	return func(namespace string) bool {
		return slices.Contains(held, namespace) && as.Scope.Reaches(namespace)
	}, nil
}

// unauthenticated answers a caller nobody was identified as: with the status and the sentence its
// identification said where a credential came that this request may not carry, which no other
// credential would put right and so asks for none; with what it said where a credential came and
// opened nothing; and otherwise with the absence of one.
func unauthenticated(w http.ResponseWriter, as Identity) {
	if as.RefusedAs != 0 {
		refuse(w, as.RefusedAs, as.Refused)
		return
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	why := as.Refused
	if why == "" {
		why = "this request carries no credential"
	}
	refuse(w, http.StatusUnauthorized, why)
}

// deny answers a refusal in the shape the scope calls for.
//
// "An inaccessible workflow answering the same 404 as an absent one, so that probing yields
// nothing." A 403 at a namespaced scope would be an oracle: ask for every name, and the ones
// that answer 403 are the ones that exist.
func (rt *Router) deny(w http.ResponseWriter, scope Scope) {
	if scope.Hides() {
		refuse(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	refuse(w, http.StatusForbidden, "you do not hold what this needs")
}

// refuse writes one refusal, and says nothing a caller could learn from.
func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// Written by hand rather than marshalled, because the only variable is a constant
	// string this package chose, the sentence Principals refuses a token with among them, and a
	// refusal that failed to encode would be worse than one that is plain.
	fmt.Fprintf(w, "{\"error\":%q}\n", message)
}

// Still answers, for the route serving r, whether its caller still holds what the route was
// authorised by: the request's credential identified again as the same principal, and the
// authorizer asked again about the same permission and the same target.
//
// A request is authorised once, when it arrives, and deleting a grant "revokes one grant, from the
// next request". A route whose answer goes on for as long as its caller reads, a log stream, is one
// request that may never end, so it asks this as it goes and stops once the answer is no. A request
// the router did not serve is answered no.
func Still(r *http.Request) func(context.Context) (bool, error) {
	if still, ok := r.Context().Value(stillKey{}).(func(context.Context) (bool, error)); ok {
		return still
	}
	return func(context.Context) (bool, error) { return false, nil }
}

// stillKey is where the router leaves the question Still asks.
type stillKey struct{}
