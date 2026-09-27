package accesstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Unknowable holds the roadmap's v0.3.0 fact against the fixture: "A principal with no permission
// on a namespace cannot establish that it exists: not through the API, and not through an error
// that distinguishes absent from forbidden." Each of as holds nothing in finance, and asks about it
// every way the API offers, each time once about finance and once about nowhere, which no namespace
// is, and the two are answered alike:
//
//   - every route of Cases that names something in its path, with each thing it names named as
//     finance holds it in turn and the rest as nothing, and again under hr, which bob owns, so that
//     neither finance's name nor anything of finance's under a namespace the asker holds tells;
//   - every request naming finance in its body or its query rather than its path, asked where the
//     asker may ask it, in hr, the answer's echo of the asker's own words aside;
//   - and every listing, and every other route reading what the asker holds, names nothing of
//     finance's: no namespace, workflow, run, secret, grant, token or service account of it.
//
// Where TimingVariable is set, the routes a prober would ask first are asked in turn about the two,
// and their medians held within Timing.Alike of each other.
func (f *Fixture) Unknowable(t testing.TB, as ...Asker) {
	t.Helper()
	for _, who := range as {
		if f.Holds(who, false).Sees(Finance) {
			t.Fatalf("%s holds something in finance, and is no prober of it", who.Name)
		}
		f.unknowableByPath(t, who)
		f.unknowableByBody(t, who)
		f.unknowableInListings(t, who)
		if os.Getenv(TimingVariable) != "" {
			f.unknowableInTime(t, who)
		}
	}
}

// financial are finance's things under each parameter a route takes, where the route can name one
// of finance's: its name, a workflow, a run of it and the run's artifact, a secret, the workflow's
// output, the service account, a grant on the namespace and one on the workflow, the service
// account's token, and the object its push stored. What no namespace holds, a login, a group, a pool
// or a runner, is named as the installation holds it, and anything else as nothing.
func (f *Fixture) financial(pattern string) map[string]string {
	run := f.Runs[Finance+"/"+Invoicing]
	document := sha256.Sum256([]byte(fmt.Sprintf(workflowDocument, Invoicing, Finance)))
	named := absent()
	maps.Copy(named, map[string]string{
		"namespace": Finance, "ns": Finance, "workflow": Invoicing, "run": run,
		"uri":   url.PathEscape("agk://run/" + run + "/normalize/ok/invoice.pdf"),
		"login": "carol", "group": TeamFinance, "pool": "default",
		"key": Finance + "/sha256/" + hex.EncodeToString(document[:]),
	})
	switch {
	case strings.Contains(pattern, "/secrets/"):
		named["name"] = f.Secrets[Finance]
	case strings.Contains(pattern, "/outputs/"):
		named["name"] = "invoices"
	case strings.Contains(pattern, "/service-accounts/"):
		named["name"] = strings.TrimPrefix(NightlySync, Finance+"/")
	}
	switch {
	case strings.Contains(pattern, "/workflows/{workflow}/grants/"):
		named["id"] = f.Grants[Finance+"/"+Invoicing]
	case strings.Contains(pattern, "/grants/"):
		named["id"] = f.Grants[Finance]
	case strings.Contains(pattern, "/auth/tokens/"):
		named["id"] = f.Token
	}
	return named
}

// identities are the parameters that say which thing a route is about, in the order its path names
// them, as against the step, the port and the commit, which say which part of it.
func identities(pattern string) []string {
	var out []string
	for _, m := range parameter.FindAllStringSubmatch(pattern, -1) {
		switch m[1] {
		case "step", "port", "commit":
		default:
			out = append(out, m[1])
		}
	}
	return out
}

// written is a pattern with each parameter written as named names it.
func written(pattern string, named map[string]string) string {
	return parameter.ReplaceAllStringFunc(pattern, func(p string) string {
		return named[parameter.FindStringSubmatch(p)[1]]
	})
}

// unknowableByPath asks every route naming something in its path, as who, once naming nothing
// and once naming finance's things, each named in turn with those before it, and holds the answers
// alike. It asks again with hr in place of any namespace a path names, and finance's things in the
// rest.
func (f *Fixture) unknowableByPath(t testing.TB, who Asker) {
	t.Helper()
	asked := 0
	for _, c := range Cases {
		ids := identities(c.Pattern)
		if len(ids) == 0 || c.Pattern == "/auth/assets/{name}" {
			// The sign-in page's own files, which name nothing anybody holds.
			continue
		}
		for _, within := range []string{"", HR} {
			named, nothing := f.financial(c.Pattern), absent()
			varying := ids
			if within != "" {
				named["namespace"], named["ns"] = within, within
				nothing["namespace"], nothing["ns"] = within, within
				varying = slices.DeleteFunc(slices.Clone(ids), func(p string) bool { return p == "namespace" || p == "ns" })
				if len(varying) == 0 {
					continue
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			none := f.ask(t, ctx, c.Method, written(c.Pattern, nothing), who, "{}")
			for i := range varying {
				mixed := maps.Clone(nothing)
				for _, p := range varying[:i+1] {
					mixed[p] = named[p]
				}
				path := written(c.Pattern, mixed)
				a := f.ask(t, ctx, c.Method, path, who, "{}")
				asked++
				if diff := Difference(none, a); diff != "" {
					t.Errorf("%s %s: as %s, %s answered %s", c.Method, c.Pattern, who.Name, path, diff)
				}
			}
			cancel()
		}
	}
	if asked < 40 {
		t.Fatalf("%d questions were asked of the routes naming something, and the API serves more of them than that", asked)
	}
}

// unknowableByBody asks what names finance in a body or a query rather than a path, as who, where
// it may ask it: a service account made in finance, a token minted for finance's, a grant in hr for
// finance's service account and its built-in identity, and a listing of runs narrowed to finance.
// A refusal saying back what it was asked about says the caller's own words, which are replaced
// before the two are compared.
func (f *Fixture) unknowableByBody(t testing.TB, who Asker) {
	t.Helper()
	for _, c := range []struct {
		method, absent, present string
		nothing, something      any
	}{
		{"POST", "/api/v1/service-accounts", "/api/v1/service-accounts",
			api.NewServiceAccount{Namespace: "nowhere", Name: "deploy"}, api.NewServiceAccount{Namespace: Finance, Name: "deploy"}},
		{"POST", "/api/v1/service-accounts", "/api/v1/service-accounts",
			api.NewServiceAccount{Namespace: "nowhere", Name: "nightly-sync"}, api.NewServiceAccount{Namespace: Finance, Name: "nightly-sync"}},
		{"POST", "/api/v1/auth/tokens", "/api/v1/auth/tokens",
			api.TokenRequest{Principal: "nowhere/nightly-sync"}, api.TokenRequest{Principal: NightlySync}},
		{"POST", "/api/v1/" + HR + "/grants", "/api/v1/" + HR + "/grants",
			api.GrantRequest{Principal: "nowhere/nightly-sync", Role: "viewer"}, api.GrantRequest{Principal: NightlySync, Role: "viewer"}},
		{"POST", "/api/v1/" + HR + "/grants", "/api/v1/" + HR + "/grants",
			api.GrantRequest{Principal: "nowhere/agentiik", Role: "viewer"}, api.GrantRequest{Principal: Finance + "/agentiik", Role: "viewer"}},
		{"GET", "/api/v1/runs?namespace=nowhere", "/api/v1/runs?namespace=" + Finance, nil, nil},
		{"GET", "/api/v1/runs?namespace=" + Finance + "&workflow=nothing", "/api/v1/runs?namespace=" + Finance + "&workflow=" + Invoicing, nil, nil},
		{"GET", "/api/v1/" + Finance + "/runs?workflow=nothing", "/api/v1/" + Finance + "/runs?workflow=" + Invoicing, nil, nil},
	} {
		none := f.ask(t, t.Context(), c.method, c.absent, who, c.nothing)
		a := f.ask(t, t.Context(), c.method, c.present, who, c.something)
		a.Body = []byte(strings.ReplaceAll(string(a.Body), Finance, "nowhere"))
		if diff := Difference(none, a); diff != "" {
			t.Errorf("%s %s naming %+v, as %s, answered %s", c.method, c.present, c.something, who.Name, diff)
		}
		if a.Status < 300 && c.method != "GET" {
			t.Errorf("%s %s naming %+v, as %s, was done", c.method, c.present, c.something, who.Name)
		}
	}
}

// unknowableInListings reads every route that names nothing in its path and reads what its caller
// holds, as who, and holds each answer to naming nothing of finance's.
func (f *Fixture) unknowableInListings(t testing.TB, who Asker) {
	t.Helper()
	words := []string{`"` + Finance + `"`, `"` + Finance + `/`, `/` + Finance + `/`, `/` + Finance + `"`,
		Invoicing, Payroll, strings.TrimPrefix(NightlySync, Finance+"/"), f.Secrets[Finance], f.Token}
	for key, run := range f.Runs {
		if strings.HasPrefix(key, Finance+"/") {
			words = append(words, run)
		}
	}
	for scope, id := range f.Grants {
		if scope == Finance || strings.HasPrefix(scope, Finance+"/") {
			words = append(words, id)
		}
	}
	read := 0
	for _, c := range Cases {
		if c.Method != "GET" || c.Public || strings.Contains(c.Pattern, "{") {
			continue
		}
		a := f.ask(t, t.Context(), "GET", c.Pattern, who, nil)
		read++
		for _, word := range words {
			if strings.Contains(string(a.Body), word) {
				t.Errorf("GET %s, as %s, answered %d naming %s of finance's: %s", c.Pattern, who.Name, a.Status, word, a.Body)
			}
		}
	}
	if read < 10 {
		t.Fatalf("%d routes reading what their caller holds were read, and the API serves more of them than that", read)
	}
}

// unknowableInTime asks the routes a prober would ask first, as who, about nowhere and about
// finance in turn, and holds their medians alike.
func (f *Fixture) unknowableInTime(t testing.TB, who Asker) {
	t.Helper()
	run := f.Runs[Finance+"/"+Invoicing]
	for _, c := range []struct{ method, absent, present string }{
		{"GET", "/api/v1/runs/" + ulid.New(), "/api/v1/runs/" + run},
		{"GET", "/api/v1/nowhere/runs", "/api/v1/" + Finance + "/runs"},
		{"GET", "/api/v1/runs?namespace=nowhere", "/api/v1/runs?namespace=" + Finance},
		{"GET", "/api/v1/nowhere/secrets", "/api/v1/" + Finance + "/secrets"},
		{"GET", "/api/v1/namespaces/nowhere", "/api/v1/namespaces/" + Finance},
		{"GET", "/api/v1/nowhere/grants", "/api/v1/" + Finance + "/grants"},
		{"POST", "/api/v1/nowhere/workflows/nothing/runs", "/api/v1/" + Finance + "/workflows/" + Invoicing + "/runs"},
		{"DELETE", "/api/v1/auth/tokens/" + ulid.New(), "/api/v1/auth/tokens/" + f.Token},
	} {
		ask := func(path string) func() {
			return func() {
				if a := f.ask(t, t.Context(), c.method, path, who, "{}"); a.Status < 400 && a.Status != http.StatusOK {
					t.Fatalf("%s %s, as %s, answered %d: %s", c.method, path, who.Name, a.Status, a.Body)
				}
			}
		}
		m := TakeAsLong(300, ask(c.absent), ask(c.present))
		t.Logf("%-28s %-6s %-52s %s", who.Name, c.method, c.present, m)
		if !m.Alike() {
			absent, present := m.Medians()
			t.Errorf("as %s, %s %s took %s where what does not exist took %s", who.Name, c.method, c.present, present, absent)
		}
	}
}
