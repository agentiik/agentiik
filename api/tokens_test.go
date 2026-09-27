package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// API tokens minted, listed and revoked through the routes, against a real PostgreSQL and the
// installation's own Principals: "stored hashed, shown once, revocable one by one, listed with last
// use and device label", "expires after 90 days unless asked otherwise, and after a year at most",
// and a scope that "can only narrow".

// tokened is an installation for the token routes and the service account routes, on a clock the
// test moves.
//
// finance holds monthly-invoicing, and hr holds nothing. alice owns finance, and bob's personal
// namespace, which bob shares with her as owner; bob edits finance and owns monthly-invoicing alone,
// which owns no namespace; carol administers the installation and holds no grant; dave owns hr with
// run:read_data denied him there, which takes a permission and leaves the role; erin is in team-hr,
// which owns hr. finance/nightly and hr/sync are service accounts. Each principal but the service
// account hr/sync holds a token with no scope, and the bootstrap token has not ended.
type tokened struct {
	pool      *db.Pool
	super     string
	at        time.Time
	h         http.Handler
	values    map[string]string
	bootstrap string
}

func tokenedInstallation(t *testing.T) *tokened {
	t.Helper()
	pool, super := dbtest.Open(t)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance'), ('hr')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
	} {
		if _, err := dbtest.Superuser(t, super).Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	in := &tokened{
		pool: pool, super: super, at: time.Now().UTC().Truncate(time.Second), values: map[string]string{},
		bootstrap: "3f1c8a0e" + strings.Repeat("7", 56),
	}
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob"},
			{Login: "carol", DisplayName: "Carol", Admin: true}, {Login: "dave", DisplayName: "Dave"},
			{Login: "erin", DisplayName: "Erin"},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "team-hr"); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "team-hr", "erin"); err != nil {
			return err
		}
		for _, sa := range []db.ServiceAccount{
			{Namespace: "finance", Name: "nightly", CreatedBy: "carol"},
			{Namespace: "hr", Name: "sync", CreatedBy: "carol"},
		} {
			if err := w.CreateServiceAccount(ctx, sa); err != nil {
				return err
			}
		}
		for _, who := range []string{"alice", "bob", "carol", "dave", "erin", "finance/nightly"} {
			value := "agktoken_" + strings.ReplaceAll(who, "/", "-") + strings.Repeat("Q", 40)
			hash := sha256.Sum256([]byte(value))
			if err := w.MintToken(ctx, db.APIToken{
				ID: ulid.New(), Hash: hash[:], Principal: who,
				CreatedAt: in.at.Add(-time.Hour), ExpiresAt: in.at.Add(7 * 24 * time.Hour),
			}); err != nil {
				return err
			}
			in.values[who] = value
		}
		hash := sha256.Sum256([]byte(in.bootstrap))
		_, err := w.SetBootstrapToken(ctx, hash[:])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// bob's personal namespace, named after his login, as every user's is.
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name, kind, owner) values ('bob', 'personal', 'bob')`); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	grant := func(ns string, gs ...access.Grant) {
		t.Helper()
		if err := pool.In(t.Context(), ns, func(ctx context.Context, n *db.NS) error {
			for _, g := range gs {
				g.ID, g.GrantedBy = ulid.New(), "carol"
				if err := n.GrantAccess(ctx, g); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	finance, hr := access.Scope{Namespace: "finance"}, access.Scope{Namespace: "hr"}
	grant("finance",
		access.Grant{Principal: "alice", Scope: finance, Role: access.Owner},
		access.Grant{Principal: "bob", Scope: finance, Role: access.Editor},
		access.Grant{Principal: "bob", Scope: access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}, Role: access.Owner},
	)
	grant("hr",
		access.Grant{Principal: "dave", Scope: hr, Role: access.Owner},
		access.Grant{Principal: "dave", Scope: hr, Deny: access.RunReadData},
		access.Grant{Principal: "group:team-hr", Scope: hr, Role: access.Owner},
	)
	grant("bob", access.Grant{Principal: "alice", Scope: access.Scope{Namespace: "bob"}, Role: access.Owner})

	clock := func() time.Time { return in.at }
	principals, err := api.NewPrincipals(pool, clock)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(principals, principals.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServiceAccounts(rt, api.ServiceAccountOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	// A route needing grant:manage in a namespace, to see what a minted token opens.
	rt.MustHandle("GET", "/api/v1/{namespace}/probe", api.Needs{Permission: api.GrantManage, Scope: api.Namespace},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
			w.WriteHeader(http.StatusNoContent)
		})
	in.h = rt
	return in
}

// ask sends one request bearing value, which is no credential where it is empty.
func (in *tokened) ask(t *testing.T, method, path, value, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	}
	if value != "" {
		r.Header.Set("Authorization", "Bearer "+value)
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// mint mints one token as value, failing the test unless it is minted.
func (in *tokened) mint(t *testing.T, value, body string) api.IssuedToken {
	t.Helper()
	w := in.ask(t, "POST", "/api/v1/auth/tokens", value, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("minting %s was answered %d: %s", body, w.Code, w.Body)
	}
	var issued api.IssuedToken
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	return issued
}

// listing is what value lists, failing the test unless it is answered.
func (in *tokened) listing(t *testing.T, value string) []api.APIToken {
	t.Helper()
	w := in.ask(t, "GET", "/api/v1/auth/tokens", value, "")
	if w.Code != http.StatusOK {
		t.Fatalf("listing was answered %d: %s", w.Code, w.Body)
	}
	var listed api.TokenList
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	return listed.Tokens
}

func idsOf(tokens []api.APIToken) []string {
	var ids []string
	for _, tk := range tokens {
		ids = append(ids, tk.ID)
	}
	return ids
}

// apiTokenForm is openapi.json's apiTokenValue: agktoken_ and 256 bits of base64url.
var apiTokenForm = regexp.MustCompile(`^agktoken_[A-Za-z0-9_-]{43}$`)

// A token is minted with 256 bits, shown in the answer that mints it and nowhere after, kept as its
// SHA-256, and opens what its principal holds from its first request: alice's opens finance, which
// she owns, and is listed with its label and its use.
func TestATokenIsMintedShownOnceAndKeptAsItsHash(t *testing.T) {
	in := tokenedInstallation(t)
	w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values["alice"], `{"device_label":"agk on alice-laptop"}`)
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("minting was answered %d, Cache-Control %q: %s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	var issued api.IssuedToken
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	record := issued.APIToken
	if !apiTokenForm.MatchString(issued.Token) {
		t.Errorf("the token is written %q", issued.Token)
	}
	if record.Principal != "alice" || record.DeviceLabel != "agk on alice-laptop" || record.Scope != nil ||
		!record.CreatedAt.Equal(in.at) || record.LastUsedAt != nil {
		t.Errorf("the token's record reads %+v", record)
	}
	if err := conforms(t, "/$defs/apiToken", record); err != nil {
		t.Errorf("the token's record is not the wire's apiToken: %s", err)
	}

	var stored []byte
	var row string
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select hash, row_to_json(api_tokens)::text from api_tokens where id = $1`, record.ID).Scan(&stored, &row); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(issued.Token)); !bytes.Equal(stored, sum[:]) || strings.Contains(row, strings.TrimPrefix(issued.Token, "agktoken_")) {
		t.Errorf("the token is kept as %x in %s, and it is kept as its SHA-256 alone", stored, row)
	}

	if w := in.ask(t, "GET", "/api/v1/finance/probe", issued.Token, ""); w.Code != http.StatusNoContent {
		t.Errorf("alice's new token was refused in finance, which she owns: %d", w.Code)
	}
	in.at = in.at.Add(time.Minute)
	listed := in.listing(t, issued.Token)
	i := slices.IndexFunc(listed, func(tk api.APIToken) bool { return tk.ID == record.ID })
	if i < 0 || listed[i].DeviceLabel != "agk on alice-laptop" || listed[i].LastUsedAt == nil || !listed[i].LastUsedAt.Equal(in.at) {
		t.Errorf("the new token is listed as %+v", listed)
	}
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", issued.Token, ""); strings.Contains(w.Body.String(), strings.TrimPrefix(issued.Token, "agktoken_")) {
		t.Error("the listing carries the token's value")
	}
	if w := in.ask(t, "POST", "/api/v1/auth/tokens", "", `{}`); w.Code != http.StatusUnauthorized {
		t.Errorf("minting with no credential was answered %d", w.Code)
	}
}

// A token expires 90 days after it is minted unless asked otherwise, and a year after at most: a
// year to the second is taken, and so is a minute past it, as asked, which a client's clock running
// ahead of the installation's asks for when it means a year; a second past that is refused, and so
// is an expiry already passed.
func TestATokenExpiresInNinetyDaysAndAYearAtMost(t *testing.T) {
	in := tokenedInstallation(t)
	alice := in.values["alice"]
	if got := in.mint(t, alice, `{}`).APIToken; !got.ExpiresAt.Equal(in.at.AddDate(0, 0, 90)) {
		t.Errorf("a token asked for no expiry expires at %s, minted at %s", got.ExpiresAt, in.at)
	}
	year := in.at.AddDate(1, 0, 0)
	if got := in.mint(t, alice, `{"expires_at":"`+year.Format(time.RFC3339)+`"}`).APIToken; !got.ExpiresAt.Equal(year) {
		t.Errorf("a token asked to last a year expires at %s", got.ExpiresAt)
	}
	ahead := year.Add(time.Minute)
	if got := in.mint(t, alice, `{"expires_at":"`+ahead.Format(time.RFC3339)+`"}`).APIToken; !got.ExpiresAt.Equal(ahead) {
		t.Errorf("a token asked to last a year and a minute expires at %s", got.ExpiresAt)
	}
	for expires, status := range map[string]int{
		ahead.Add(time.Second).Format(time.RFC3339): http.StatusUnprocessableEntity,
		in.at.Format(time.RFC3339):                  http.StatusUnprocessableEntity,
		in.at.Add(-time.Hour).Format(time.RFC3339):  http.StatusUnprocessableEntity,
		in.at.Add(time.Hour).Format("2006-01-02"):   http.StatusBadRequest,
		"": http.StatusBadRequest,
		in.at.Add(time.Hour).Format(time.RFC3339Nano): http.StatusCreated,
	} {
		if w := in.ask(t, "POST", "/api/v1/auth/tokens", alice, `{"expires_at":"`+expires+`"}`); w.Code != status {
			t.Errorf("a token expiring at %q was answered %d, want %d: %s", expires, w.Code, status, w.Body)
		}
	}

	// And it stops at its expiry, gone from the listing too.
	short := in.mint(t, alice, `{"expires_at":"`+in.at.Add(time.Hour).Format(time.RFC3339)+`"}`)
	in.at = in.at.Add(time.Hour)
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", short.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a token at its expiry was answered %d", w.Code)
	}
	if slices.Contains(idsOf(in.listing(t, alice)), short.APIToken.ID) {
		t.Error("a token past its expiry is listed")
	}
}

// A scope only narrows: a token keeps what its principal holds within it and nothing past it, a
// permission its principal lacks grants nothing, and a token narrowed by a scope mints no other.
func TestAScopeOnlyNarrowsAndANarrowedTokenMintsNone(t *testing.T) {
	in := tokenedInstallation(t)
	elsewhere := in.mint(t, in.values["alice"], `{"scope":{"within":["hr"]}}`)
	if s := elsewhere.APIToken.Scope; s == nil || !slices.Equal(s.Within, []string{"hr"}) || s.Permissions != nil {
		t.Errorf("the scope is answered as %+v", s)
	}
	if err := conforms(t, "/$defs/apiToken", elsewhere.APIToken); err != nil {
		t.Errorf("a narrowed token's record is not the wire's apiToken: %s", err)
	}
	if w := in.ask(t, "GET", "/api/v1/finance/probe", elsewhere.Token, ""); w.Code != http.StatusNotFound {
		t.Errorf("alice's token within hr was let into finance: %d", w.Code)
	}
	inFinance := in.mint(t, in.values["alice"], `{"scope":{"permissions":["grant:manage"],"within":["finance"]}}`)
	if w := in.ask(t, "GET", "/api/v1/finance/probe", inFinance.Token, ""); w.Code != http.StatusNoContent {
		t.Errorf("alice's token keeping grant:manage in finance was refused there: %d", w.Code)
	}
	lacking := in.mint(t, in.values["bob"], `{"scope":{"permissions":["grant:manage"]}}`)
	if w := in.ask(t, "GET", "/api/v1/finance/probe", lacking.Token, ""); w.Code != http.StatusNotFound {
		t.Errorf("bob's token naming grant:manage, which he lacks, was let in: %d", w.Code)
	}

	for _, narrowed := range []string{elsewhere.Token, inFinance.Token} {
		w := in.ask(t, "POST", "/api/v1/auth/tokens", narrowed, `{}`)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "narrowed by a scope mints no token") {
			t.Errorf("a narrowed token minting another was answered %d: %s", w.Code, w.Body)
		}
	}

	for body, why := range map[string]string{
		`{"scope":{}}`:                                        "names neither",
		`{"scope":{"permissions":[]}}`:                        "keeps no permission",
		`{"scope":{"within":[]}}`:                             "reaches nothing",
		`{"scope":{"permissions":["run:read","run:read"]}}`:   "twice",
		`{"scope":{"permissions":["workflow:everything"]}}`:   "not a permission",
		`{"scope":{"within":["Finance"]}}`:                    "not a namespace",
		`{"scope":{"within":["runs"]}}`:                       "a word the API routes on",
		`{"scope":{"within":["finance/"]}}`:                   "names no workflow",
		`{"scope":{"permissions":["run:read"],"every":true}}`: "not a field of it",
		`{"scope":{"within":["finance","finance"]}}`:          "twice",
		`{"scope":"finance"}`:                                 "an object",
		`{"device_label":"a\u0000b"}`:                         "U+0000",
		`{"device_label":""}`:                                 "device_label is empty",
		`{"device_label":"` + strings.Repeat("é", 257) + `"}`: "at most 256",
		`{"priority":"high"}`:                                 "not a field of it",
	} {
		w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values["alice"], body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), why) {
			t.Errorf("%s was answered %d, and it is refused as %q: %s", body, w.Code, why, w.Body)
		}
	}
	if w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values["alice"], `{"device_label":"`+strings.Repeat("a", 70<<10)+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body past 64 KiB was answered %d", w.Code)
	}
	if got := in.mint(t, in.values["alice"], `{"device_label":null,"expires_at":null,"scope":null,"principal":null}`).APIToken; got.Scope != nil || got.DeviceLabel != "" || got.Principal != "alice" {
		t.Errorf("a request of nulls minted %+v", got)
	}
}

// A token is minted for the caller, or for a service account of a namespace it owns, through a grant
// of its own or of a group's and whatever a deny beside it takes; for anybody else it is refused
// with one sentence whether they exist or not, the user a personal namespace owned is named after
// included, and a group is not a holder at all. A service account's token mints none for that
// service account, named or left out: its next token is minted by someone who still means it.
func TestATokenIsMintedForAServiceAccountOfANamespaceTheCallerOwns(t *testing.T) {
	in := tokenedInstallation(t)
	for _, c := range []struct {
		who, principal string
	}{
		{"alice", "finance/nightly"},
		{"erin", "hr/sync"},
		{"dave", "hr/sync"},
		{"alice", "alice"},
	} {
		body := `{}`
		if c.principal != "" {
			body = `{"principal":"` + c.principal + `"}`
		}
		want := c.principal
		if want == "" {
			want = c.who
		}
		if got := in.mint(t, in.values[c.who], body).APIToken; got.Principal != want {
			t.Errorf("%s minting %s minted a token of %s", c.who, body, got.Principal)
		}
	}
	nightly := in.mint(t, in.values["alice"], `{"principal":"finance/nightly","scope":{"permissions":["grant:manage"]}}`)
	if w := in.ask(t, "GET", "/api/v1/finance/probe", nightly.Token, ""); w.Code != http.StatusNotFound {
		t.Errorf("finance/nightly's token was let in where finance/nightly holds nothing: %d", w.Code)
	}

	for _, c := range []struct {
		who, principal string
		status         int
		says           string
	}{
		{"bob", "finance/nightly", http.StatusUnprocessableEntity, "neither you nor a service account of a namespace you own"},
		{"alice", "bob", http.StatusUnprocessableEntity, "neither you nor"},
		{"alice", "hr/sync", http.StatusUnprocessableEntity, "neither you nor"},
		{"alice", "hr/nobody", http.StatusUnprocessableEntity, "neither you nor"},
		{"carol", "finance/nightly", http.StatusUnprocessableEntity, "neither you nor"},
		{"bob", "alice", http.StatusUnprocessableEntity, "neither you nor"},
		{"finance/nightly", "alice", http.StatusUnprocessableEntity, "neither you nor"},
		{"alice", "finance/ghost", http.StatusUnprocessableEntity, "finance/ghost names nobody"},
		{"alice", "group:team-hr", http.StatusBadRequest, "is a group"},
		{"alice", "operator", http.StatusBadRequest, "operator"},
		{"alice", "Finance/nightly", http.StatusBadRequest, "names no service account"},
		{"alice", "", http.StatusBadRequest, "principal is empty"},
		{"finance/nightly", "finance/nightly", http.StatusForbidden, "mints no token for that service account"},
	} {
		w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values[c.who], `{"principal":"`+c.principal+`"}`)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s minting for %s was answered %d, want %d saying %q: %s", c.who, c.principal, w.Code, c.status, c.says, w.Body)
		}
	}
	if w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values["finance/nightly"], `{}`); w.Code != http.StatusForbidden {
		t.Errorf("finance/nightly minting a token of its own was answered %d: %s", w.Code, w.Body)
	}
}

// The bootstrap token mints nothing, for itself or for anybody, and says what it is for instead;
// once it has ended it identifies nobody at all.
func TestTheBootstrapTokenMintsNoToken(t *testing.T) {
	in := tokenedInstallation(t)
	for _, body := range []string{`{}`, `{"principal":"finance/nightly"}`, `{"principal":"operator"}`} {
		w := in.ask(t, "POST", "/api/v1/auth/tokens", in.bootstrap, body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "agk user create LOGIN --admin") {
			t.Errorf("the bootstrap token minting %s was answered %d: %s", body, w.Code, w.Body)
		}
	}
	var minted int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), `select count(*) from api_tokens`).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != len(in.values) {
		t.Errorf("the bootstrap token's refusals left %d tokens, and there were %d", minted, len(in.values))
	}

	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, in.at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"POST", "GET"} {
		if w := in.ask(t, method, "/api/v1/auth/tokens", in.bootstrap, `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("the ended bootstrap token's %s was answered %d", method, w.Code)
		}
	}
}

// A listing holds the caller's tokens and those of the service accounts of the namespaces it owns,
// newest first, and none revoked, expired or anybody else's; a narrowed token lists itself alone,
// and the bootstrap token, owning every namespace until it ends, the service accounts' of each.
func TestAListingHoldsWhatTheCallerMayRevoke(t *testing.T) {
	in := tokenedInstallation(t)
	alice := in.values["alice"]
	in.at = in.at.Add(time.Second)
	nightly := in.mint(t, alice, `{"principal":"finance/nightly","device_label":"terraform"}`)
	in.at = in.at.Add(time.Second)
	sync := in.mint(t, in.values["erin"], `{"principal":"hr/sync"}`)
	in.at = in.at.Add(time.Second)
	revoked := in.mint(t, alice, `{}`)
	in.at = in.at.Add(time.Second)
	narrowed := in.mint(t, alice, `{"scope":{"within":["finance"]}}`)
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+revoked.APIToken.ID, alice, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoking was answered %d", w.Code)
	}

	listed := in.listing(t, alice)
	for _, tk := range listed {
		if err := conforms(t, "/$defs/apiToken", tk); err != nil {
			t.Errorf("%s is listed off the wire's apiToken: %s", tk.ID, err)
		}
	}
	ids := idsOf(listed)
	if len(ids) != 4 || ids[0] != narrowed.APIToken.ID || ids[1] != nightly.APIToken.ID || !slices.Contains(ids, in.tokenOf(t, "alice")) || !slices.Contains(ids, in.tokenOf(t, "finance/nightly")) {
		t.Errorf("alice lists %q", ids)
	}
	// bob's own token is not listed, though alice owns the namespace named after his login: a
	// login is no service account of it.
	if slices.Contains(ids, revoked.APIToken.ID) || slices.Contains(ids, sync.APIToken.ID) || slices.Contains(ids, in.tokenOf(t, "bob")) {
		t.Errorf("alice lists a revoked token or one not hers: %q", ids)
	}
	if got := idsOf(in.listing(t, narrowed.Token)); !slices.Equal(got, []string{narrowed.APIToken.ID}) {
		t.Errorf("a narrowed token lists %q, and it lists itself alone", got)
	}
	if got := idsOf(in.listing(t, in.values["bob"])); !slices.Equal(got, []string{in.tokenOf(t, "bob")}) {
		t.Errorf("bob, who owns nothing, lists %q", got)
	}
	if got := idsOf(in.listing(t, in.bootstrap)); len(got) != 3 || got[0] != sync.APIToken.ID || got[1] != nightly.APIToken.ID {
		t.Errorf("the bootstrap token lists %q, the service accounts' tokens of every namespace", got)
	}
}

// A revoked token opens nothing from its next request, the one revoking itself included. A token is
// revoked by its principal, or by an owner of its service account's namespace, and anybody else is
// answered as though it did not exist; a narrowed token revokes itself alone. Revoking again is
// answered the same and changes nothing.
func TestARevokedTokenOpensNothingFromItsNextRequest(t *testing.T) {
	in := tokenedInstallation(t)
	alice := in.values["alice"]
	laptop := in.mint(t, alice, `{"device_label":"laptop"}`)
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", laptop.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("the new token was answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+laptop.APIToken.ID, alice, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoking was answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", laptop.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the revoked token was answered %d at its next request", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+laptop.APIToken.ID, alice, ""); w.Code != http.StatusNoContent {
		t.Errorf("revoking again was answered %d", w.Code)
	}

	self := in.mint(t, alice, `{}`)
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+self.APIToken.ID, self.Token, ""); w.Code != http.StatusNoContent {
		t.Errorf("a token revoking itself was answered %d", w.Code)
	}
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", self.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a token that revoked itself was answered %d next", w.Code)
	}

	nightly := in.mint(t, alice, `{"principal":"finance/nightly"}`)
	sync := in.mint(t, in.values["erin"], `{"principal":"hr/sync"}`)
	narrowed := in.mint(t, alice, `{"scope":{"within":["finance"]}}`)
	for _, c := range []struct {
		who, id string
	}{
		{in.values["bob"], nightly.APIToken.ID},
		{in.values["bob"], in.tokenOf(t, "alice")},
		{alice, sync.APIToken.ID},
		{alice, in.tokenOf(t, "bob")},
		{in.values["carol"], in.tokenOf(t, "alice")},
		{in.values["finance/nightly"], in.tokenOf(t, "alice")},
		{narrowed.Token, in.tokenOf(t, "alice")},
		{narrowed.Token, nightly.APIToken.ID},
		{alice, "01M2AD1R3T5W7Y9A1C3E5G7J9M"},
		{alice, "not-a-token"},
	} {
		if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+c.id, c.who, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no such token, or not yours") {
			t.Errorf("revoking %s was answered %d: %s", c.id, w.Code, w.Body)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/auth/tokens", nightly.Token, ""); w.Code != http.StatusOK {
		t.Errorf("a token nobody may revoke was revoked: %d", w.Code)
	}
	for who, id := range map[string]string{alice: nightly.APIToken.ID, narrowed.Token: narrowed.APIToken.ID, in.values["erin"]: sync.APIToken.ID} {
		if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+id, who, ""); w.Code != http.StatusNoContent {
			t.Errorf("revoking %s was answered %d: %s", id, w.Code, w.Body)
		}
	}
	for _, value := range []string{nightly.Token, narrowed.Token, sync.Token} {
		if w := in.ask(t, "GET", "/api/v1/auth/tokens", value, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("a revoked token was answered %d", w.Code)
		}
	}
}

// Minting and revoking are recorded in the audit log, by the token's identifier and never its value,
// with who acted, whose token it is, its label, expiry and scope, in the namespace of the service
// account it belongs to and on the installation for a user's; revoking a token already revoked is
// recorded as unchanged, and a refusal records nothing.
func TestMintingAndRevokingAreRecorded(t *testing.T) {
	in := tokenedInstallation(t)
	minted := in.mint(t, in.values["alice"], `{"principal":"finance/nightly","device_label":"terraform","scope":{"permissions":["workflow:run"],"within":["finance/monthly-invoicing"]}}`)
	in.ask(t, "POST", "/api/v1/auth/tokens", in.values["bob"], `{"principal":"finance/nightly"}`)
	for range 2 {
		if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+minted.APIToken.ID, in.values["alice"], ""); w.Code != http.StatusNoContent {
			t.Fatalf("revoking was answered %d", w.Code)
		}
	}
	in.ask(t, "DELETE", "/api/v1/auth/tokens/"+in.tokenOf(t, "alice"), in.values["bob"], "")
	own := in.mint(t, in.values["alice"], `{}`)

	entries := audited(t, in.pool)
	if len(entries) != 4 {
		t.Fatalf("the audit log holds %d entries, and four acts were done: %+v", len(entries), entries)
	}
	if e := entries[3]; e.Action != audit.APITokenCreate || e.Target != own.APIToken.ID || e.Namespace != "" || detailOf(t, e)["principal"] != "alice" {
		t.Errorf("alice's own token is recorded as %+v", e)
	}
	for i, want := range []struct{ action, result string }{
		{audit.APITokenCreate, audit.Done}, {audit.APITokenRevoke, audit.Done}, {audit.APITokenRevoke, audit.Unchanged},
	} {
		e := entries[i]
		if e.Actor != "alice" || e.Action != want.action || e.Target != minted.APIToken.ID || e.Result != want.result || e.Namespace != "finance" {
			t.Errorf("entry %d reads %+v, want %s %s", i, e, want.action, want.result)
		}
		if strings.Contains(e.Detail, strings.TrimPrefix(minted.Token, "agktoken_")) {
			t.Errorf("entry %d carries the token's value", i)
		}
	}
	created := detailOf(t, entries[0])
	scope, _ := json.Marshal(created["scope"])
	if created["principal"] != "finance/nightly" || created["device_label"] != "terraform" ||
		created["expires_at"] != minted.APIToken.ExpiresAt.Format(time.RFC3339Nano) ||
		string(scope) != `{"permissions":["workflow:run"],"within":["finance/monthly-invoicing"]}` {
		t.Errorf("the minting is recorded as %v", created)
	}
	if revoked := detailOf(t, entries[1]); revoked["principal"] != "finance/nightly" {
		t.Errorf("the revocation is recorded as %v", revoked)
	}
}

// A token nobody may record is not minted: the audit log refusing the entry refuses the act.
func TestATokenTheAuditLogCannotRecordIsNotMinted(t *testing.T) {
	in := tokenedInstallation(t)
	refuseAppends(t, in.super)
	if w := in.ask(t, "POST", "/api/v1/auth/tokens", in.values["alice"], `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("minting with an audit log that records nothing was answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+in.tokenOf(t, "alice"), in.values["alice"], ""); w.Code != http.StatusInternalServerError {
		t.Errorf("revoking with an audit log that records nothing was answered %d", w.Code)
	}
	var minted int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), `select count(*) from api_tokens where revoked_at is null`).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != len(in.values) {
		t.Errorf("%d tokens are live, and nothing was minted or revoked", minted)
	}
}

// tokenOf is the identifier of the token the installation was seeded with for who.
func (in *tokened) tokenOf(t *testing.T, who string) string {
	t.Helper()
	var id string
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select id from api_tokens where principal = $1 order by created_at limit 1`, who).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A principal holds a hundred live tokens at most: the next is refused with 409 and nothing is
// minted or recorded, one revoked or expired makes room again, and a service account is held to the
// same hundred, whoever mints for it. Mints racing for the last place are counted one after the
// other, and one of them takes it.
func TestAPrincipalHoldsAHundredLiveTokensAtMost(t *testing.T) {
	in := tokenedInstallation(t)
	alice := in.values["alice"]
	eightDays := `{"expires_at":"` + in.at.Add(8*24*time.Hour).Format(time.RFC3339) + `"}`
	// alice holds the one she was seeded with, which expires in a week.
	var last api.IssuedToken
	for range 99 {
		last = in.mint(t, alice, eightDays)
	}
	w := in.ask(t, "POST", "/api/v1/auth/tokens", alice, `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "alice holds 100 live tokens") {
		t.Fatalf("a hundred and first token was answered %d: %s", w.Code, w.Body)
	}
	before := len(audited(t, in.pool))
	if w := in.ask(t, "DELETE", "/api/v1/auth/tokens/"+last.APIToken.ID, alice, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoking was answered %d", w.Code)
	}
	kept := in.mint(t, alice, eightDays)
	if w := in.ask(t, "POST", "/api/v1/auth/tokens", alice, `{}`); w.Code != http.StatusConflict {
		t.Errorf("a token past the hundred, once one was revoked and one minted in its place, was answered %d", w.Code)
	}
	if got := len(audited(t, in.pool)); got != before+2 {
		t.Errorf("the audit log grew by %d entries over a revocation, a mint and a refusal", got-before)
	}
	// A week on, the seeded one has expired and makes room.
	in.at = in.at.Add(7 * 24 * time.Hour)
	in.mint(t, kept.Token, `{}`)

	// finance/nightly's seeded token has expired too; alice fills it to a hundred, with the last
	// place raced for.
	for range 99 {
		in.mint(t, kept.Token, `{"principal":"finance/nightly"}`)
	}
	codes := make(chan int, 5)
	for range 5 {
		go func() {
			codes <- in.ask(t, "POST", "/api/v1/auth/tokens", kept.Token, `{"principal":"finance/nightly"}`).Code
		}()
	}
	minted := 0
	for range 5 {
		switch code := <-codes; code {
		case http.StatusCreated:
			minted++
		case http.StatusConflict:
		default:
			t.Errorf("a token racing for the last place was answered %d", code)
		}
	}
	var live int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select count(*) from api_tokens where principal = 'finance/nightly' and revoked_at is null and expires_at > $1`, in.at).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if minted != 1 || live != 100 {
		t.Errorf("%d of five mints racing for the last place were minted, and finance/nightly holds %d live tokens", minted, live)
	}
}
