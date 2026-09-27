package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
)

// A refusal that takes longer for what exists than for what does not says which exists as plainly
// as a 403 would: "an inaccessible workflow answering the same 404 as an absent one, so that probing
// yields nothing" holds of how long the answer takes as well as of what it says. What the router and
// the listing ask the authorizer is the same, question for question, whether what a request names is
// there or not, which a timing measured against a real database follows: see
// cmd/agentiik-api's TestAnAbsentNameAndAnInvisibleOneTakeAsLongToRefuse.

// counted answers as its Authorizer does and counts what it is asked one target at a time, keeping
// the last target, and, as countedAmong, several as one question.
type counted struct {
	api.Authorizer
	one, many atomic.Int32

	mu   sync.Mutex
	last api.Target
}

func (c *counted) Allow(ctx context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	c.one.Add(1)
	c.mu.Lock()
	c.last = over
	c.mu.Unlock()
	return c.Authorizer.Allow(ctx, who, what, over)
}

// lastAsked is the target counted was last asked about.
func (c *counted) lastAsked() api.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// countedAmong is counted implementing api.Among.
type countedAmong struct{ *counted }

func (c countedAmong) AllowAmong(ctx context.Context, who api.Principal, what api.Permission, over []api.Target) ([]bool, error) {
	c.many.Add(1)
	held := make([]bool, len(over))
	for i, target := range over {
		allowed, err := c.Authorizer.Allow(ctx, who, what, target)
		if err != nil {
			return nil, err
		}
		held[i] = allowed
	}
	return held, nil
}

// narrowing identifies as bearer does, and a caller named NAME@NS as NAME bearing a token narrowed
// to the namespace NS.
func narrowing(r *http.Request) (api.Identity, error) {
	as, err := bearer(r)
	if name, within, ok := strings.Cut(string(as.Principal), "@"); ok {
		as.Principal = api.Principal(name)
		as.Scope = access.TokenScope{Within: []access.Scope{{Namespace: within}}}
	}
	return as, err
}

// A route naming a run is refused after the question a run that is there is asked, whether the run
// is not there, is of a workflow the caller holds nothing on, is under another namespace than its
// path names, is outside what the caller's token reaches, or is named by a URI that does not parse:
// one question each, about a workflow of a namespace, the path's where it names one, so that none is
// answered sooner than the others. Before, a run that was not there and one outside a token's reach
// were refused with no question at all, a lookup sooner than a run the caller could not read. And a
// question that could not be answered is a 500 whether the run is there or not.
func TestARouteNamingARunAsksTheSameQuestionWhateverTheRunIs(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	auth := &counted{Authorizer: granted{"alice": {{api.RunRead, invoicing}, {api.RunReadData, invoicing}}}}
	rt, err := api.NewRouter(auth, narrowing)
	if err != nil {
		t.Fatal(err)
	}
	const (
		mine      = "01M2Z8V1P9C4XQ7K2N4D6F8H0C"
		unread    = "01M2Z8V1P9C4XQ7K2N4D6F8H0D"
		elsewhere = "01M2Z8V1P9C4XQ7K2N4D6F8H0E"
		absent    = "01M2ZZZZZZZZZZZZZZZZZZZZZZ"
	)
	rt.ServeRuns(&runsOf{of: map[string]api.Target{
		mine: invoicing, unread: {Namespace: "finance", Workflow: "payroll"},
		elsewhere: {Namespace: "team-ops", Workflow: "monthly-invoicing"},
	}})
	ok := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
		w.WriteHeader(http.StatusOK)
	}
	rt.MustHandle("GET", "/api/v1/runs/{run}", api.OnRun{Permission: api.RunRead}, ok)
	rt.MustHandle("GET", "/api/v1/{namespace}/runs/{run}", api.OnRun{Permission: api.RunRead}, ok)
	rt.MustHandle("GET", "/api/v1/artifacts/{uri}", api.OnArtifact{Permission: api.RunReadData}, ok)
	artifact := func(run string) string {
		return "/api/v1/artifacts/" + url.PathEscape("agk://run/"+run+"/normalize/ok/invoice.pdf")
	}

	for _, c := range []struct {
		who     string
		allowed string
		refused []string
	}{
		{"alice", "/api/v1/runs/" + mine, []string{
			"/api/v1/runs/" + unread, "/api/v1/runs/" + elsewhere, "/api/v1/runs/" + absent,
			"/api/v1/finance/runs/" + unread, "/api/v1/finance/runs/" + elsewhere, "/api/v1/finance/runs/" + absent,
			"/api/v1/team-ops/runs/" + mine, "/api/v1/nowhere/runs/" + absent,
			artifact(unread), artifact(elsewhere), artifact(absent), "/api/v1/artifacts/not-a-uri",
		}},
		{"alice@finance", "/api/v1/finance/runs/" + mine, []string{
			"/api/v1/runs/" + unread, "/api/v1/runs/" + elsewhere, "/api/v1/runs/" + absent,
			"/api/v1/team-ops/runs/" + elsewhere, "/api/v1/team-ops/runs/" + absent, artifact(elsewhere), artifact(absent),
		}},
	} {
		before := auth.one.Load()
		if code, body := reached(t, rt, "GET", c.allowed, c.who); code != http.StatusOK {
			t.Fatalf("%s: %s answered %d %s", c.who, c.allowed, code, body)
		}
		asked := auth.one.Load() - before
		if asked == 0 {
			t.Fatalf("%s: %s was let through with no question asked", c.who, c.allowed)
		}
		for _, path := range c.refused {
			before := auth.one.Load()
			if code, _ := reached(t, rt, "GET", path, c.who); code != http.StatusNotFound {
				t.Errorf("%s: %s answered %d", c.who, path, code)
			}
			if got := auth.one.Load() - before; got != asked {
				t.Errorf("%s: %s was refused after %d questions, and a run the caller reads is answered after %d", c.who, path, got, asked)
			}
			// A workflow of a namespace, since a question about the installation, or a namespace
			// alone, reads less than one about a run's workflow does.
			over := auth.lastAsked()
			if over.Namespace == "" || over.Workflow == "" {
				t.Errorf("%s: %s was refused after a question about %+v, which is no workflow", c.who, path, over)
			}
			if ns, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/v1/"), "/"); ns != "runs" && ns != "artifacts" && over.Namespace != ns {
				t.Errorf("%s: %s was refused after a question about %+v, outside the namespace its path names", c.who, path, over)
			}
		}
	}

	// An authorizer that cannot answer is a 500 for a run that is there and for one that is not.
	broken := &counted{Authorizer: holder{broke: true}}
	rt, err = api.NewRouter(broken, narrowing)
	if err != nil {
		t.Fatal(err)
	}
	rt.ServeRuns(&runsOf{of: map[string]api.Target{mine: invoicing}})
	rt.MustHandle("GET", "/api/v1/runs/{run}", api.OnRun{Permission: api.RunRead}, ok)
	for _, path := range []string{"/api/v1/runs/" + mine, "/api/v1/runs/" + absent} {
		if code, _ := reached(t, rt, "GET", path, "alice"); code != http.StatusInternalServerError {
			t.Errorf("with the authorizer failing, %s answered %d", path, code)
		}
	}
}

// A listing's question about many targets is one question, of an authorizer answering several at
// once, whether it names none of them or many, narrowed by the caller's token as each one-target
// question is; of an authorizer answering one at a time it is one question per target; and a target
// outside the namespace the path names, or naming none, is an error before anything is asked.
func TestAListingAsksAboutEveryTargetAsOneQuestion(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	onboarding := api.Target{Namespace: "hr", Workflow: "onboarding"}
	allowed := granted{"alice": {{api.RunRead, invoicing}, {api.RunRead, onboarding}}}

	for _, c := range []struct {
		name  string
		among bool
	}{{"answering several at once", true}, {"answering one at a time", false}} {
		auth := &counted{Authorizer: allowed}
		var authorizer api.Authorizer = auth
		if c.among {
			authorizer = countedAmong{auth}
		}
		rt, err := api.NewRouter(authorizer, narrowing)
		if err != nil {
			t.Fatal(err)
		}
		var held []bool
		var failed error
		var over []api.Target
		rt.MustHandleAcross("GET", "/api/v1/runs", api.Across{Permission: api.RunRead},
			func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target, _ api.Holds) {
				held, failed = api.HoldsEach(r)(r.Context(), over)
				w.WriteHeader(http.StatusOK)
			})
		rt.MustHandleAcross("GET", "/api/v1/{namespace}/runs", api.Across{Permission: api.RunRead},
			func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target, _ api.Holds) {
				held, failed = api.HoldsEach(r)(r.Context(), over)
				w.WriteHeader(http.StatusOK)
			})

		for _, q := range []struct {
			who, path string
			over      []api.Target
			want      []bool
		}{
			{"alice", "/api/v1/runs", nil, []bool{}},
			{"alice", "/api/v1/runs", []api.Target{invoicing, payroll, onboarding}, []bool{true, false, true}},
			{"alice@finance", "/api/v1/runs", []api.Target{invoicing, payroll, onboarding}, []bool{true, false, false}},
			{"alice", "/api/v1/finance/runs", []api.Target{invoicing, payroll}, []bool{true, false}},
			{"alice", "/api/v1/nowhere/runs", nil, []bool{}},
		} {
			over = q.over
			one, many := auth.one.Load(), auth.many.Load()
			if code, body := reached(t, rt, "GET", q.path, q.who); code != http.StatusOK || failed != nil {
				t.Fatalf("%s: %s as %s answered %d %s, failing with %v", c.name, q.path, q.who, code, body, failed)
			}
			if !slices.Equal(held, q.want) {
				t.Errorf("%s: %s as %s held %v of %v, want %v", c.name, q.path, q.who, held, q.over, q.want)
			}
			gotOne, gotMany := auth.one.Load()-one, auth.many.Load()-many
			if c.among && (gotMany != 1 || gotOne != 0) {
				t.Errorf("%s: %s as %s about %d targets asked %d questions of several and %d of one, and it is one question", c.name, q.path, q.who, len(q.over), gotMany, gotOne)
			}
			if !c.among && (gotMany != 0 || int(gotOne) != len(q.over)) {
				t.Errorf("%s: %s as %s about %d targets asked %d questions of one", c.name, q.path, q.who, len(q.over), gotOne)
			}
		}

		for _, q := range []struct{ path string }{{"/api/v1/finance/runs"}, {"/api/v1/runs"}} {
			over = []api.Target{invoicing, onboarding, {}}
			if q.path == "/api/v1/runs" {
				over = []api.Target{invoicing, {}}
			}
			one, many := auth.one.Load(), auth.many.Load()
			if reached(t, rt, "GET", q.path, "alice"); failed == nil {
				t.Errorf("%s: %s asked about %v and was answered %v", c.name, q.path, over, held)
			}
			if auth.one.Load() != one || auth.many.Load() != many {
				t.Errorf("%s: %s asked the authorizer about a target it may not ask about", c.name, q.path)
			}
		}
	}

	// A request the router did not serve is answered no about everything.
	r := httptest.NewRequest("GET", "/api/v1/runs", nil)
	held, err := api.HoldsEach(r)(r.Context(), []api.Target{{Namespace: "finance", Workflow: "payroll"}})
	if err != nil || len(held) != 1 || held[0] {
		t.Errorf("a request the router did not serve was answered %v, %v", held, err)
	}
}

// A listing of runs asks about every workflow it could list as one question, and asks it whether
// there is one workflow, two or none, the namespace nobody created included: refusing a listing of
// finance, which holds two workflows, takes what refusing one of a namespace that does not exist
// takes. Before, each workflow was a question of its own, and a listing of a namespace holding
// twenty took ten times as long to refuse as one of a namespace nobody created.
func TestAListingOfRunsAsksOneQuestionWhateverItLists(t *testing.T) {
	s := withSomeRuns(t)
	auth := &counted{Authorizer: granted{"alice": {{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}}}}}
	h := s.servedTo(t, countedAmong{auth})
	for _, c := range []struct {
		path   string
		listed int
	}{
		{"/api/v1/finance/runs", 1},
		{"/api/v1/nowhere/runs", 0},
		{"/api/v1/team-ops/runs", 0},
		{"/api/v1/runs", 1},
		{"/api/v1/runs?namespace=nowhere", 0},
		{"/api/v1/runs?namespace=finance&workflow=monthly-invoicing", 0},
	} {
		one, many := auth.one.Load(), auth.many.Load()
		if listed := listedAt(t, h, "alice", c.path); len(listed) != c.listed {
			t.Errorf("%s listed %v", c.path, listed)
		}
		if gotOne, gotMany := auth.one.Load()-one, auth.many.Load()-many; gotOne != 0 || gotMany != 1 {
			t.Errorf("%s asked %d questions of one target and %d of several, and a listing asks one of several", c.path, gotOne, gotMany)
		}
	}
}

// What Principals answers about several targets as one question is what it answers about each of
// them, for every principal it knows and every permission, at the installation, in a namespace, in
// one workflow, where no grant could name, and for the bootstrap token before and after it ends.
func TestPrincipalsAnswerSeveralTargetsAsTheyAnswerEach(t *testing.T) {
	in := somePrincipals(t)
	in.withBootstrap(t, "agk_op_among")
	targets := []api.Target{
		installationTarget, financeTarget, invoicingTarget, payrollTarget, onboardingTarget,
		{Namespace: "hr"}, {Namespace: "nowhere", Workflow: "nothing"}, {Namespace: "runs"}, {Namespace: "%ff"},
	}
	check := func(when string) {
		for _, who := range []api.Principal{"alice", "bob", "carol", "dave", "finance/nightly", "nobody", api.BootstrapOperator, ""} {
			for _, what := range append(slices.Clone(api.Permissions), "run:everything") {
				held, err := in.p.AllowAmong(t.Context(), who, what, targets)
				if err != nil {
					t.Fatalf("%s: asking about %s: %s", when, who, err)
				}
				if len(held) != len(targets) {
					t.Fatalf("%s: %s was answered about %d targets of %d", when, who, len(held), len(targets))
				}
				for i, over := range targets {
					if each := in.holds(t, who, what, over); held[i] != each {
						t.Errorf("%s: %s %s over %+v is %v asked with the others and %v alone", when, who, what, over, held[i], each)
					}
				}
			}
		}
	}
	check("while the bootstrap lasts")
	in.endBootstrap(t)
	check("once the bootstrap has ended")
	if held, err := in.p.AllowAmong(t.Context(), "alice", api.RunRead, nil); err != nil || len(held) != 0 {
		t.Errorf("asked about nothing, alice was answered %v, %v", held, err)
	}
}

// "Artifacts from another namespace. A presigned URL covers one artifact of one run and expires in
// minutes." The URL an artifact of finance is redirected to is signed for its namespace, its run and
// its digest, and checked for all three when it is followed: the same bytes held by team-ops, which
// the namespace is the one segment of the key to tell apart, another object of finance, and the same
// object under another run are each refused, as the URL with nothing edited is not.
func TestAnArtifactsURLOpensThatObjectOfThatNamespaceForThatRun(t *testing.T) {
	s := withSomeRuns(t)
	content := []byte("invoice 2026-01, the same bytes in two namespaces")
	u := s.anArtifact(t, s.finance[0], "invoice.pdf", 0, content)
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	other := []byte("another invoice of finance")
	otherSum := sha256.Sum256(other)
	for key, bytesOf := range map[string][]byte{
		artifact.Key("team-ops", digest):                         content,
		artifact.Key("finance", hex.EncodeToString(otherSum[:])): other,
	} {
		if err := s.objects.Put(t.Context(), key, bytes.NewReader(bytesOf)); err != nil {
			t.Fatal(err)
		}
	}
	h := s.servedTo(t, granted{"alice": {{api.RunReadData, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}}})

	w, _ := call(t, h, "GET", artifactPath(u), "alice", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("the artifact answered %d: %s", w.Code, w.Body)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if followed, _ := call(t, h, "GET", location.RequestURI(), "", nil); followed.Code != http.StatusOK || followed.Body.String() != string(content) {
		t.Fatalf("the URL as it was signed answered %d %q", followed.Code, followed.Body)
	}
	edited := func(path string, run string) string {
		q := location.Query()
		q.Set("run", run)
		return path + "?" + q.Encode()
	}
	for what, uri := range map[string]string{
		"the same bytes in team-ops":              edited(strings.Replace(location.Path, "/objects/finance/", "/objects/team-ops/", 1), s.finance[0]),
		"another object of finance":               edited(strings.Replace(location.Path, digest, hex.EncodeToString(otherSum[:]), 1), s.finance[0]),
		"the same object for another run":         edited(location.Path, s.finance[1]),
		"the same object for a run of team-ops's": edited(location.Path, s.teamOps),
	} {
		if w, _ := call(t, h, "GET", uri, "", nil); w.Code != http.StatusForbidden || w.Body.Len() != 0 {
			t.Errorf("%s, through the URL signed for finance's artifact, answered %d %q", what, w.Code, w.Body)
		}
	}
}

// What Principals costs to answer about several targets as one question is the same whatever the
// targets and however many, none, one or a hundred across namespaces that exist and namespaces that
// do not: who the principal is, read once, and its grants, read once. It is what one target costs
// Allow, so that a listing takes what a route about one workflow takes, and what a run that is not
// there costs to refuse is what one that is costs. Asked a target at a time, a hundred targets were
// two hundred transactions.
func TestPrincipalsAnswerManyTargetsAtTheCostOfOne(t *testing.T) {
	in := somePrincipals(t)
	var many []api.Target
	for i := range 100 {
		many = append(many, api.Target{Namespace: []string{"finance", "hr", "nowhere"}[i%3], Workflow: fmt.Sprintf("workflow-%d", i)})
	}
	one := func(ask func()) int64 {
		before := api.Questions(in.p)
		ask()
		return api.Questions(in.p) - before
	}
	for _, who := range []api.Principal{"alice", "finance/nightly", "carol"} {
		single := one(func() { in.holds(t, who, api.RunRead, invoicingTarget) })
		for _, over := range [][]api.Target{nil, {invoicingTarget}, {{Namespace: "nowhere", Workflow: "nothing"}}, many} {
			cost := one(func() {
				if _, err := in.p.AllowAmong(t.Context(), who, api.RunRead, over); err != nil {
					t.Fatal(err)
				}
			})
			if cost != single {
				t.Errorf("%s: asked about %d targets as one question, Principals opened %d transactions, and one target costs %d", who, len(over), cost, single)
			}
		}
	}
}
