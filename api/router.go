package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

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

// Identify says who is asking, from the request alone.
//
// It answers the empty principal for a caller it does not recognise rather than an error, because
// an unauthenticated caller is not a failure: it is a caller who gets what an unauthenticated
// caller gets, which is "Deny by default at the API". An error is for a credential that could not
// be checked, which is a 500.
type Identify func(r *http.Request) (Principal, error)

// Router is the API's surface.
//
// Two muxes rather than one, because the documentation's surface is not one net/http can hold.
// GET /api/v1/runs/{id} and GET /api/v1/{ns}/runs both match /api/v1/runs/runs, neither is the more
// specific, and the mux refuses to serve the two together; so does GET /api/v1/artifacts/{uri} beside
// either. So a route whose path goes on from /api/v1/ with a word of its own, runs, artifacts,
// runners and the like, is held apart from those under /api/v1/{namespace}/, and a request whose
// first segment there is one of those words is answered by the first and never by the second. The
// words are the API's: a namespace of that name is one whose routes under /api/v1/ nothing reaches.
// Nothing creates a namespace through the API yet; what does, and the personal namespace a login
// is given, has to refuse them.
type Router struct {
	mux      *http.ServeMux
	auth     Authorizer
	identify Identify

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
	// caller holds Permission over, across the installation.
	Public bool
	Runner bool
	OfRun  bool
	Across bool
	Why    string
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
	return &Router{
		mux: http.NewServeMux(), namespaced: http.NewServeMux(), words: map[string]bool{},
		auth: auth, identify: identify,
	}, nil
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
// authorizer would either always allow or always refuse. A route taking OnRun carries {run} and
// neither of the others, since the router finds both from the run.
func (rt *Router) Handle(method, pattern string, g Guard, h Handler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	if g == nil {
		return fmt.Errorf("api: %s %s has no guard, and every request is authorised at the API boundary", method, pattern)
	}
	guard := g.guards()
	if err := guard.check(method, pattern); err != nil {
		return err
	}
	if guard.across {
		return fmt.Errorf("api: %s %s answers across the installation, and is registered with HandleAcross, whose handler is given what it may ask", method, pattern)
	}
	if !guard.public && !guard.run {
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
		case strings.Contains(pattern, "{namespace}") || strings.Contains(pattern, "{workflow}"):
			return fmt.Errorf("api: %s %s is authorised against the namespace and workflow of its run and names one of them as well, which a request could make another", method, pattern)
		case rt.runs == nil:
			return fmt.Errorf("api: %s %s is authorised against the workflow of its run and nothing was given to find one in", method, pattern)
		}
	}

	if err := rt.register(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		rt.serve(w, r, guard, h)
	}); err != nil {
		return err
	}
	rt.routes = append(rt.routes, Route{
		Method: method, Pattern: pattern,
		Permission: guard.permission, Scope: guard.scope,
		Public: guard.public, OfRun: guard.run, Why: guard.why,
	})
	return nil
}

// HandleAcross registers one route answering across the installation what its caller holds one
// permission over.
//
// Separate from Handle for the reason HandleRunner is: the handler is given something else, here
// Holds in place of a target, since there is no one target to give it.
func (rt *Router) HandleAcross(method, pattern string, g Across, h AcrossHandler) error {
	if h == nil {
		return fmt.Errorf("api: %s %s has no handler", method, pattern)
	}
	guard := g.guards()
	if err := guard.check(method, pattern); err != nil {
		return err
	}
	for _, named := range []string{"{namespace}", "{workflow}", "{run}", "{uri}"} {
		if strings.Contains(pattern, named) {
			return fmt.Errorf("api: %s %s answers across the installation and names %s, which is a target to authorise before the handler runs rather than one to ask about", method, pattern, named)
		}
	}
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

// AcrossHandler is a route answering across the installation, given who asks and what it may ask
// about them.
type AcrossHandler func(w http.ResponseWriter, r *http.Request, who Principal, holds Holds)

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

	who, err := rt.identify(r)
	if err != nil {
		// A credential that could not be checked is not a credential that failed. Saying
		// no here would tell a caller their token is bad when the database is down.
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	if who == "" {
		// "An unauthenticated caller: Deny by default at the API." It is a 401 rather than
		// the scope's own answer, because a caller with no credential has learned nothing
		// about what exists by being told to present one.
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "this request carries no credential")
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
		// Looked up across the installation, since the path names no namespace, before
		// anything is authorised, and answered to nobody but the authorizer: a run that is not
		// there is refused exactly as one the caller may not reach is.
		of, err := rt.runs.RunOf(r.Context(), run)
		switch {
		case errors.Is(err, ErrNoRun):
			rt.deny(w, g.scope)
			return
		case err != nil:
			refuse(w, http.StatusInternalServerError, "the request could not be authorised")
			return
		}
		target = of
	}

	allowed, err := rt.auth.Allow(r.Context(), who, g.permission, target)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authorised")
		return
	}
	if !allowed {
		rt.deny(w, g.scope)
		return
	}
	h(w, r, who, target)
}

// serveAcross is the hook every route taking Across passes through: the caller is identified as on
// any other route, and the handler is given what it may ask about them rather than an answer.
func (rt *Router) serveAcross(w http.ResponseWriter, r *http.Request, g guard, h AcrossHandler) {
	who, err := rt.identify(r)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the request could not be authenticated")
		return
	}
	if who == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "this request carries no credential")
		return
	}
	h(w, r, who, func(ctx context.Context, over Target) (bool, error) {
		if over.Namespace == "" {
			return false, errors.New("api: a route answering across the installation asked about a target naming no namespace, which is the installation itself")
		}
		return rt.auth.Allow(ctx, who, g.permission, over)
	})
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
	// string this package chose and a refusal that failed to encode would be worse than one
	// that is plain.
	fmt.Fprintf(w, "{\"error\":%q}\n", message)
}
