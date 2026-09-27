package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// A route about the caller's own credentials: any principal reaches it, nobody unidentified does,
// and its handler is told who asks, how their credential narrows them, and what they own through
// it, by the router rather than by reading anything itself.

// owning is an authorizer that allows nothing and says who owns what, and fails to say where broke
// is set.
type owning struct {
	owns  map[api.Principal][]string
	broke bool
}

func (owning) Allow(context.Context, api.Principal, api.Permission, api.Target) (bool, error) {
	return false, nil
}

func (o owning) Owned(_ context.Context, who api.Principal) ([]string, error) {
	if o.broke {
		return nil, context.DeadlineExceeded
	}
	return o.owns[who], nil
}

// narrowedBearer is bearer, with the credential narrowed where the request says so and the token
// it presented named after its principal.
func narrowedBearer(r *http.Request) (api.Identity, error) {
	as, err := bearer(r)
	if err != nil || as.Principal == "" {
		return as, err
	}
	as.Token = "01TOKENOF" + strings.ToUpper(string(as.Principal))
	if within := r.Header.Get("X-Within"); within != "" {
		scope, err := access.ParseTokenScope(nil, []string{within})
		if err != nil {
			return api.Identity{}, err
		}
		as.Scope = scope
	}
	return as, nil
}

func TestWhatCannotBeRegisteredAboutOnesOwn(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request, api.Caller) {}
	rt := router(t, owning{})
	for _, c := range []struct {
		name    string
		pattern string
		handler api.OwnHandler
	}{
		{"no handler", "/api/v1/auth/tokens", nil},
		{"a namespace in its path", "/api/v1/{namespace}/tokens", ok},
		{"a workflow in its path", "/api/v1/auth/{workflow}", ok},
		{"a run in its path", "/api/v1/runs/{run}/tokens", ok},
		{"an artifact in its path", "/api/v1/artifacts/{uri}/tokens", ok},
	} {
		if err := rt.HandleOwn("GET", c.pattern, api.Own{}, c.handler); err == nil {
			t.Errorf("a route about one's own with %s was registered", c.name)
		}
	}
	if err := router(t, api.DenyAll{}).HandleOwn("GET", "/api/v1/auth/tokens", api.Own{}, ok); err == nil {
		t.Error("a route about one's own was registered beside an authorizer that cannot say who owns what")
	}
	if err := rt.Handle("GET", "/api/v1/auth/tokens", api.Own{}, func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}); err == nil || !strings.Contains(err.Error(), "HandleOwn") {
		t.Errorf("a route about one's own registered with Handle, whose handler is given no caller, was answered %v", err)
	}
	if len(rt.Routes()) != 0 {
		t.Errorf("a refused route is on the surface: %+v", rt.Routes())
	}
}

// Nobody unidentified reaches a route about one's own, and anybody identified does, told who they
// are, the token they presented, and what they own: through a narrowed token, nothing, and a
// question the authorizer could not answer is not answered as nothing.
func TestARouteAboutOnesOwnIsToldWhoAsksAndWhatTheyOwn(t *testing.T) {
	auth := owning{owns: map[api.Principal][]string{"alice": {"finance", "hr"}}}
	rt, err := api.NewRouter(auth, narrowedBearer)
	if err != nil {
		t.Fatal(err)
	}
	var told api.Caller
	var owned []string
	var ownedErr error
	rt.MustHandleOwn("GET", "/api/v1/auth/tokens", api.Own{}, func(w http.ResponseWriter, r *http.Request, c api.Caller) {
		told = c
		owned, ownedErr = c.Owned(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	if routes := rt.Routes(); len(routes) != 1 || !routes[0].Own || routes[0].Permission != "" {
		t.Errorf("the surface reads %+v", routes)
	}

	if code, body := reached(t, rt, "GET", "/api/v1/auth/tokens", ""); code != http.StatusUnauthorized || !strings.Contains(body, "no credential") {
		t.Errorf("a request with no credential was answered %d: %s", code, body)
	}
	if told.Principal != "" {
		t.Errorf("the handler ran for nobody, told %+v", told)
	}

	if code, _ := reached(t, rt, "GET", "/api/v1/auth/tokens", "alice"); code != http.StatusNoContent {
		t.Fatalf("alice was answered %d", code)
	}
	if told.Principal != "alice" || told.Token != "01TOKENOFALICE" || told.Narrowed() || !slices.Equal(owned, []string{"finance", "hr"}) || ownedErr != nil {
		t.Errorf("alice's handler was told %+v owning %q, %v", told, owned, ownedErr)
	}

	w := askingOwn(rt, "alice", "X-Within", "finance")
	if w.Code != http.StatusNoContent || !told.Narrowed() || owned != nil || ownedErr != nil {
		t.Errorf("alice through a narrowed token was answered %d, told %+v owning %q", w.Code, told, owned)
	}

	broken, err := api.NewRouter(owning{broke: true}, narrowedBearer)
	if err != nil {
		t.Fatal(err)
	}
	broken.MustHandleOwn("GET", "/api/v1/auth/tokens", api.Own{}, func(w http.ResponseWriter, r *http.Request, c api.Caller) {
		owned, ownedErr = c.Owned(r.Context())
	})
	reached(t, broken, "GET", "/api/v1/auth/tokens", "alice")
	if ownedErr == nil {
		t.Error("what alice owns could not be read, and her handler was told she owns nothing")
	}

	if w := askingOwn(rt, "alice", "X-Broken", "yes"); w.Code != http.StatusInternalServerError {
		t.Errorf("a credential that could not be checked was answered %d", w.Code)
	}
}

// askingOwn is a request of the token listing from who, with one more header.
func askingOwn(rt *api.Router, who, header, value string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/v1/auth/tokens", nil)
	r.Header.Set("Authorization", "Bearer "+who)
	r.Header.Set(header, value)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}
