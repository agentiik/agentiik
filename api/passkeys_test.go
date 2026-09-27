package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
)

// The passkey ceremonies, through a router built as serve builds it, over a real PostgreSQL, with a
// software authenticator answering the options the API issues as a browser would: the first
// administrator enrolling from the link the bootstrap token made them, which ends the token; a
// sign-in, which gives a user their personal namespace the first time; and every refusal a
// ceremony owes.

// ceremonies is an installation with no user yet, its bootstrap token set, serving the user routes,
// the passkey ceremonies, the password routes and the authentication policy on
// https://agentiik.example.com, on a clock the test moves.
type ceremonies struct {
	pool      *db.Pool
	super     string
	clock     *time.Time
	h         http.Handler
	bootstrap string
	policies  *api.PolicyAPI
}

func someCeremonies(t *testing.T) ceremonies {
	return ceremoniesOn(t, "https://agentiik.example.com")
}

func ceremoniesOn(t *testing.T, publicURL string) ceremonies {
	t.Helper()
	pool, super := dbtest.Open(t)
	now := time.Now().UTC().Truncate(time.Second)
	in := ceremonies{pool: pool, super: super, clock: &now, bootstrap: "agk_op_" + ulid.New()}
	clock := func() time.Time { return *in.clock }
	p, err := api.NewPrincipals(pool, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AcceptSessions(publicURL); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(p, p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewPasskeys(rt, api.PasskeyOptions{
		Pool: pool, PublicURL: publicURL, Identify: p.Identify, Now: clock,
		Trouble: func(err error) { t.Errorf("trouble: %s", err) },
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: pool, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewPasswords(rt, api.PasswordOptions{
		Pool: pool, PublicURL: publicURL, Identify: p.Identify, Now: clock,
		Trouble: func(err error) { t.Errorf("trouble: %s", err) },
	}); err != nil {
		t.Fatal(err)
	}
	if in.policies, err = api.NewPolicies(rt, api.PolicyOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	in.h = rt
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.SetBootstrapToken(ctx, hashOf(in.bootstrap))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return in
}

// newBrowser is a browser on the sign-in page, with a device-bound authenticator that verifies its
// user and counts its signatures.
func newBrowser() *webauthntest.Authenticator { return webauthntest.New(publicOrigin) }

// call sends one request as the sign-in page does, with fetch, from the public URL's origin, from
// the address from where it is not empty, carrying the cookies given.
func (in ceremonies) call(t *testing.T, method, path, body, from string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", publicOrigin)
	if from != "" {
		r.RemoteAddr = from
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// bearer sends one request bearing a token, as agk does.
func (in ceremonies) bearer(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sent(t, in.h, method, path, token, body)
}

// user creates login with the bootstrap token, an administrator where admin is set, and answers
// the code their link carries.
func (in ceremonies) user(t *testing.T, login string, admin bool) string {
	t.Helper()
	w := in.bearer(t, "POST", "/api/v1/users", in.bootstrap, fmt.Sprintf(`{"login":%q,"admin":%t}`, login, admin))
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("creating %s answered %d %s", login, w.Code, w.Body)
	}
	var created api.CreatedUser
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return codeOf(t, created.Enrolment.Link)
}

// optionsAnswer is openapi.json's passkeyOptions, the options kept as written.
type optionsAnswer struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

// options starts a ceremony with body and answers its options, failing the test unless they are
// given.
func (in ceremonies) options(t *testing.T, body string, cookies ...*http.Cookie) []byte {
	t.Helper()
	w := in.call(t, "POST", "/api/v1/auth/passkey/options", body, "", cookies...)
	if w.Code != http.StatusOK {
		t.Fatalf("the options %s answered %d %s", body, w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the options are answered with Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	var o optionsAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	return o.Options
}

// verify finishes a ceremony with the credential given, and a label where it is not empty.
func (in ceremonies) verify(t *testing.T, ceremony string, c webauthntest.Credential, label, from string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"ceremony": ceremony, "credential": c}
	if label != "" {
		body["label"] = label
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return in.call(t, "POST", "/api/v1/auth/passkey/verify", string(b), from, cookies...)
}

// enrol registers a passkey on browser with the enrolment code, and answers the verification.
func (in ceremonies) enrol(t *testing.T, browser *webauthntest.Authenticator, code, label string) *httptest.ResponseRecorder {
	t.Helper()
	made, _, err := browser.Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	return in.verify(t, "registration", made, label, "")
}

// signIn runs an assertion with browser's newest passkey, and answers the verification.
func (in ceremonies) signIn(t *testing.T, browser *webauthntest.Authenticator) *httptest.ResponseRecorder {
	t.Helper()
	got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	return in.verify(t, "assertion", got, "", "")
}

// junk sends an assertion that answers no challenge, from the address from, presenting the
// credential ID id: what anybody may send, which signs nobody in.
func (in ceremonies) junk(t *testing.T, from, id string) {
	t.Helper()
	client := fmt.Sprintf(`{"type":"webauthn.get","challenge":"%s","origin":%q}`,
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)), publicOrigin)
	c := webauthntest.Credential{ID: id, RawID: id, Type: "public-key", Response: webauthntest.Response{
		ClientDataJSON: base64.RawURLEncoding.EncodeToString([]byte(client)), AuthenticatorData: "AAAA", Signature: "AAAA", UserHandle: "AAAA",
	}}
	if w := in.verify(t, "assertion", c, "", from); w.Code != http.StatusUnauthorized {
		t.Fatalf("a sign-in answering no challenge answered %d %s", w.Code, w.Body)
	}
}

// session is the session cookie an answer set, failing the test where it set none.
func session(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	t.Fatalf("the answer %d %s set no session", w.Code, w.Body)
	return nil
}

// query scans one row the database answers, behind every policy.
func (in ceremonies) query(t *testing.T, query string, into ...any) {
	t.Helper()
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), query).Scan(into...); err != nil {
		t.Fatalf("%s: %s", query, err)
	}
}

// count is one number the database answers.
func (in ceremonies) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	in.query(t, query, &n)
	return n
}

// exec runs statements behind every policy.
func (in ceremonies) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
}

// actions are the audit log's entries, as actor action target, in order.
func (in ceremonies) actions(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select actor || ' ' || action || ' ' || target from audit_log order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		all = append(all, s)
	}
	return all
}

// The first administrator enrols from the link the bootstrap token made them, on the options the
// OpenAPI document shows, and the registration signs them in, gives them their personal namespace
// and ends the bootstrap token, which is a 401 from then on; then they sign in with the passkey
// they made, which opens another session and records the counter it reported.
func TestTheFirstAdministratorEnrolsFromTheLinkAndSignsInWithThePasskey(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "alice", true)

	raw := in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code))
	var o struct {
		RP struct {
			ID, Name string
		} `json:"rp"`
		User struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"user"`
		Challenge        string `json:"challenge"`
		PubKeyCredParams []struct {
			Type string `json:"type"`
			Alg  int    `json:"alg"`
		} `json:"pubKeyCredParams"`
		Timeout                int              `json:"timeout"`
		ExcludeCredentials     []map[string]any `json:"excludeCredentials"`
		AuthenticatorSelection map[string]any   `json:"authenticatorSelection"`
		Attestation            string           `json:"attestation"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	handle, _ := base64.RawURLEncoding.DecodeString(o.User.ID)
	challenge, _ := base64.RawURLEncoding.DecodeString(o.Challenge)
	var algs []int
	for _, p := range o.PubKeyCredParams {
		if p.Type == "public-key" {
			algs = append(algs, p.Alg)
		}
	}
	switch {
	case o.RP.ID != "agentiik.example.com" || o.RP.Name == "":
		t.Errorf("the Relying Party is %+v, and it is the public URL's host", o.RP)
	case o.User.Name != "alice" || o.User.DisplayName != "alice" || len(handle) != 32 || strings.Contains(string(handle), "alice"):
		t.Errorf("the user is %+v, a handle of 32 random bytes naming nobody", o.User)
	case len(challenge) != 32 || o.Timeout != 300000:
		t.Errorf("the challenge is %d bytes and lives %d ms", len(challenge), o.Timeout)
	case !slices.Equal(algs, []int{-7, -8, -257}):
		t.Errorf("the algorithms offered are %v", algs)
	case o.ExcludeCredentials == nil || len(o.ExcludeCredentials) != 0:
		t.Errorf("a user holding no passkey is asked to exclude %v", o.ExcludeCredentials)
	case o.AuthenticatorSelection["residentKey"] != "required" || o.AuthenticatorSelection["requireResidentKey"] != true || o.AuthenticatorSelection["userVerification"] != "required":
		t.Errorf("the authenticator is asked for %v, a discoverable credential verifying its user", o.AuthenticatorSelection)
	case o.Attestation != "none":
		t.Errorf("the attestation asked for is %q", o.Attestation)
	}

	browser := newBrowser()
	made, passkey, err := browser.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	w := in.verify(t, "registration", made, "work laptop", "192.0.2.7:4431")
	if w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	var enrolled api.Verified
	if err := json.Unmarshal(w.Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	if c := enrolled.Credential; enrolled.Ceremony != "registration" || enrolled.Login != "alice" || c == nil ||
		c.ID != made.ID || c.Label != "work laptop" || c.Kind != "device-bound" || c.BackupEligible || c.BackupState || c.CreatedAt.IsZero() {
		t.Errorf("the registration answered %s", w.Body)
	}
	first := session(t, w)

	// The bootstrap token ended with the enrolment, and the session opened is a full one.
	if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "ended when the first administrator signed in") {
		t.Errorf("the bootstrap token after the first administrator enrolled answered %d %s", w.Code, w.Body)
	}
	listing := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/users", nil)
	listing.AddCookie(first)
	listed := httptest.NewRecorder()
	if in.h.ServeHTTP(listed, listing); listed.Code != http.StatusOK {
		t.Errorf("the session the registration opened reads the users as %d %s", listed.Code, listed.Body)
	}
	var count, aaguid int
	var label string
	var eligible, state bool
	in.query(t, `select sign_count, octet_length(aaguid), label, backup_eligible, backup_state from credentials where id = '`+made.ID+`'`,
		&count, &aaguid, &label, &eligible, &state)
	if count != 0 || aaguid != 16 || label != "work laptop" || eligible || state {
		t.Errorf("the passkey is stored with the counter %d, an AAGUID of %d bytes, label %q, BE %v, BS %v", count, aaguid, label, eligible, state)
	}
	if n := in.count(t, `select count(*) from enrolment_codes where used_at is not null`); n != 1 {
		t.Errorf("%d codes were spent", n)
	}
	if n := in.count(t, `select count(*) from bootstrap where enrolled_at is not null and token_hash is null`); n != 1 {
		t.Error("the bootstrap state does not record the first administrator's enrolment")
	}
	var kind, owner string
	in.query(t, `select kind, owner from namespaces where name = 'alice'`, &kind, &owner)
	if kind != "personal" || owner != "alice" ||
		in.count(t, `select count(*) from grants where namespace = 'alice' and principal = 'alice' and role = 'owner' and granted_by = 'installation'`) != 1 ||
		in.count(t, `select count(*) from service_accounts where namespace = 'alice' and name = 'agentiik'`) != 1 {
		t.Errorf("the personal namespace is %s owned by %s, with its owner's grant and built-in identity missing", kind, owner)
	}
	want := []string{
		"operator user.create alice", "operator enrolment.issue alice",
		"alice credential.enrol " + made.ID, "alice enrolment.use alice", "alice bootstrap.end operator",
	}
	got := in.actions(t)
	if len(got) != 8 || !slices.Equal(got[:5], want) || !strings.HasPrefix(got[5], "installation grant.create ") ||
		got[6] != "installation namespace.create alice" || got[7] != "alice signin.succeed alice" {
		t.Errorf("the audit log reads %q", got)
	}

	// A sign-in: the options name nobody, and the passkey names alice.
	raw = in.options(t, `{"ceremony":"assertion"}`)
	var asked map[string]any
	if err := json.Unmarshal(raw, &asked); err != nil {
		t.Fatal(err)
	}
	if asked["rpId"] != "agentiik.example.com" || asked["userVerification"] != "required" || asked["timeout"] != 300000.0 ||
		asked["allowCredentials"] == nil || len(asked["allowCredentials"].([]any)) != 0 {
		t.Errorf("an assertion's options are %v", asked)
	}
	got2, err := browser.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	w = in.verify(t, "assertion", got2, "", "")
	if w.Code != http.StatusOK || w.Body.String() != "{\"ceremony\":\"assertion\",\"login\":\"alice\"}\n" {
		t.Fatalf("the sign-in answered %d %s", w.Code, w.Body)
	}
	if second := session(t, w); second.Value == first.Value {
		t.Error("the sign-in answered the session the registration opened")
	}
	var used time.Time
	in.query(t, `select sign_count, last_used_at from credentials where id = '`+made.ID+`'`, &count, &used)
	if count != int(passkey.Count) || count != 1 || !used.Equal(*in.clock) {
		t.Errorf("the sign-in recorded the counter %d, at %s", count, used)
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'alice'`); n != 1 {
		t.Errorf("alice has %d personal namespaces after her second sign-in", n)
	}
	if got := in.actions(t); len(got) != 9 || got[8] != "alice signin.succeed alice" {
		t.Errorf("the second sign-in is recorded as %q", got[8:])
	}
}

// A user an administrator creates enrols from their link and is signed in, and the bootstrap token
// goes on working, since only the first administrator's enrolment ends it. Their personal namespace
// is made at that first sign-in and never again.
func TestAUsersPersonalNamespaceIsMadeAtTheirFirstSignInAndOnlyThen(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)
	browser := newBrowser()
	if w := in.enrol(t, browser, code, ""); w.Code != http.StatusOK {
		t.Fatalf("bob's registration answered %d %s", w.Code, w.Body)
	}
	if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusOK {
		t.Errorf("the bootstrap token after a user who is not the first administrator enrolled answered %d %s", w.Code, w.Body)
	}
	for range 2 {
		if w := in.signIn(t, browser); w.Code != http.StatusOK {
			t.Fatalf("bob's sign-in answered %d %s", w.Code, w.Body)
		}
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'bob' and kind = 'personal'`); n != 1 {
		t.Errorf("bob has %d personal namespaces", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'namespace.create'`); n != 1 {
		t.Errorf("%d personal namespaces were recorded as created", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.succeed' and target = 'bob'`); n != 3 {
		t.Errorf("bob's three sign-ins are recorded %d times", n)
	}
	var signedIn time.Time
	in.query(t, `select last_sign_in_at from users where login = 'bob'`, &signedIn)
	if !signedIn.Equal(*in.clock) {
		t.Errorf("bob last signed in at %s", signedIn)
	}
}

// A challenge answers one verification, and none past its five minutes: the same answer sent twice
// signs in once, and an answer sent late signs nobody in. A registration's challenge does not
// answer an assertion. Each refusal is the one sentence.
func TestAChallengeIsTakenOnceAndLapsesAfterFiveMinutes(t *testing.T) {
	in := someCeremonies(t)
	browser := newBrowser()
	if w := in.enrol(t, browser, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}

	got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "assertion", got, "", ""); w.Code != http.StatusOK {
		t.Fatalf("the sign-in answered %d %s", w.Code, w.Body)
	}
	if w := in.verify(t, "assertion", got, "", ""); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "signs nobody in") {
		t.Errorf("the same answer sent again answered %d %s", w.Code, w.Body)
	}

	late, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	*in.clock = in.clock.Add(db.ChallengeLife)
	if w := in.verify(t, "assertion", late, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("an answer five minutes after its options answered %d %s", w.Code, w.Body)
	}

	// A registration's challenge, answered as an assertion.
	code := in.user(t, "carol", false)
	crossed, err := browser.GetWith(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)), browser.Passkeys()[0])
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "assertion", crossed, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("an assertion answering a registration's challenge answered %d %s", w.Code, w.Body)
	}
	// The lapsed challenge went when the next was issued, and the others as they were answered.
	if n := in.count(t, `select count(*) from webauthn_challenges`); n != 0 {
		t.Errorf("%d challenges are kept after every one was answered or lapsed", n)
	}
	var reasons []string
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select detail::jsonb->>'reason' from audit_log where action = 'signin.fail' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, r)
	}
	if len(reasons) != 3 || !strings.Contains(reasons[0], "answered already") || !strings.Contains(reasons[1], "lapsed") || !strings.Contains(reasons[2], "registration") {
		t.Errorf("the refusals are recorded for %q", reasons)
	}
}

// A ceremony run on another origin, or for another Relying Party, is refused, and so is a request
// that does not come from the sign-in page: a form posted from another site, or from a sandboxed
// frame that sends Origin: null, could otherwise sign a browser in as somebody else.
func TestACeremonyOfAnotherOriginOrRelyingPartyIsRefused(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)

	phished := webauthntest.New("https://agentiik.example.com.evil.example")
	made, _, err := phished.Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "registration", made, "", ""); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "was not registered") {
		t.Errorf("a registration run on another origin answered %d %s", w.Code, w.Body)
	}
	otherRP := newBrowser()
	otherRP.RPID = "evil.example"
	if made, _, err = otherRP.Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code))); err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "registration", made, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a registration for another Relying Party answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials`); n != 0 {
		t.Errorf("%d passkeys were stored from the refused registrations", n)
	}

	browser := newBrowser()
	if w := in.enrol(t, browser, code, ""); w.Code != http.StatusOK {
		t.Fatalf("the code refused twice registers nothing it should not, and then %d %s", w.Code, w.Body)
	}
	browser.Origin = "https://evil.example"
	if w := in.signIn(t, browser); w.Code != http.StatusUnauthorized {
		t.Errorf("a sign-in run on another origin answered %d %s", w.Code, w.Body)
	}
	browser.Origin, browser.RPID = publicOrigin, "evil.example"
	if w := in.signIn(t, browser); w.Code != http.StatusUnauthorized {
		t.Errorf("a sign-in for another Relying Party answered %d %s", w.Code, w.Body)
	}

	for name, origin := range map[string]string{"another site": "https://evil.example", "a sandboxed frame": "null", "no page at all": ""} {
		for _, path := range []string{"/api/v1/auth/passkey/options", "/api/v1/auth/passkey/verify"} {
			r := httptest.NewRequestWithContext(t.Context(), "POST", path, strings.NewReader(`{"ceremony":"assertion"}`))
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			w := httptest.NewRecorder()
			in.h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Origin header") {
				t.Errorf("%s asking %s answered %d %s", name, path, w.Code, w.Body)
			}
		}
	}
}

// A signature counter that did not move forward refuses the sign-in, stores nothing of it, records
// signin.fail with the reason and tells the passkey's user in a notification, and does not lock
// the passkey: the next sign-in counting on from the stored counter goes through. Both are written
// whatever the bound on failed sign-ins says, which anybody sending junk from the same address, a
// proxy's, could otherwise fill first.
func TestACounterThatDidNotMoveForwardRefusesTheSignInAndTellsTheUser(t *testing.T) {
	in := someCeremonies(t)
	browser := newBrowser()
	if w := in.enrol(t, browser, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	for range 3 {
		if w := in.signIn(t, browser); w.Code != http.StatusOK {
			t.Fatalf("the sign-in answered %d %s", w.Code, w.Body)
		}
	}
	passkey := browser.Passkeys()[0]
	id := base64.RawURLEncoding.EncodeToString(passkey.ID)
	var used time.Time
	in.query(t, `select last_used_at from credentials where id = '`+id+`'`, &used)
	*in.clock = in.clock.Add(time.Minute)

	for range failuresPerAddress {
		in.junk(t, "192.0.2.1:1234", "AAAA")
	}
	passkey.Count = 1 // a copy of the key, whose counter is behind
	if w := in.signIn(t, browser); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "signs nobody in") {
		t.Fatalf("a sign-in whose counter went back answered %d %s", w.Code, w.Body)
	}
	var count int
	var still time.Time
	in.query(t, `select sign_count, last_used_at from credentials where id = '`+id+`'`, &count, &still)
	if count != 3 || !still.Equal(used) {
		t.Errorf("the refused sign-in left the counter at %d, last used at %s", count, still)
	}
	var actor, target, reason, credential string
	in.query(t, `select actor, target, detail::jsonb->>'reason', detail::jsonb->>'credential' from audit_log where action = 'signin.fail' and target = 'bob'`,
		&actor, &target, &reason, &credential)
	if actor != "192.0.2.1" || target != "bob" || !strings.Contains(reason, "signature counter did not move forward") || credential != id {
		t.Errorf("the refusal is recorded as %s %s %q %s", actor, target, reason, credential)
	}
	var kind, told string
	var at time.Time
	in.query(t, `select kind, credential, at from notifications where recipient = 'bob'`, &kind, &told, &at)
	if kind != "passkey_counter_refused" || told != id || !at.Equal(*in.clock) {
		t.Errorf("bob is told %s about %s at %s", kind, told, at)
	}
	if n := in.count(t, `select count(*) from sessions where login = 'bob'`); n != 4 {
		t.Errorf("bob holds %d sessions, and the refused sign-in opened none", n)
	}

	passkey.Count = 3
	w := in.signIn(t, browser)
	if w.Code != http.StatusOK {
		t.Fatalf("the passkey was locked: a sign-in counting on from the stored counter answered %d %s", w.Code, w.Body)
	}
	me := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/me", nil)
	me.AddCookie(session(t, w))
	read := httptest.NewRecorder()
	in.h.ServeHTTP(read, me)
	var answer struct {
		Notifications []struct {
			Kind, Credential string
		} `json:"notifications"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &answer); err != nil || read.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/me answered %d %s", read.Code, read.Body)
	}
	if len(answer.Notifications) != 1 || answer.Notifications[0].Kind != "passkey_counter_refused" || answer.Notifications[0].Credential != id {
		t.Errorf("GET /api/v1/me tells bob %s", read.Body)
	}
}

// The user handle an assertion hands back names the account its passkey was registered under, and
// one naming another, or none, signs nobody in.
func TestAnAssertionsUserHandleIsItsAccounts(t *testing.T) {
	in := someCeremonies(t)
	browser := newBrowser()
	if w := in.enrol(t, browser, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	got.Response.UserHandle = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if w := in.verify(t, "assertion", got, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("an assertion naming another user handle answered %d %s", w.Code, w.Body)
	}
	if got, err = browser.Get(in.options(t, `{"ceremony":"assertion"}`)); err != nil {
		t.Fatal(err)
	}
	got.Response.UserHandle = ""
	if w := in.verify(t, "assertion", got, "", ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "userHandle") {
		t.Errorf("an assertion naming no user handle answered %d %s", w.Code, w.Body)
	}
}

// An installation addressed by an IP address runs no ceremony, since a browser refuses an IP
// address as a Relying Party Identifier, and both routes say so; localhost runs them.
func TestAnInstallationAddressedByAnIPAddressRunsNoCeremony(t *testing.T) {
	for _, publicURL := range []string{"https://192.0.2.10:8443", "https://[2001:db8::10]"} {
		in := ceremoniesOn(t, publicURL)
		for _, path := range []string{"/api/v1/auth/passkey/options", "/api/v1/auth/passkey/verify"} {
			r := httptest.NewRequestWithContext(t.Context(), "POST", path, strings.NewReader(`{"ceremony":"assertion"}`))
			r.Header.Set("Origin", strings.TrimSuffix(publicURL, ":443"))
			w := httptest.NewRecorder()
			in.h.ServeHTTP(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "addressed by an IP address") {
				t.Errorf("%s on %s answered %d %s", path, publicURL, w.Code, w.Body)
			}
		}
		if n := in.count(t, `select count(*) from webauthn_challenges`); n != 0 {
			t.Errorf("%d challenges were issued on %s", n, publicURL)
		}
	}

	in := ceremoniesOn(t, "https://localhost:8443")
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/passkey/options", strings.NewReader(`{"ceremony":"assertion"}`))
	r.Header.Set("Origin", "https://localhost:8443")
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rpId":"localhost"`) {
		t.Errorf("an installation on localhost answered %d %s", w.Code, w.Body)
	}
}

// An enrolment code is spent by the registration that records the passkey, not by the options: a
// ceremony dismissed halfway leaves the link working, and once a registration has spent it, it
// starts nothing more. A link replaced by a fresher one before the verification registers nothing.
func TestAnEnrolmentCodeIsSpentByTheRegistrationItStarted(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)
	in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)) // dismissed
	browser := newBrowser()
	if w := in.enrol(t, browser, code, ""); w.Code != http.StatusOK {
		t.Fatalf("a registration after one dismissed answered %d %s", w.Code, w.Body)
	}
	w := in.call(t, "POST", "/api/v1/auth/passkey/options", fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code), "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "opens nothing") {
		t.Errorf("a spent code answered %d %s", w.Code, w.Body)
	}

	code = in.user(t, "carol", false)
	made, _, err := newBrowser().Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	if w := in.bearer(t, "POST", "/api/v1/users/carol/enrolment", in.bootstrap, ""); w.Code != http.StatusCreated {
		t.Fatalf("a fresh link answered %d %s", w.Code, w.Body)
	}
	if w := in.verify(t, "registration", made, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a registration from a replaced link answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'carol'`); n != 0 {
		t.Errorf("carol holds %d passkeys from a replaced link", n)
	}
}

// A signed-in user registers another passkey from their session, one the options exclude the
// first from, and the registration opens no other session. Without a session or a code, or with a
// bearer token, nothing is registered.
func TestASignedInUserRegistersAnotherPasskeyFromTheirSession(t *testing.T) {
	in := someCeremonies(t)
	phone := newBrowser()
	w := in.enrol(t, phone, in.user(t, "bob", false), "phone")
	if w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	signedIn := session(t, w)

	raw := in.options(t, `{"ceremony":"registration"}`, signedIn)
	if !strings.Contains(string(raw), base64.RawURLEncoding.EncodeToString(phone.Passkeys()[0].ID)) {
		t.Errorf("the options exclude nothing bob holds: %s", raw)
	}
	if _, _, err := phone.Create(raw); err == nil {
		t.Error("the authenticator holding bob's passkey made a second for him")
	}
	laptop := newBrowser()
	made, _, err := laptop.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "registration", made, "laptop", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a registration a session started, finished without it, answered %d %s", w.Code, w.Body)
	}
	if made, _, err = laptop.Create(in.options(t, `{"ceremony":"registration"}`, signedIn)); err != nil {
		t.Fatal(err)
	}
	w = in.verify(t, "registration", made, "laptop", "", signedIn)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Errorf("a registration from a session answered %d %s, setting %v", w.Code, w.Body, w.Result().Cookies())
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob'`); n != 2 {
		t.Errorf("bob holds %d passkeys", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = 'bob'`); n != 2 {
		t.Errorf("%d of bob's passkeys are recorded as enrolled", n)
	}

	token := "agk_test_bob_" + ulid.New()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		return wide.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hashOf(token), Principal: "bob", CreatedAt: *in.clock, ExpiresAt: in.clock.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*http.Request{
		"nothing":        httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/passkey/options", strings.NewReader(`{"ceremony":"registration"}`)),
		"a bearer token": httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/passkey/options", strings.NewReader(`{"ceremony":"registration"}`)),
	} {
		r.Header.Set("Origin", publicOrigin)
		if name == "a bearer token" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "carries neither") {
			t.Errorf("a registration carrying %s answered %d %s", name, w.Code, w.Body)
		}
	}
}

// Starting a ceremony removes challenges past their minutes, and skips one another transaction
// holds rather than waiting for it: a ceremony anybody may start never waits on another's.
func TestStartingACeremonyWaitsOnNoOtherCeremony(t *testing.T) {
	in := someCeremonies(t)
	in.exec(t, `insert into webauthn_challenges (challenge, ceremony, issued_at, expires_at)
	            values ('\x`+strings.Repeat("ab", 32)+`', 'assertion', now() - interval '1 hour', now() - interval '55 minutes')`)
	holder, err := dbtest.Superuser(t, in.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.WithoutCancel(t.Context()))
	if _, err := holder.Exec(t.Context(), `select from webauthn_challenges for update`); err != nil {
		t.Fatal(err)
	}
	answered := make(chan int, 1)
	go func() {
		answered <- in.call(t, "POST", "/api/v1/auth/passkey/options", `{"ceremony":"assertion"}`, "").Code
	}()
	select {
	case code := <-answered:
		if code != http.StatusOK {
			t.Errorf("the options answered %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the options waited on another transaction holding a lapsed challenge")
	}
}

// The policy's user_verification is asked for and held to: required by default, so an
// authenticator that did not verify its user registers nothing; preferred, and it does.
// device_bound_only refuses a synced passkey with 403 naming the setting, and spends no code.
func TestThePolicyDecidesUserVerificationAndSyncedPasskeys(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)
	careless := newBrowser()
	careless.UserVerified = false
	if w := in.enrol(t, careless, code, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a registration without user verification answered %d %s", w.Code, w.Body)
	}
	in.exec(t, `update auth_policy set user_verification = 'preferred' where namespace is null`)
	raw := in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code))
	if !strings.Contains(string(raw), `"userVerification":"preferred"`) {
		t.Errorf("the options under a policy preferring user verification ask for %s", raw)
	}
	made, _, err := careless.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "registration", made, "", ""); w.Code != http.StatusOK {
		t.Errorf("a registration without user verification, which the policy prefers, answered %d %s", w.Code, w.Body)
	}

	// A namespace bob holds a grant in requires it again, and asks for device-bound passkeys.
	in.exec(t,
		`insert into namespaces (name) values ('finance')`,
		`insert into grants (id, namespace, principal, role, granted_by) values ('`+ulid.New()+`', 'finance', 'bob', 'viewer', 'carol')`,
		`insert into auth_policy (namespace, user_verification, device_bound_only) values ('finance', 'required', true)`)
	if w := in.signIn(t, careless); w.Code != http.StatusUnauthorized {
		t.Errorf("a sign-in without user verification, which bob's namespace requires, answered %d %s", w.Code, w.Body)
	}
	if !strings.Contains(string(in.options(t, `{"ceremony":"assertion"}`)), `"userVerification":"required"`) {
		t.Error("an assertion's options ask for less than a namespace's policy requires")
	}

	code = in.user(t, "carol", false)
	in.exec(t, `insert into grants (id, namespace, principal, role, granted_by) values ('`+ulid.New()+`', 'finance', 'carol', 'viewer', 'bob')`)
	synced := newBrowser()
	synced.BackupEligible, synced.BackedUp = true, true
	w := in.enrol(t, synced, code, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"device_bound_only"`) {
		t.Errorf("a synced passkey where device_bound_only applies answered %d %s", w.Code, w.Body)
	}
	if w := in.enrol(t, newBrowser(), code, ""); w.Code != http.StatusOK {
		t.Errorf("the code a refused synced passkey left answered %d %s", w.Code, w.Body)
	}
}

// A suspended user enrols with a code, since enrolling is how an account suspended for having no
// passkey comes back, and opens no session while the suspension lasts: neither the registration
// nor a sign-in signs them in, and a suspended first administrator's does not end the bootstrap.
// Once the suspension is lifted, the first sign-in is an assertion, and that gives the user their
// personal namespace.
func TestASuspendedUserEnrolsAndOpensNoSession(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)
	in.exec(t, `update users set suspended = true where login = 'bob'`)
	browser := newBrowser()
	w := in.enrol(t, browser, code, "")
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Fatalf("a suspended user's registration answered %d %s, setting %v", w.Code, w.Body, w.Result().Cookies())
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'bob'`); n != 0 {
		t.Error("a registration that signed nobody in made a personal namespace")
	}
	if w := in.signIn(t, browser); w.Code != http.StatusUnauthorized {
		t.Errorf("a suspended user's sign-in answered %d %s", w.Code, w.Body)
	}
	var reason string
	in.query(t, `select detail::jsonb->>'reason' from audit_log where action = 'signin.fail'`, &reason)
	if reason != "the account is suspended" {
		t.Errorf("the refusal is recorded for %q", reason)
	}

	// A suspended first administrator's enrolment leaves the bootstrap token working, since
	// ending it would leave nobody who can sign in to administer the installation.
	first := in.user(t, "alice", true)
	in.exec(t, `update users set suspended = true where login = 'alice'`)
	if w := in.enrol(t, newBrowser(), first, ""); w.Code != http.StatusOK {
		t.Fatalf("a suspended first administrator's registration answered %d %s", w.Code, w.Body)
	}
	if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusOK {
		t.Errorf("the bootstrap token after a suspended first administrator enrolled answered %d %s", w.Code, w.Body)
	}

	in.exec(t, `update users set suspended = false where login = 'bob'`)
	if w := in.signIn(t, browser); w.Code != http.StatusOK {
		t.Fatalf("bob's sign-in once his suspension was lifted answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'bob' and kind = 'personal' and owner = 'bob'`); n != 1 {
		t.Error("bob's first sign-in, an assertion, gave him no personal namespace")
	}
}

// The bound on failed sign-ins, as the API holds it, for the tests: ten entries from one address and
// a hundred from all of them in ten minutes.
const (
	failuresPerAddress = 10
	failuresAll        = 100
)

// What failed sign-ins append to the audit log is bounded: ten from one address in ten minutes, and
// a hundred from every address together, so that a sender with many addresses is held too. Past
// either, a refusal is answered all the same, and counted in the next entry appended. A credential
// ID naming no passkey is recorded cut short, since the sender wrote it.
func TestFailedSignInsAreBoundedInTheAuditLog(t *testing.T) {
	in := someCeremonies(t)
	for range failuresPerAddress + 3 {
		in.junk(t, "198.51.100.4:50000", "AAAA")
	}
	in.junk(t, "[2001:db8:7:1::1]:443", "AAAA")
	in.junk(t, "[2001:db8:7:1::2]:443", "AAAA")
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.fail' and actor = '198.51.100.4'`); n != failuresPerAddress {
		t.Errorf("thirteen failures from one address appended %d entries", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.fail' and actor like '2001:db8:7:1::%'`); n != 2 {
		t.Errorf("two failures from another address appended %d entries", n)
	}
	for i := range 10 {
		for range failuresPerAddress {
			in.junk(t, fmt.Sprintf("203.0.113.%d:443", i+1), "AAAA")
		}
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.fail'`); n != failuresAll {
		t.Errorf("a hundred and fifteen failures from twelve addresses appended %d entries", n)
	}

	*in.clock = in.clock.Add(10 * time.Minute)
	long := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 1000))
	in.junk(t, "198.51.100.9:50001", long)
	var target, credential, unrecorded string
	in.query(t, `select target, detail::jsonb->>'credential', detail::jsonb->>'unrecorded' from audit_log where action = 'signin.fail' order by seq desc limit 1`,
		&target, &credential, &unrecorded)
	// Three went unrecorded before the first failure from the second address, which says so, and
	// twelve before this one.
	if total := in.count(t, `select sum((detail::jsonb->>'unrecorded')::int) from audit_log where action = 'signin.fail'`); unrecorded != "12" || total != 15 {
		t.Errorf("the first entry past the window counts %s unrecorded failures before it, and the log %d in all, where 15 went unrecorded", unrecorded, total)
	}
	if target != long[:64] || credential != long[:64] {
		t.Errorf("a credential ID of %d characters naming no passkey is recorded as %d and %d", len(long), len(target), len(credential))
	}
}

// What the verification reads of a body: a member a newer browser adds to the credential is skipped,
// and the rest is held to the schema, a label beside an assertion, a code beside an assertion, an
// ID that is not its raw ID and agk login's terminal, which this release does not read, refused.
func TestACeremonysBodyIsHeldToItsSchema(t *testing.T) {
	in := someCeremonies(t)
	code := in.user(t, "bob", false)
	browser := newBrowser()
	made, _, err := browser.Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(made)
	if err != nil {
		t.Fatal(err)
	}
	newer := `{"ceremony":"registration","credential":` + strings.Replace(string(b), `"type":"public-key"`, `"type":"public-key","hints":["security-key"],"response2":{"a":[1,2,{}]}`, 1) + `}`
	if w := in.call(t, "POST", "/api/v1/auth/passkey/verify", newer, ""); w.Code != http.StatusOK {
		t.Errorf("a credential carrying members a newer browser adds answered %d %s", w.Code, w.Body)
	}

	for body, want := range map[string]string{
		`{"ceremony":"assertion","code":"agkenrol_` + strings.Repeat("A", 43) + `"}`: "carries no enrolment code",
		`{"ceremony":"signing"}`:                              "ceremony:",
		`{"ceremony":"registration","code":"agkenrol_short"}`: "code:",
	} {
		if w := in.call(t, "POST", "/api/v1/auth/passkey/options", body, ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
			t.Errorf("the options %s answered %d %s", body, w.Code, w.Body)
		}
	}
	got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"ceremony":"assertion","label":"x","credential":` + string(a) + `}`:                                                           "label:",
		`{"ceremony":"assertion","credential":` + strings.Replace(string(a), `"id":"`, `"id":"A`, 1) + `}`:                              "credential.id",
		`{"ceremony":"assertion","credential":` + string(a) + `,"terminal":{"redirect_uri":"http://127.0.0.1:1/","code_challenge":""}}`: "terminal",
		`{"ceremony":"assertion","credential":null}`:                                                                                    "no credential",
		`{"ceremony":"registration","label":"a\nb","credential":` + string(b) + `}`:                                                     "control character",
	} {
		if w := in.call(t, "POST", "/api/v1/auth/passkey/verify", body, ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
			t.Errorf("the verification %.80s answered %d %s", body, w.Code, w.Body)
		}
	}
}

// The first administrator's enrolment and a fresh first administrator's link issued at the same
// moment take turns rather than each waiting on what the other holds: the link, which asked for the
// bootstrap state first, revokes the code the enrolment would have spent, and the enrolment finds it
// revoked and registers nothing. Taken in the other order, the enrolment would hold its code while
// the link waited on it, and wait on the bootstrap state the link held, which PostgreSQL ends by
// failing one of them.
//
// The bootstrap state is held until both wait on it, the link first, so that the order does not
// depend on the scheduler.
func TestTheFirstAdministratorsEnrolmentAndAFreshLinkTakeTurns(t *testing.T) {
	in := someCeremonies(t)
	in.user(t, "bob", true)
	code := in.user(t, "alice", true)
	made, _, err := newBrowser().Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := dbtest.Superuser(t, in.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(t.Context(), `select from bootstrap for update`); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) error {
		for deadline := time.Now().Add(10 * time.Second); ; {
			if in.count(t, `select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`) >= n {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%d transactions were expected to wait on a lock", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	link, enrolment := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { link <- in.bearer(t, "POST", "/api/v1/users/bob/enrolment", in.bootstrap, "") }()
	err = waiting(1)
	if err == nil {
		go func() { enrolment <- in.verify(t, "registration", made, "", "") }()
		err = waiting(2)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if w := <-link; w.Code != http.StatusCreated {
		t.Errorf("the fresh link answered %d %s", w.Code, w.Body)
	}
	if w := <-enrolment; w.Code != http.StatusUnauthorized {
		t.Errorf("the enrolment from the code the fresh link revoked answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials`) + in.count(t, `select count(*) from bootstrap where enrolled_at is not null`); n != 0 {
		t.Error("the enrolment from a revoked code registered a passkey or ended the bootstrap")
	}
}

// device_bound_only refuses a synced passkey at sign-in as it does at registration, with 403 naming
// the setting, and the refusal is recorded whatever the bound on failed sign-ins says, since only the
// passkey's holder can make it. The Backup State an assertion reports is recorded, as it may change
// between sign-ins.
func TestASyncedPasskeyIsRefusedAtSignInWhereDeviceBoundOnlyApplies(t *testing.T) {
	in := someCeremonies(t)
	synced := newBrowser()
	synced.BackupEligible = true
	if w := in.enrol(t, synced, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	id := base64.RawURLEncoding.EncodeToString(synced.Passkeys()[0].ID)
	synced.BackedUp = true
	if w := in.signIn(t, synced); w.Code != http.StatusOK {
		t.Fatalf("the sign-in answered %d %s", w.Code, w.Body)
	}
	var state bool
	in.query(t, `select backup_state from credentials where id = '`+id+`'`, &state)
	if !state {
		t.Error("the Backup State the sign-in reported was not recorded")
	}

	in.exec(t, `update auth_policy set device_bound_only = true where namespace is null`)
	for range failuresPerAddress {
		in.junk(t, "192.0.2.1:1234", "AAAA")
	}
	w := in.signIn(t, synced)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"device_bound_only"`) {
		t.Errorf("a synced passkey's sign-in where device_bound_only applies answered %d %s", w.Code, w.Body)
	}
	var reason string
	in.query(t, `select detail::jsonb->>'reason' from audit_log where action = 'signin.fail' and target = 'bob'`, &reason)
	if !strings.Contains(reason, "device_bound_only") {
		t.Errorf("the refusal is recorded for %q", reason)
	}
}

// A code decides over a session: the page an enrolment link opens enrols the link's user whoever
// last signed in in that browser, and the registration signs the browser in as them.
func TestAnEnrolmentCodeDecidesOverTheSessionBesideIt(t *testing.T) {
	in := someCeremonies(t)
	w := in.enrol(t, newBrowser(), in.user(t, "bob", false), "")
	if w.Code != http.StatusOK {
		t.Fatalf("bob's registration answered %d %s", w.Code, w.Body)
	}
	bobs := session(t, w)
	code := in.user(t, "carol", false)
	raw := in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code), bobs)
	if !strings.Contains(string(raw), `"name":"carol"`) {
		t.Errorf("options asked with carol's code in bob's browser are for %s", raw)
	}
	made, _, err := newBrowser().Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	w = in.verify(t, "registration", made, "", "", bobs)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"login":"carol"`) {
		t.Fatalf("carol's registration in bob's browser answered %d %s", w.Code, w.Body)
	}
	session(t, w)
	if n := in.count(t, `select count(*) from credentials where login = 'carol'`); n != 1 {
		t.Errorf("carol holds %d passkeys", n)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob'`); n != 1 {
		t.Errorf("bob holds %d passkeys", n)
	}
}

// A namespace's policy tightens the sign-in of an account holding a grant in it, and a deny alone
// is not one: an account holding nothing there but a deny signs in under the installation's policy.
func TestADenyAloneBringsNoNamespacesPolicyToASignIn(t *testing.T) {
	in := someCeremonies(t)
	in.exec(t, `update auth_policy set user_verification = 'preferred' where namespace is null`)
	careless := newBrowser()
	careless.UserVerified = false
	if w := in.enrol(t, careless, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	in.exec(t,
		`insert into namespaces (name) values ('finance')`,
		`insert into grants (id, namespace, principal, deny, granted_by) values ('`+ulid.New()+`', 'finance', 'bob', 'run:read_data', 'carol')`,
		`insert into auth_policy (namespace, user_verification) values ('finance', 'required')`)
	if w := in.signIn(t, careless); w.Code != http.StatusOK {
		t.Errorf("a sign-in by an account holding only a deny in a namespace requiring user verification answered %d %s", w.Code, w.Body)
	}
}

// A session that may only enrol, as the password sign-in's is where a passkey is required,
// registers a passkey: the router refuses it everywhere else, and the ceremony reads it itself.
func TestASessionThatMayOnlyEnrolRegistersAPasskey(t *testing.T) {
	pool, _ := dbtest.Open(t)
	now := time.Now().UTC().Truncate(time.Second)
	var cookie *http.Cookie
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "bob-password", Login: "bob", Type: db.CredentialPassword, PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"}); err != nil {
			return err
		}
		var err error
		cookie, err = api.OpenSession(ctx, w, "bob", api.OpenedBy{Credential: "bob-password"}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, err := api.NewPrincipals(pool, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AcceptSessions(publicOrigin); err != nil {
		t.Fatal(err)
	}
	// Every session may only enrol here, as a password's does where the policy requires a
	// passkey.
	enrolling := func(r *http.Request) (api.Identity, error) {
		as, err := p.Identify(r)
		as.Enrolling = as.Principal != "" && as.Token == ""
		return as, err
	}
	rt, err := api.NewRouter(p, enrolling)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewPasskeys(rt, api.PasskeyOptions{Pool: pool, PublicURL: publicOrigin, Identify: enrolling, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	in := ceremonies{pool: pool, clock: &now, h: rt}
	made, _, err := newBrowser().Create(in.options(t, `{"ceremony":"registration"}`, cookie))
	if err != nil {
		t.Fatal(err)
	}
	if w := in.verify(t, "registration", made, "", "", cookie); w.Code != http.StatusOK {
		t.Errorf("a registration from a session that may only enrol answered %d %s", w.Code, w.Body)
	}
}
