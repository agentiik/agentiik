package accesstest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The router's refusals, as it words them: the absence a namespace or a workflow refuses with, the
// refusal at the installation, a request presenting no credential, a token that opens nothing, and
// a principal's credential at a runner's route. A token that opens nothing is told more once the
// bootstrap token has ended, so that one is held to its beginning.
const (
	hidden       = `{"error":"no such thing, or not yours"}` + "\n"
	forbidden    = `{"error":"you do not hold what this needs"}` + "\n"
	noCredential = `{"error":"this request carries no credential"}` + "\n"
	opensNothing = `{"error":"that token opens nothing`
	notARunner   = `{"error":"that credential opens nothing"}` + "\n"
)

// refusedBody is what a route the fixture asks without changing anything is sent: a member no
// route reads, which every route reading a body refuses with 400 before it acts, unless it refused
// the asker before reading it, with a 403 or a 404 of its own.
const refusedBody = `{"agentiik_access_fixture":true}`

// Target is what one asking names: a namespace, a workflow of it, a run of the workflow, or none of
// them for a route about the installation or the caller.
type Target struct{ Namespace, Workflow, Run string }

func (at Target) String() string {
	switch {
	case at.Run != "":
		return "run " + at.Run + " of " + at.Namespace + "/" + at.Workflow
	case at.Workflow != "":
		return at.Namespace + "/" + at.Workflow
	case at.Namespace != "":
		return at.Namespace
	}
	return "the installation"
}

// Probe asks every route served, the router's table of them, as every asker, about each thing of
// the fixture the route can name, as its case of Cases says to ask it, and holds each answer to
// what Holds says the asker holds: let through where it holds what the route needs, and otherwise
// refused with the answer the route's scope refuses with, byte for byte. A route that removes what
// it names is asked about a thing made for the asking; one that would act on a body it takes is
// sent one it refuses; the listings are held to what they may list. Nothing the fixture holds is
// changed. A route served with no case is a failure, since nobody has said how it is guarded.
//
// It asks before the lapse, or from it where lapsed is set, whose instant the installation's clock
// is to read throughout.
func (f *Fixture) Probe(t testing.TB, served []api.Route, lapsed bool) {
	t.Helper()
	askers := append(f.Askers(), f.Nobody, f.Bootstrap)
	asked := 0
	for _, r := range served {
		i := slices.IndexFunc(Cases, func(c Case) bool { return c.Method == r.Method && c.Pattern == r.Pattern })
		if i < 0 {
			t.Errorf("%s %s is served, and no case of Cases says how it is guarded or asked", r.Method, r.Pattern)
			continue
		}
		c := Cases[i]
		if c.Public {
			continue
		}
		for _, at := range f.targets(c) {
			for _, as := range askers {
				f.probe(t, c, as, at, f.Holds(as, lapsed))
				asked++
			}
		}
	}
	for _, as := range askers {
		f.lists(t, as, f.Holds(as, lapsed), lapsed)
	}
	if asked < 1000 {
		t.Fatalf("the routes were asked %d questions, and the fixture names more than that", asked)
	}
}

// targets are what a route is asked about: every run of the fixture where it names a run, every
// workflow where it names a workflow, each shared namespace where it names a namespace, in its path
// or, for a service account made, in its body. A route of the installation's is asked about what no
// namespace holds, since whether it lets an asker through does not depend on what it names, and a
// name it finds nothing under keeps an administrator's asking from changing anything.
func (f *Fixture) targets(c Case) []Target {
	switch {
	case strings.Contains(c.Pattern, "{run}") || strings.Contains(c.Pattern, "{uri}"):
		var runs []Target
		for _, key := range slices.Sorted(maps.Keys(f.Runs)) {
			namespace, workflow, _ := strings.Cut(key, "/")
			runs = append(runs, Target{namespace, workflow, f.Runs[key]})
		}
		return runs
	case strings.Contains(c.Pattern, "{workflow}") || strings.Contains(c.Pattern, "{repository}"):
		return []Target{{Finance, Invoicing, ""}, {Finance, Payroll, ""}, {HR, Onboarding, ""}, {HR, Offboarding, ""}}
	case c.Scope == api.Installation && !c.Own:
		return []Target{{}}
	case c.Makes == "token":
		// The token made is finance/nightly-sync's, the one service account the fixture holds.
		return []Target{{Namespace: Finance}}
	case strings.Contains(c.Pattern, "{namespace}") || strings.Contains(c.Pattern, "{ns}") || c.Owned:
		return []Target{{Namespace: Finance}, {Namespace: HR}}
	}
	return []Target{{}}
}

// owner is who owns a shared namespace, and makes and removes what a probe acts on there.
func (f *Fixture) owner(namespace string) Asker {
	if namespace == HR {
		return f.Bob
	}
	return f.Carol
}

// lets says whether a route lets as through at at, holding h.
func (f *Fixture) lets(c Case, as Asker, at Target, h Holding) bool {
	switch {
	case !h.Opens || c.Runner:
		return false
	case c.Repository && as.Bearer == "":
		// Git is answered with the command line's token alone, and a session is no credential
		// there.
		return false
	case c.Owned && c.Makes == "token" && as.Principal == NightlySync:
		// The token made is finance/nightly-sync's own, which it revokes as its own.
		return true
	case c.Owned:
		return slices.Contains(h.Owns, at.Namespace)
	case c.Own, c.Across:
		// Answered about the caller, or listing what it holds: each is held by lists.
		return true
	case c.Members && !strings.Contains(c.Pattern, "{namespace}"):
		return true
	case c.Members:
		return h.Sees(at.Namespace)
	case c.Scope == api.Installation:
		return h.Admin
	case c.OrAdministrator && h.Admin:
		return true
	}
	return h.Hold(string(c.Permission), at.Namespace, at.Workflow)
}

// refusal is the answer a route refuses as with, holding h: its status, and the body or the
// beginning of the body the router answers it with, empty where the route answers it itself.
func refusal(c Case, as Asker, h Holding) (int, string) {
	switch {
	case as.Bearer == "" && (as.Session == nil || c.Repository):
		return http.StatusUnauthorized, noCredential
	case c.Runner && as.Bearer == "":
		return http.StatusUnauthorized, noCredential
	case c.Runner:
		return http.StatusUnauthorized, notARunner
	case !h.Opens:
		return http.StatusUnauthorized, opensNothing
	case c.Owned:
		return c.Refusal, ""
	case c.Scope == api.Installation:
		return http.StatusForbidden, forbidden
	}
	return http.StatusNotFound, hidden
}

// probe asks one route once, as as about at.
func (f *Fixture) probe(t testing.TB, c Case, as Asker, at Target, h Holding) {
	t.Helper()
	allowed := f.lets(c, as, at, h)
	made := f.make(t, c, at)
	path := f.fill(c.Pattern, at, made)
	var body any
	switch {
	case c.Refused:
		body = refusedBody
	case c.Owned && c.Method == "POST":
		made = f.freshName()
		body = api.NewServiceAccount{Namespace: at.Namespace, Name: made}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := f.ask(t, ctx, c.Method, path, as, body)
	name := c.Method + " " + c.Pattern
	status, refused := refusal(c, as, h)
	f.carriesNoSecret(t, name, as, a)
	switch {
	case allowed && c.Makes != "" && a.Status != http.StatusNoContent:
		t.Errorf("%s: %s, holding it, asked about %s to remove %s, which it answered %d: %s", name, as.Name, at, made, a.Status, a.Body)
	case allowed && c.Owned && c.Method == "POST" && a.Status != http.StatusCreated:
		t.Errorf("%s: %s, owning %s, was answered %d making a service account there: %s", name, as.Name, at.Namespace, a.Status, a.Body)
	case allowed && c.FindsNothing:
	case allowed && c.Refused && (a.Status < 400 || a.Status >= 500):
		// The body sent is one the route refuses as it reads it, so that asking changes
		// nothing: a route answering it otherwise may have acted on the fixture.
		t.Errorf("%s: %s asked about %s at %s with a body the route refuses, and was answered %d: %s", name, as.Name, at, path, a.Status, a.Body)
	case allowed && (a.Status == http.StatusUnauthorized || a.Status >= 500 || refusedAs(a, status, refused)):
		t.Errorf("%s: %s, holding what it needs, asked about %s at %s and was refused %d: %s", name, as.Name, at, path, a.Status, a.Body)
	case !allowed && a.Status != status:
		t.Errorf("%s: %s, holding nothing it needs, asked about %s at %s and was answered %d, want %d: %s", name, as.Name, at, path, a.Status, status, a.Body)
	case !allowed && !bytes.HasPrefix(a.Body, []byte(refused)):
		t.Errorf("%s: %s was refused about %s with %s, which is not the router's %s", name, as.Name, at, a.Body, refused)
	case !allowed && refused != "" && (a.Header.Get("Content-Type") != "application/json" || a.Header.Get("Cache-Control") != "no-store"):
		t.Errorf("%s: %s was refused about %s with the headers %v", name, as.Name, at, a.Header)
	}
	if allowed && c.Reveals != "" && a.Status == http.StatusOK {
		// A run's inputs are envelope contents, answered only to whoever holds run:read_data on
		// its workflow, and left out for anybody else.
		var view map[string]json.RawMessage
		json.Unmarshal(a.Body, &view)
		if _, told := view["inputs"]; told != h.Hold(string(c.Reveals), at.Namespace, at.Workflow) {
			t.Errorf("%s: %s was answered the run's inputs %v, holding %s there %v", name, as.Name, told, c.Reveals, !told)
		}
	}
	if allowed && c.Makes == "token" {
		f.revoked(made)
	}
	f.unmake(t, c, at, made, allowed)
}

// carriesNoSecret holds an answer to carrying no value of a secret the fixture declared, in any
// spelling an answer writes bytes in: "secret values, at any role".
func (f *Fixture) carriesNoSecret(t testing.TB, name string, as Asker, a Answer) {
	t.Helper()
	answer := string(a.Body) + fmt.Sprint(a.Header)
	for _, value := range f.Values {
		for _, spelled := range []string{
			value, base64.StdEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value)),
			hex.EncodeToString([]byte(value)),
		} {
			if strings.Contains(answer, spelled) {
				t.Errorf("%s, asked by %s, answered %d carrying a secret's value: %s", name, as.Name, a.Status, answer)
			}
		}
	}
}

// refusedAs says whether an answer is the refusal of status and refused.
func refusedAs(a Answer, status int, refused string) bool {
	return a.Status == status && refused != "" && bytes.HasPrefix(a.Body, []byte(refused))
}

// freshName is a name no probe has used.
func (f *Fixture) freshName() string {
	f.fresh++
	return fmt.Sprintf("probe-%d", f.fresh)
}

// make makes what a route removing what it names is asked to remove, as the namespace's owner, and
// answers what its path names it by.
func (f *Fixture) make(t testing.TB, c Case, at Target) string {
	t.Helper()
	owner := f.owner(at.Namespace)
	switch c.Makes {
	case "grant":
		scope := at.Namespace
		if at.Workflow != "" {
			scope += "/" + at.Workflow
		}
		return f.grant(t, owner, scope, api.GrantRequest{Principal: at.Namespace + "/agentiik", Role: "viewer"})
	case "secret":
		name, value := f.freshName(), "sk_probe_"+randomHex(t)
		f.must(t, "PUT", "/api/v1/"+at.Namespace+"/secrets/"+name, owner, api.Declare{Provider: "builtin", Value: &value}, http.StatusCreated)
		return name
	case "token":
		return f.mint(t, f.Carol, NightlySync, api.TokenRequest{Principal: NightlySync, DeviceLabel: "a probe's"}).tokenID
	case "service account":
		name := f.freshName()
		f.must(t, "POST", "/api/v1/service-accounts", owner, api.NewServiceAccount{Namespace: at.Namespace, Name: name}, http.StatusCreated)
		return name
	}
	return ""
}

// unmake removes, as the namespace's owner, what a probe made and did not remove: a thing made for
// a route an asker was refused, and a service account an owner made.
func (f *Fixture) unmake(t testing.TB, c Case, at Target, made string, allowed bool) {
	t.Helper()
	if made == "" || (allowed && c.Makes != "") || (!allowed && c.Makes == "") {
		return
	}
	owner := f.owner(at.Namespace)
	kind := c.Makes
	if kind == "" {
		kind = "service account"
	}
	switch kind {
	case "grant":
		path := "/api/v1/" + at.Namespace + "/grants/" + made
		if at.Workflow != "" {
			path = "/api/v1/" + at.Namespace + "/workflows/" + at.Workflow + "/grants/" + made
		}
		f.must(t, "DELETE", path, owner, nil, http.StatusNoContent)
	case "secret":
		f.must(t, "DELETE", "/api/v1/"+at.Namespace+"/secrets/"+made, owner, nil, http.StatusNoContent)
	case "token":
		f.revoke(t, f.Carol, made)
	case "service account":
		f.must(t, "DELETE", "/api/v1/service-accounts/"+at.Namespace+"/"+made, owner, nil, http.StatusNoContent)
	}
}

// parameter is a path parameter of a pattern, {name} or {name...}.
var parameter = regexp.MustCompile(`\{([a-z]+)(\.\.\.)?\}`)

// fill writes a pattern's parameters as naming at, made being what the probe made for it: what
// the route's path names inside a namespace as the fixture holds it there, and anything else,
// which a route of the installation's or of the caller's own names, as nothing anybody holds.
func (f *Fixture) fill(pattern string, at Target, made string) string {
	nothing := absent()
	return parameter.ReplaceAllStringFunc(pattern, func(p string) string {
		name := parameter.FindStringSubmatch(p)[1]
		switch name {
		case "namespace", "ns":
			if at.Namespace != "" {
				return at.Namespace
			}
		case "workflow":
			return at.Workflow
		case "repository":
			return at.Workflow + ".git"
		case "run":
			return at.Run
		case "uri":
			return url.PathEscape("agk://run/" + at.Run + "/normalize/ok/invoice.pdf")
		case "step":
			return "normalize"
		case "port":
			return "ok"
		case "commit":
			return Commit
		case "name":
			switch {
			case made != "":
				return made
			case strings.Contains(pattern, "/secrets/"):
				return f.Secrets[at.Namespace]
			case strings.Contains(pattern, "/outputs/"):
				return "invoices"
			}
		case "id":
			if made != "" {
				return made
			}
		}
		return nothing[name]
	})
}

// absent names nothing anybody holds under each parameter a route takes, each under a name its
// grammar takes, so that what is asked about is the absence of the thing and not the refusal of a
// name.
func absent() map[string]string {
	return map[string]string{
		"namespace": "nowhere", "ns": "nowhere", "workflow": "nothing", "run": ulid.New(),
		"step": "normalize", "port": "ok", "commit": Commit, "login": "nobody", "group": "nobody",
		"runner": strings.ToLower(ulid.New()), "pool": "nowhere", "key": "nowhere/sha256/" + strings.Repeat("0", 64),
		"uri":  url.PathEscape("agk://run/" + ulid.New() + "/normalize/ok/invoice.pdf"),
		"name": "nothing", "id": ulid.New(),
	}
}

// lists holds each listing, as as asks it, to what h lets it list, before the lapse or from it
// where lapsed is set: the runs of the workflows it holds run:read on, across the installation and
// within each namespace; the namespaces it sees; the service accounts of the namespaces it owns;
// the tokens it may revoke, by identifier, a narrowed token's being itself alone; and the grants of
// each scope it manages them at. "The existence of workflows they cannot read, in any listing":
// whatever a listing names beyond these is something the asker was not to learn of, and whatever
// it names past its expiry is something that no longer holds.
func (f *Fixture) lists(t testing.TB, as Asker, h Holding, lapsed bool) {
	t.Helper()
	if !h.Opens {
		for _, path := range []string{"/api/v1/runs", "/api/v1/namespaces", "/api/v1/service-accounts", "/api/v1/auth/tokens"} {
			if a := f.ask(t, t.Context(), "GET", path, as, nil); a.Status != http.StatusUnauthorized {
				t.Errorf("GET %s as %s, whose credential opens nothing, answered %d: %s", path, as.Name, a.Status, a.Body)
			}
		}
		return
	}
	var readable []string
	for _, run := range f.started {
		if h.Hold("run:read", run.Namespace, run.Workflow) {
			readable = append(readable, run.Namespace+"/"+run.Run)
		}
	}
	f.listed(t, as, "/api/v1/runs", "runs", func(e map[string]any) string { return fmt.Sprint(e["namespace"], "/", e["run"]) }, readable, false)
	for _, namespace := range []string{Finance, HR} {
		within := slices.DeleteFunc(slices.Clone(readable), func(r string) bool { return !strings.HasPrefix(r, namespace+"/") })
		f.listed(t, as, "/api/v1/"+namespace+"/runs", "runs", func(e map[string]any) string { return fmt.Sprint(e["namespace"], "/", e["run"]) }, within, false)
	}

	var seen []string
	for _, namespace := range []string{Finance, HR, "carol", "alice", "bob"} {
		if h.Sees(namespace) {
			seen = append(seen, namespace)
		}
	}
	// An administrator sees every namespace of the installation, those it was made with among
	// them, so theirs is held to include the fixture's.
	f.listed(t, as, "/api/v1/namespaces", "namespaces", func(e map[string]any) string { return fmt.Sprint(e["name"]) }, seen, h.Admin)

	var accounts []string
	for _, namespace := range h.Owns {
		accounts = append(accounts, namespace+"/agentiik")
		if namespace == Finance {
			accounts = append(accounts, NightlySync)
		}
	}
	// A namespace handed to carol may hold service accounts, and their tokens, that the fixture did
	// not make, so her listings of both are held to include what it knows of.
	handed := slices.ContainsFunc(h.Owns, func(namespace string) bool { return slices.Contains(f.Handed, namespace) })
	f.listed(t, as, "/api/v1/service-accounts", "service_accounts", func(e map[string]any) string { return fmt.Sprint(e["namespace"], "/", e["name"]) }, accounts, handed)

	// The tokens it may revoke, those "still accepted": its own, and the service accounts' of the
	// namespaces it owns, finance/nightly-sync's being the fixture's one, none revoked and none
	// past its expiry; a narrowed token lists itself alone.
	var tokens []string
	if as.Session == nil && (as.holding == "carol for finance" || strings.HasPrefix(as.holding, "alice for")) {
		tokens = []string{as.tokenID}
	} else {
		for id, m := range f.tokens {
			whose := m.principal == as.Principal || (m.principal == NightlySync && slices.Contains(h.Owns, Finance))
			if whose && !m.revoked && !(m.lapses && lapsed) {
				tokens = append(tokens, id)
			}
		}
	}
	f.listed(t, as, "/api/v1/auth/tokens", "tokens", func(e map[string]any) string { return fmt.Sprint(e["id"]) }, tokens, handed)

	// The grants and denies written at each scope it holds grant:manage on, "leaving out those
	// expired": a namespace's own, and a workflow's with its namespace's before them. Those the
	// namespace's record gave its owner are not the fixture's to name, so a listing is held to
	// holding the fixture's that apply there and last, and none that has lapsed or applies
	// elsewhere.
	for _, at := range []Target{{Namespace: Finance}, {Namespace: HR}, {Finance, Invoicing, ""}, {Finance, Payroll, ""}, {HR, Onboarding, ""}, {HR, Offboarding, ""}} {
		if !h.Hold("grant:manage", at.Namespace, at.Workflow) {
			continue
		}
		path := "/api/v1/" + at.Namespace + "/grants"
		if at.Workflow != "" {
			path = "/api/v1/" + at.Namespace + "/workflows/" + at.Workflow + "/grants"
		}
		a := f.ask(t, t.Context(), "GET", path, as, nil)
		var listing struct {
			Grants []struct {
				ID string `json:"id"`
			} `json:"grants"`
		}
		if a.Status != http.StatusOK || json.Unmarshal(a.Body, &listing) != nil {
			t.Errorf("GET %s as %s answered %d: %s", path, as.Name, a.Status, a.Body)
			continue
		}
		var listed []string
		for _, g := range listing.Grants {
			listed = append(listed, g.ID)
		}
		for _, g := range f.written {
			applies := g.scope == at.Namespace || (at.Workflow != "" && g.scope == at.Namespace+"/"+at.Workflow)
			if want := applies && !(g.lapses && lapsed); want != slices.Contains(listed, g.id) {
				t.Errorf("GET %s as %s lists %v, and %s written at %s is listed %v", path, as.Name, listed, g.id, g.scope, !want)
			}
		}
	}
}

// listed holds one listing, member names each entry of it, to want: the same set, or one holding it
// where atLeast is set. An entry named twice is counted once, since a listing of tokens names their
// principal once per token.
func (f *Fixture) listed(t testing.TB, as Asker, path, member string, key func(map[string]any) string, want []string, atLeast bool) {
	t.Helper()
	a := f.ask(t, t.Context(), "GET", path, as, nil)
	if a.Status != http.StatusOK {
		t.Errorf("GET %s as %s answered %d: %s", path, as.Name, a.Status, a.Body)
		return
	}
	var listing map[string][]map[string]any
	if err := json.Unmarshal(a.Body, &listing); err != nil {
		t.Errorf("GET %s as %s answered what is no listing: %s", path, as.Name, a.Body)
		return
	}
	var got []string
	for _, e := range listing[member] {
		if k := key(e); !slices.Contains(got, k) {
			got = append(got, k)
		}
	}
	sort.Strings(got)
	want = slices.Compact(slices.Sorted(slices.Values(want)))
	switch {
	case atLeast && !isSubset(want, got):
		t.Errorf("GET %s as %s lists %v, and leaves out some of %v", path, as.Name, got, want)
	case !atLeast && !slices.Equal(got, want):
		t.Errorf("GET %s as %s lists %v, want %v", path, as.Name, got, want)
	}
}

func isSubset(of, in []string) bool {
	for _, s := range of {
		if !slices.Contains(in, s) {
			return false
		}
	}
	return true
}
