package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
)

// A sign-in refused is recorded even though its request is refused, in a transaction of its own:
// an enrolment link or a recovery code presented to register a passkey or set a password, which
// signs its user in where it opens something, as surely as an assertion or a password is. And what
// anybody can send is bounded, so that a flood of them leaves the one chain every act appends to to
// the acts.

// codeOptions starts a registration with an enrolment code, as the enrolment page does.
func (in passwordsOf) codeOptions(t *testing.T, code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"ceremony":"registration"}`
	if code != "" {
		body = fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)
	}
	return in.call(t, "POST", "/api/v1/auth/passkey/options", body, cookies...)
}

// registeredFrom answers the options of a registration with a new passkey on browser, and sends the
// verification, carrying the cookies given.
func (in passwordsOf) registeredFrom(t *testing.T, browser *webauthntest.Authenticator, options *httptest.ResponseRecorder, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	if options.Code != http.StatusOK {
		t.Fatalf("the options answered %d %s", options.Code, options.Body)
	}
	var o optionsAnswer
	if err := json.Unmarshal(options.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	made, _, err := browser.Create(o.Options)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"ceremony": "registration", "credential": made})
	if err != nil {
		t.Fatal(err)
	}
	return in.call(t, "POST", "/api/v1/auth/passkey/verify", string(body), cookies...)
}

// details are the details of the signin.fail entries, in order.
func (in passwordsOf) details(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select detail from audit_log where action = 'signin.fail' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []map[string]any
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			t.Fatal(err)
		}
		all = append(all, d)
	}
	return all
}

// A code that opens nothing is a failed sign-in wherever it is presented, since a registration or a
// password a code starts signs its user in: recorded as signin.fail by the address it came from,
// about the account the code was issued for, with why it opened nothing and the code's kind, or
// about an unknown enrolment code where no code of its value is kept, and never with the code. A
// registration its code started and refused is one too, and so is one answering a challenge that
// names nothing, which anybody may send, about the credential ID it presented. A registration a
// session started signs nobody in, and its refusal is none.
func TestACodeThatOpensNothingIsAFailedSignIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, login := range []string{"gina", "hank", "ivan"} {
			if err := w.CreateUser(ctx, db.User{Login: login, DisplayName: login}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s answered %d %s", what, w.Code, w.Body)
		}
	}
	const secret = "a long enough passphrase"

	nobody := "agkenrol_" + strings.Repeat("A", 43)
	refused("a password set from a code nobody issued", in.enrolWith(t, nobody, secret))
	refused("a registration started with a code nobody issued", in.codeOptions(t, nobody))

	erin := in.enrolCode(t, "erin", db.EnrolmentNewUser)
	if w := in.enrolWith(t, erin, secret); w.Code != http.StatusOK {
		t.Fatalf("erin's password answered %d %s", w.Code, w.Body)
	}
	refused("erin's code used again", in.enrolWith(t, erin, "another long enough passphrase"))

	replaced := in.enrolCode(t, "gina", db.EnrolmentNewUser)
	in.enrolCode(t, "gina", db.EnrolmentNewUser)
	refused("gina's code replaced", in.codeOptions(t, replaced))

	lapsed := in.enrolCode(t, "hank", db.EnrolmentRecovery)
	*in.clock = in.clock.Add(time.Hour)
	refused("hank's code lapsed", in.enrolWith(t, lapsed, secret))

	// ivan's recovery code starts a registration, and is replaced before it is verified.
	ivan := in.enrolCode(t, "ivan", db.EnrolmentRecovery)
	options := in.codeOptions(t, ivan)
	in.enrolCode(t, "ivan", db.EnrolmentRecovery)
	refused("ivan's registration, his code replaced since its options", in.registeredFrom(t, webauthntest.New(in.origin), options))

	// A registration answering no challenge, presenting a credential ID of its own.
	id := base64.RawURLEncoding.EncodeToString([]byte("a credential nobody registered"))
	client := fmt.Sprintf(`{"type":"webauthn.create","challenge":"%s","origin":%q}`, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), in.origin)
	junk, err := json.Marshal(map[string]any{"ceremony": "registration", "credential": webauthntest.Credential{
		ID: id, RawID: id, Type: "public-key", Response: webauthntest.Response{
			ClientDataJSON: base64.RawURLEncoding.EncodeToString([]byte(client)), AttestationObject: "AAAA",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	refused("a registration answering no challenge", in.call(t, "POST", "/api/v1/auth/passkey/verify", string(junk)))

	// carol's session starts a registration, and alice's finishes it: refused, and nobody's sign-in.
	carol, alice := in.passkeyed(t, "carol", "carol-passkey", false), in.passkeyed(t, "alice", "alice-passkey", false)
	refused("carol's registration finished from alice's session", in.registeredFrom(t, webauthntest.New(in.origin), in.codeOptions(t, "", carol), alice))

	unknown := "192.0.2.1 an unknown enrolment code no code of that value was issued, or its account was removed"
	if got := in.failures(t); !slices.Equal(got, []string{
		unknown, unknown,
		"192.0.2.1 erin the code was used already",
		"192.0.2.1 gina the code was replaced by a fresher one",
		"192.0.2.1 hank the code lapsed",
		"192.0.2.1 ivan the code was replaced by a fresher one",
		"192.0.2.1 " + id + " the challenge was never issued, was answered already or lapsed",
	}) {
		t.Errorf("the failed sign-ins recorded are\n%s", strings.Join(got, "\n"))
	}
	var kinds, types []string
	for _, d := range in.details(t) {
		kind, _ := d["code"].(string)
		kinds, types = append(kinds, kind), append(types, d["credential_type"].(string))
		if d["address"] != "192.0.2.1" {
			t.Errorf("a failure records the address %v", d["address"])
		}
	}
	if !slices.Equal(kinds, []string{"", "", "enrolment", "enrolment", "recovery", "recovery", ""}) {
		t.Errorf("the failures record the codes' kinds %q", kinds)
	}
	if !slices.Equal(types, []string{"password", "passkey", "password", "passkey", "password", "passkey", "passkey"}) {
		t.Errorf("the failures record the credentials' types %q", types)
	}
	for _, code := range []string{nobody, erin, replaced, lapsed, ivan} {
		if n := in.count(t, `select count(*) from audit_log where strpos(detail || target, $1) > 0 or strpos(detail || target, $2) > 0`,
			code, strings.TrimPrefix(code, "agkenrol_")); n != 0 {
			t.Errorf("a code is in %d entries of the audit log", n)
		}
	}
	if n := in.count(t, `select count(*) from enrolment_codes where used_at is not null`); n != 1 {
		t.Errorf("%d codes are spent, and erin's alone was", n)
	}
	if len(*in.trouble) != 0 {
		t.Errorf("the refusals reported %v", *in.trouble)
	}
}

// What refused codes append is bounded as a refused assertion's is, and shares its count: ten from
// one address in ten minutes, the rest answered all the same and counted in the next entry recorded.
func TestRefusedCodesAreBoundedWithTheOtherFailedSignIns(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	for i := range 12 {
		code := fmt.Sprintf("agkenrol_%043d", i)
		var w *httptest.ResponseRecorder
		if i%2 == 0 {
			w = in.enrolWith(t, code, "a long enough passphrase")
		} else {
			w = in.codeOptions(t, code)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code %d answered %d %s", i, w.Code, w.Body)
		}
	}
	if n := len(in.failures(t)); n != 10 {
		t.Errorf("twelve codes refused from one address recorded %d entries", n)
	}
	if w := in.login(t, `{"login":"alice","password":"not the password"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password answered %d", w.Code)
	}
	if n := len(in.failures(t)); n != 10 {
		t.Errorf("a password refused past the codes' bound recorded %d entries", n)
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/password/enrol",
		strings.NewReader(`{"code":"agkenrol_`+strings.Repeat("B", 43)+`","password":"a long enough passphrase"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", in.origin)
	r.RemoteAddr = "198.51.100.9:4000"
	w := httptest.NewRecorder()
	if in.h.ServeHTTP(w, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("a code from another address answered %d", w.Code)
	}
	details := in.details(t)
	if got := details[len(details)-1]; got["address"] != "198.51.100.9" || got["unrecorded"] != float64(3) {
		t.Errorf("the entry after the bound records %v", got)
	}
}

// A flood of failed sign-ins from many addresses, as a botnet sends them, appends what the bound
// lets through and no more, each in a transaction of its own that holds the head of the chain for
// its one append; so an act appending at the same moment, a grant here, waits behind a hundred
// appends at most, however long the flood goes on, and lands in the chain as it would have with no
// flood.
func TestAFloodOfFailedSignInsLeavesTheChainToTheOtherActs(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	const senders, each = 25, 24
	// An assertion answering no challenge, what anybody may send.
	client := fmt.Sprintf(`{"type":"webauthn.get","challenge":"%s","origin":%q}`, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), in.origin)
	answered, err := json.Marshal(map[string]any{"ceremony": "assertion", "credential": webauthntest.Credential{
		ID: "AAAA", RawID: "AAAA", Type: "public-key", Response: webauthntest.Response{
			ClientDataJSON: base64.RawURLEncoding.EncodeToString([]byte(client)), AuthenticatorData: "AAAA", Signature: "AAAA", UserHandle: "AAAA",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertion := string(answered)
	var wg sync.WaitGroup
	for s := range senders {
		wg.Go(func() {
			for i := range each {
				body, path := `{"code":"agkenrol_`+strings.Repeat("C", 43)+`","password":"a long enough passphrase"}`, "/api/v1/auth/password/enrol"
				switch i % 3 {
				case 1:
					body, path = `{"ceremony":"registration","code":"agkenrol_`+strings.Repeat("D", 43)+`"}`, "/api/v1/auth/passkey/options"
				case 2:
					body, path = assertion, "/api/v1/auth/passkey/verify"
				}
				r := httptest.NewRequestWithContext(t.Context(), "POST", path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", in.origin)
				r.RemoteAddr = fmt.Sprintf("10.9.%d.%d:5000", s, i)
				w := httptest.NewRecorder()
				if in.h.ServeHTTP(w, r); w.Code != http.StatusUnauthorized {
					t.Errorf("a failed sign-in in the flood answered %d %s", w.Code, w.Body)
				}
			}
		})
	}
	var slowest time.Duration
	var granted []string
	for range 10 {
		id := ulid.New()
		began := time.Now()
		if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
			if err := n.GrantAccess(ctx, access.Grant{ID: id, Principal: "erin", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"}); err != nil {
				return err
			}
			return n.Audit(ctx, audit.Record{Actor: "carol", Action: audit.GrantCreate, Target: id, Result: audit.Done})
		}); err != nil {
			t.Fatal(err)
		}
		slowest = max(slowest, time.Since(began))
		granted = append(granted, id)
	}
	wg.Wait()

	if n := len(in.failures(t)); n != 100 {
		t.Errorf("%d failed sign-ins from %d addresses recorded %d entries", senders*each, senders*each, n)
	}
	// A hundred appends of a line each, however slow the machine, take far less than this; a
	// flood appending one for each of its six hundred would not.
	if slowest > 5*time.Second {
		t.Errorf("a grant beside the flood took %s", slowest)
	}
	entries, err := in.pool.AuditTrail().After(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(entries); err != nil {
		t.Errorf("the chain beside the flood does not verify: %s", err)
	}
	var grants []string
	for _, e := range entries {
		if e.Action == audit.GrantCreate {
			grants = append(grants, e.Target)
		}
	}
	if !slices.Equal(grants, granted) {
		t.Errorf("the grants in the chain are %q, and %q were written", grants, granted)
	}
}
