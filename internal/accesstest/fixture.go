package accesstest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
	"github.com/agentiik/agentiik/version"
)

// The names the fixture writes, which the tests ask about.
const (
	Finance     = "finance"
	HR          = "hr"
	Invoicing   = "monthly-invoicing"
	Payroll     = "payroll"
	Onboarding  = "onboarding"
	Offboarding = "offboarding"
	TeamFinance = "team-finance"

	// NightlySync is finance's service account, as a grant names it.
	NightlySync = "finance/nightly-sync"

	// Commit is what every workflow is pushed as.
	Commit = "c4d1e7a90b3f5e2d8a6c1b9f0e7d3a5c2b8f4e61"
)

// Installation is how the fixture reaches an installation.
type Installation struct {
	// PublicURL is the installation's public URL: where every request is sent, the origin a
	// browser's requests come from, and the host its passkeys are bound to.
	PublicURL string

	// Client sends the requests, as it is, but for redirects, which the fixture answers as they
	// come: GET /api/v1/artifacts/{uri} answers one, and where it leads is not the route's answer.
	// Serve makes one of a handler in the test's own process.
	Client *http.Client

	// Bootstrap is the installation's bootstrap token, which creates the administrator and ends
	// when the administrator signs in.
	Bootstrap string

	// Now is the installation's clock, which the lapse is set an hour after.
	Now func() time.Time
}

// Serve is a client whose every request h answers in this process, from 192.0.2.1, the address
// httptest gives a request, whatever its URL names.
func Serve(h http.Handler) *http.Client {
	return &http.Client{Transport: served{h}}
}

// served is Serve's transport.
type served struct{ h http.Handler }

func (s served) RoundTrip(r *http.Request) (*http.Response, error) {
	in := r.Clone(r.Context())
	in.RemoteAddr = "192.0.2.1:1234"
	in.RequestURI = r.URL.RequestURI()
	if in.Body == nil {
		in.Body = http.NoBody
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, in)
	answered := w.Result()
	answered.Request = r
	return answered, nil
}

// Clock is an installation's clock that a test holds: the wall clock until it is held at an
// instant, and that instant from then on, however long the test takes.
type Clock struct {
	mu   sync.Mutex
	held time.Time
}

// Now is what the clock reads.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held.IsZero() {
		return c.held
	}
	return time.Now().UTC()
}

// Hold holds the clock at an instant.
func (c *Clock) Hold(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = at.UTC()
}

// Asker is one way of asking: a principal bearing an API token, a principal's browser carrying its
// session from the public URL's origin, or nobody.
type Asker struct {
	// Name is what a failure calls it: "alice's browser", "alice's token for hr".
	Name string

	// Principal is who it asks as, as a grant writes it, and empty for nobody.
	Principal string

	Bearer  string
	Session *http.Cookie

	// holding is the row of holdings saying what it holds, and tokenID the identifier of the
	// token it bears, where it bears one the fixture minted. from is the origin a browser's
	// request says it comes from, the public URL's where it is empty.
	holding, tokenID, from string
}

// Answer is what a request was answered.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// Fixture is the fixture, stood up on one installation.
type Fixture struct {
	Installation
	client *http.Client

	// The askers: each user by a token of theirs with nothing narrowed and by their browser;
	// carol by a token narrowed to finance, which carries none of an administrator's powers;
	// alice by a token narrowed to monthly-invoicing and to workflow:read and run:read, by one
	// narrowed to hr, and by one that lapses; finance/nightly-sync by its token; nobody by no
	// credential; and the bootstrap token, which ended when carol first signed in.
	Carol, CarolsBrowser, CarolForFinance                             Asker
	Alice, AlicesBrowser, AliceForInvoicing, AliceForHR, AliceLapsing Asker
	Bob, BobsBrowser                                                  Asker
	NightlySyncToken                                                  Asker
	Nobody, Bootstrap                                                 Asker

	// Lapse is when finance/nightly-sync's viewer grant, alice's deny on hr/onboarding and alice's
	// lapsing token end.
	Lapse time.Time

	// Runs are the run started of each workflow that has one, by NS/workflow.
	Runs map[string]string

	// Grants are the identifier of one grant or deny written at each scope the fixture writes
	// one, by the scope as a grant writes it: finance, finance/monthly-invoicing, finance/payroll,
	// hr and hr/onboarding.
	Grants map[string]string

	// Secrets are the name of the secret each namespace declares, and Values the values they
	// hold, which no answer may carry.
	Secrets map[string]string
	Values  []string

	// Token is the identifier of finance/nightly-sync's token.
	Token string

	// fresh numbers the things probes make to act on.
	fresh int
}

// Build stands the fixture up on an installation nothing has been done with since it was made, but
// for what its host did on its own: its bootstrap token has not ended, and none of the fixture's
// names is taken.
func Build(t testing.TB, in Installation) *Fixture {
	t.Helper()
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	f := &Fixture{
		Installation: in,
		client: &http.Client{
			Transport: client.Transport, Timeout: client.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Lapse:   in.Now().Add(time.Hour).Truncate(time.Second).UTC(),
		Runs:    map[string]string{},
		Grants:  map[string]string{},
		Secrets: map[string]string{},
	}
	f.Nobody = Asker{Name: "nobody", holding: "nobody"}
	f.Bootstrap = Asker{Name: "the bootstrap token", Principal: "operator", Bearer: in.Bootstrap, holding: "nobody"}

	// carol, made an administrator by the bootstrap token, which her first sign-in ends.
	carol := f.user(t, f.Bootstrap, "carol", true)
	f.CarolsBrowser = Asker{Name: "carol's browser", Principal: "carol", Session: carol, holding: "carol"}
	f.Carol = f.mint(t, f.CarolsBrowser, "carol", api.TokenRequest{DeviceLabel: "carol's laptop"})
	f.Carol.holding = "carol"
	f.must(t, "GET", "/api/v1/me", f.Bootstrap, nil, http.StatusUnauthorized)

	// alice and bob, made by carol.
	f.AlicesBrowser = Asker{Name: "alice's browser", Principal: "alice", Session: f.user(t, f.Carol, "alice", false), holding: "alice"}
	f.BobsBrowser = Asker{Name: "bob's browser", Principal: "bob", Session: f.user(t, f.Carol, "bob", false), holding: "bob"}
	f.Alice = f.mint(t, f.AlicesBrowser, "alice", api.TokenRequest{DeviceLabel: "alice's laptop"})
	f.Alice.holding = "alice"
	f.Bob = f.mint(t, f.BobsBrowser, "bob", api.TokenRequest{DeviceLabel: "bob's laptop"})
	f.Bob.holding = "bob"

	// The two namespaces, finance owned by carol and hr by bob, and team-finance, alice in it.
	f.must(t, "POST", "/api/v1/namespaces", f.Carol, api.NamespaceRecord{Name: Finance, Owner: "carol"}, http.StatusCreated)
	f.must(t, "POST", "/api/v1/namespaces", f.Carol, api.NamespaceRecord{Name: HR, Owner: "bob"}, http.StatusCreated)
	f.must(t, "POST", "/api/v1/groups", f.Carol, api.NewGroup{Name: TeamFinance, Members: []string{"alice"}}, http.StatusCreated)

	// finance/nightly-sync, and a token of it, minted by carol, who owns finance.
	f.must(t, "POST", "/api/v1/service-accounts", f.Carol, api.NewServiceAccount{Namespace: Finance, Name: "nightly-sync"}, http.StatusCreated)
	f.NightlySyncToken = f.mint(t, f.Carol, NightlySync, api.TokenRequest{Principal: NightlySync, DeviceLabel: "the nightly job"})
	f.NightlySyncToken.Name, f.NightlySyncToken.holding = NightlySync, NightlySync
	f.Token = f.NightlySyncToken.tokenID

	// The workflows, each pushed by its namespace's owner.
	for _, w := range []struct {
		by                  Asker
		namespace, workflow string
	}{{f.Carol, Finance, Invoicing}, {f.Carol, Finance, Payroll}, {f.Bob, HR, Onboarding}, {f.Bob, HR, Offboarding}} {
		f.must(t, "PUT", "/api/v1/"+w.namespace+"/workflows/"+w.workflow+"/versions/"+Commit, w.by, Pushed(t, w.namespace, w.workflow), http.StatusOK)
	}

	// The grants and denies, each written by whoever the page lets write it: carol owns finance
	// and bob owns hr.
	for _, g := range []struct {
		by                    Asker
		scope                 string
		principal, role, deny string
		lapses                bool
	}{
		{f.Carol, Finance, "group:" + TeamFinance, "editor", "", false},
		{f.Carol, Finance + "/" + Invoicing, "alice", "", "run:read_data", false},
		{f.Carol, Finance + "/" + Payroll, NightlySync, "operator", "", false},
		{f.Carol, Finance, NightlySync, "viewer", "", true},
		{f.Bob, HR, "group:" + TeamFinance, "", "workflow:run", false},
		{f.Bob, HR + "/" + Onboarding, "alice", "editor", "", false},
		{f.Bob, HR + "/" + Onboarding, "alice", "", "workflow:read", true},
	} {
		q := api.GrantRequest{Principal: g.principal, Role: g.role, Deny: g.deny}
		if g.lapses {
			q.ExpiresAt = &f.Lapse
		}
		id := f.grant(t, g.by, g.scope, q)
		if _, ok := f.Grants[g.scope]; !ok {
			f.Grants[g.scope] = id
		}
	}

	// A secret of each namespace, declared by whoever may write one there: alice, whom
	// team-finance makes an editor of finance, and bob, who owns hr.
	for _, s := range []struct {
		by              Asker
		namespace, name string
	}{{f.Alice, Finance, "billing"}, {f.Bob, HR, "payslips"}} {
		value := "sk_" + s.namespace + "_" + randomHex(t)
		f.must(t, "PUT", "/api/v1/"+s.namespace+"/secrets/"+s.name, s.by, api.Declare{Provider: "builtin", Value: &value}, http.StatusCreated)
		f.Secrets[s.namespace] = s.name
		f.Values = append(f.Values, value)
	}

	// A run of monthly-invoicing started by alice, one of payroll started by finance/nightly-sync,
	// which operates it, and one of onboarding started by bob.
	for _, r := range []struct {
		by                  Asker
		namespace, workflow string
	}{{f.Alice, Finance, Invoicing}, {f.NightlySyncToken, Finance, Payroll}, {f.Bob, HR, Onboarding}} {
		started := f.must(t, "POST", "/api/v1/"+r.namespace+"/workflows/"+r.workflow+"/runs", r.by,
			api.Start{Commit: Commit, Inputs: map[string]any{"orders": []any{}}}, http.StatusAccepted)
		var run struct {
			Run string `json:"run"`
		}
		decode(t, started, &run)
		f.Runs[r.namespace+"/"+r.workflow] = run.Run
	}

	// The narrowed tokens, minted from the browsers, and alice's token that lapses.
	f.CarolForFinance = f.mint(t, f.CarolsBrowser, "carol", api.TokenRequest{
		DeviceLabel: "finance's terminal", Scope: &api.TokenScope{Within: []string{Finance}},
	})
	f.CarolForFinance.Name, f.CarolForFinance.holding = "carol's token for finance", "carol for finance"
	f.AliceForInvoicing = f.mint(t, f.AlicesBrowser, "alice", api.TokenRequest{
		DeviceLabel: "the invoicing dashboard",
		Scope:       &api.TokenScope{Permissions: []string{"workflow:read", "run:read"}, Within: []string{Finance + "/" + Invoicing}},
	})
	f.AliceForInvoicing.Name, f.AliceForInvoicing.holding = "alice's token for monthly-invoicing", "alice for monthly-invoicing"
	f.AliceForHR = f.mint(t, f.AlicesBrowser, "alice", api.TokenRequest{DeviceLabel: "hr's terminal", Scope: &api.TokenScope{Within: []string{HR}}})
	f.AliceForHR.Name, f.AliceForHR.holding = "alice's token for hr", "alice for hr"
	f.AliceLapsing = f.mint(t, f.AlicesBrowser, "alice", api.TokenRequest{DeviceLabel: "a borrowed laptop", ExpiresAt: &f.Lapse})
	f.AliceLapsing.Name, f.AliceLapsing.holding = "alice's token that lapses", "alice until the lapse"
	return f
}

// Askers are the fixture's askers holding a credential of a principal, in the order Fixture
// declares them: nobody and the bootstrap token are not among them.
func (f *Fixture) Askers() []Asker {
	return []Asker{
		f.Carol, f.CarolsBrowser, f.CarolForFinance,
		f.Alice, f.AlicesBrowser, f.AliceForInvoicing, f.AliceForHR, f.AliceLapsing,
		f.Bob, f.BobsBrowser, f.NightlySyncToken,
	}
}

// codeIn is the code an enrolment link carries after its #, which a browser never sends.
var codeIn = regexp.MustCompile(`#(agkenrol_[A-Za-z0-9_-]+)$`)

// user creates a user as by, and signs them in for the first time from their enrolment link, as the
// enrolment page does: a passkey registered where the installation is addressed by a name, and a
// password set where it is addressed by an IP address, where no passkey signs anybody in. Either
// opens a full session, which it answers.
func (f *Fixture) user(t testing.TB, by Asker, login string, admin bool) *http.Cookie {
	t.Helper()
	var created struct {
		Enrolment api.EnrolmentLink `json:"enrolment"`
	}
	decode(t, f.must(t, "POST", "/api/v1/users", by, api.NewUser{Login: login, Admin: admin}, http.StatusCreated), &created)
	m := codeIn.FindStringSubmatch(created.Enrolment.Link)
	if m == nil {
		t.Fatalf("%s's enrolment link %q carries no code", login, created.Enrolment.Link)
	}
	code := m[1]
	if f.addressedByIP() {
		return sessionOf(t, f.must(t, "POST", "/api/v1/auth/password/enrol", f.Nobody,
			map[string]string{"code": code, "password": "the access fixture's passphrase for " + login}, http.StatusOK))
	}
	var options struct {
		Options json.RawMessage `json:"options"`
	}
	decode(t, f.must(t, "POST", "/api/v1/auth/passkey/options", f.Nobody,
		map[string]string{"ceremony": "registration", "code": code}, http.StatusOK), &options)
	made, _, err := webauthntest.New(strings.TrimSuffix(f.PublicURL, "/")).Create(options.Options)
	if err != nil {
		t.Fatalf("%s's authenticator refused the registration: %s", login, err)
	}
	return sessionOf(t, f.must(t, "POST", "/api/v1/auth/passkey/verify", f.Nobody,
		map[string]any{"ceremony": "registration", "credential": made}, http.StatusOK))
}

// addressedByIP says whether the public URL's host is an IP address, where the policy is applied
// with passwords allowed and no passkey required.
func (f *Fixture) addressedByIP() bool {
	u, err := url.Parse(f.PublicURL)
	return err == nil && net.ParseIP(u.Hostname()) != nil
}

// sessionOf is the session an answer opened.
func sessionOf(t testing.TB, a Answer) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: a.Header}).Cookies() {
		if c.Name == api.SessionCookie && c.Value != "" {
			return &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	t.Fatalf("the answer %d %s opened no session", a.Status, a.Body)
	return nil
}

// mint mints an API token as from asks it, and answers the asker bearing it, whose principal is
// principal.
func (f *Fixture) mint(t testing.TB, from Asker, principal string, q api.TokenRequest) Asker {
	t.Helper()
	var issued api.IssuedToken
	decode(t, f.must(t, "POST", "/api/v1/auth/tokens", from, q, http.StatusCreated), &issued)
	if issued.Token == "" || issued.APIToken.Principal != principal {
		t.Fatalf("minting a token of %s answered %+v", principal, issued.APIToken)
	}
	return Asker{Name: principal, Principal: principal, Bearer: issued.Token, tokenID: issued.APIToken.ID}
}

// grant writes one grant or deny at scope, NS or NS/workflow, as by, and answers its identifier.
func (f *Fixture) grant(t testing.TB, by Asker, scope string, q api.GrantRequest) string {
	t.Helper()
	path := "/api/v1/" + scope + "/grants"
	if namespace, workflow, ok := strings.Cut(scope, "/"); ok {
		path = "/api/v1/" + namespace + "/workflows/" + workflow + "/grants"
	}
	var written struct {
		ID string `json:"id"`
	}
	decode(t, f.must(t, "POST", path, by, q, http.StatusCreated), &written)
	if written.ID == "" {
		t.Fatalf("the grant written at %s answered no identifier", scope)
	}
	return written.ID
}

// The workflow every workflow of the fixture is a version of, under its own name, and the brick it
// runs, named by digest as a push records it.
const (
	image = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

	workflowDocument = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: %s, namespace: %s }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: normalize, port: ok } }
steps:
  normalize:
    image: ` + image + `
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
`

	manifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`
)

// Pushed is the push of the fixture's workflow as namespace/workflow, as agk push would send it.
func Pushed(t testing.TB, namespace, workflow string) api.Push {
	t.Helper()
	return pushOf(t, fmt.Sprintf(workflowDocument, workflow, namespace))
}

// PushedNaming is Pushed of a version whose one step names secret, which the workflow declares.
func PushedNaming(t testing.TB, namespace, workflow, secret string) api.Push {
	t.Helper()
	document := fmt.Sprintf(workflowDocument, workflow, namespace)
	document = strings.Replace(document, "steps:\n", "secrets: ["+secret+"]\nsteps:\n", 1)
	document = strings.Replace(document, "    outputs: [ok, rejected]\n", "    outputs: [ok, rejected]\n    secrets: ["+secret+"]\n", 1)
	return pushOf(t, document)
}

// pushOf is the push of a workflow document running the fixture's brick.
func pushOf(t testing.TB, document string) api.Push {
	t.Helper()
	m, err := brick.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(document)}}
	v, err := version.Capture(tree, "agentiik.yaml", map[string]brick.Manifest{image: m})
	if err != nil {
		t.Fatal(err)
	}
	return api.Push{
		Entry: v.Entry, Document: v.Document, Includes: v.Includes, Manifests: v.Manifests, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}},
	}
}

// Ask sends one request as as, with body sent as it is where it is a string and as its JSON
// otherwise, and answers what came back. A browser's request comes from the public URL's origin, as
// the sign-in page's and the console's do, and so does a request carrying no credential, which is
// how the enrolment page asks.
//
// An answer cut by ctx is what arrived before, a log stream's among them, which answers until its
// reader goes.
func (f *Fixture) Ask(ctx context.Context, method, path string, as Asker, body any) (Answer, error) {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return Answer{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	r, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(f.PublicURL, "/")+path, reader)
	if err != nil {
		return Answer{}, err
	}
	if reader != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	switch {
	case as.Bearer != "":
		r.Header.Set("Authorization", "Bearer "+as.Bearer)
	case as.from != "":
		r.Header.Set("Origin", as.from)
	default:
		r.Header.Set("Origin", f.origin())
	}
	if as.Session != nil {
		r.AddCookie(as.Session)
	}
	answered, err := f.client.Do(r)
	if err != nil {
		return Answer{}, err
	}
	defer answered.Body.Close()
	read, err := io.ReadAll(io.LimitReader(answered.Body, 16<<20))
	if err != nil && ctx.Err() == nil {
		return Answer{}, err
	}
	return Answer{Status: answered.StatusCode, Header: answered.Header, Body: read}, nil
}

// origin is the public URL's origin, which a browser's requests carry.
func (f *Fixture) origin() string {
	u, err := url.Parse(f.PublicURL)
	if err != nil {
		return f.PublicURL
	}
	return u.Scheme + "://" + u.Host
}

// ask is Ask, failing the test where the request could not be made.
func (f *Fixture) ask(t testing.TB, ctx context.Context, method, path string, as Asker, body any) Answer {
	t.Helper()
	a, err := f.Ask(ctx, method, path, as, body)
	if err != nil {
		t.Fatalf("%s %s as %s: %s", method, path, as.Name, err)
	}
	return a
}

// must is ask, failing the test on any status but status.
func (f *Fixture) must(t testing.TB, method, path string, as Asker, body any, status int) Answer {
	t.Helper()
	a := f.ask(t, t.Context(), method, path, as, body)
	if a.Status != status {
		t.Fatalf("%s %s as %s answered %d, want %d: %s", method, path, as.Name, a.Status, status, a.Body)
	}
	return a
}

// decode reads an answer's body into into.
func decode(t testing.TB, a Answer, into any) {
	t.Helper()
	if err := json.Unmarshal(a.Body, into); err != nil {
		t.Fatalf("the answer %s does not decode: %s", a.Body, err)
	}
}

// randomHex is sixteen random bytes, written in hexadecimal.
func randomHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
