package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Service accounts created, listed and removed through the routes, against a real PostgreSQL and
// the installation's own Principals, in the installation the token routes are tested in: "the
// service accounts of the namespaces the caller owns, and a new one in one of them, written
// NS/NAME", and the removal of one, which "revokes its tokens". finance and hr are given their
// built-in identities, as init gives them to the namespaces an upgrade keeps.

func withBuiltIns(t *testing.T) *tokened {
	t.Helper()
	in := tokenedInstallation(t)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.GiveBuiltInIdentities(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return in
}

// createdAccount is a service account as the routes answer one, failing the test unless it decodes.
func createdAccount(t *testing.T, body []byte) api.ServiceAccount {
	t.Helper()
	var s api.ServiceAccount
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("%s: %s", body, err)
	}
	return s
}

// accounts is what value lists, failing the test unless it is answered.
func (in *tokened) accounts(t *testing.T, value string) []string {
	t.Helper()
	w := in.ask(t, "GET", "/api/v1/service-accounts", value, "")
	if w.Code != http.StatusOK {
		t.Fatalf("listing was answered %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"service_accounts":[`) {
		t.Errorf("the listing is not an array: %s", w.Body)
	}
	var listed api.ServiceAccountList
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range listed.ServiceAccounts {
		if err := conforms(t, "/$defs/serviceAccount", s); err != nil {
			t.Errorf("%s/%s is listed off the wire's serviceAccount: %s", s.Namespace, s.Name, err)
		}
		names = append(names, s.Namespace+"/"+s.Name)
	}
	return names
}

// A service account is created in a namespace the caller owns, by a grant of its own or of a
// group's, a deny beside it taking nothing from the ownership, and by the bootstrap token in any; it
// is answered as the wire's serviceAccount, created by whoever asked, holding no grant, and its
// namespace's owner mints its tokens. Anybody else is answered as though the namespace did not
// exist, a narrowed token owning none; a name taken is 409, and a name no service account can have,
// agentiik among them, is 400.
func TestAServiceAccountIsCreatedInANamespaceTheCallerOwns(t *testing.T) {
	in := withBuiltIns(t)
	for _, c := range []struct {
		who, namespace, name, by string
	}{
		{"alice", "finance", "deploy", "alice"},
		{"erin", "hr", "payroll", "erin"},
		{"dave", "hr", "audit", "dave"},
	} {
		w := in.ask(t, "POST", "/api/v1/service-accounts", in.values[c.who], `{"namespace":"`+c.namespace+`","name":"`+c.name+`"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s creating %s/%s was answered %d: %s", c.who, c.namespace, c.name, w.Code, w.Body)
		}
		made := createdAccount(t, w.Body.Bytes())
		if made.Kind != "service_account" || made.Namespace != c.namespace || made.Name != c.name || made.CreatedBy != c.by || made.CreatedAt.IsZero() {
			t.Errorf("%s/%s is answered %+v", c.namespace, c.name, made)
		}
		if err := conforms(t, "/$defs/serviceAccount", made); err != nil {
			t.Errorf("%s/%s is answered off the wire's serviceAccount: %s", c.namespace, c.name, err)
		}
	}
	w := in.ask(t, "POST", "/api/v1/service-accounts", in.bootstrap, `{"namespace":"bob","name":"backup"}`)
	if made := createdAccount(t, w.Body.Bytes()); w.Code != http.StatusCreated || made.CreatedBy != "operator" {
		t.Errorf("the bootstrap token creating bob/backup was answered %d: %s", w.Code, w.Body)
	}

	// Created, it holds nothing until somebody grants it something, and its owner mints its token.
	deploy := in.mint(t, in.values["alice"], `{"principal":"finance/deploy"}`)
	if w := in.ask(t, "GET", "/api/v1/finance/probe", deploy.Token, ""); w.Code != http.StatusNotFound {
		t.Errorf("finance/deploy's token was let in before anybody granted it anything: %d", w.Code)
	}

	narrowed := in.mint(t, in.values["alice"], `{"scope":{"within":["finance"]}}`)
	for _, c := range []struct {
		value, body string
		status      int
		says        string
	}{
		{in.values["bob"], `{"namespace":"finance","name":"ci"}`, http.StatusUnprocessableEntity, "namespace finance does not exist, or is not one you own"},
		{in.values["carol"], `{"namespace":"finance","name":"ci"}`, http.StatusUnprocessableEntity, "not one you own"},
		{in.values["alice"], `{"namespace":"hr","name":"ci"}`, http.StatusUnprocessableEntity, "not one you own"},
		{in.values["alice"], `{"namespace":"nowhere","name":"ci"}`, http.StatusUnprocessableEntity, "namespace nowhere does not exist, or is not one you own"},
		{in.values["finance/nightly"], `{"namespace":"finance","name":"ci"}`, http.StatusUnprocessableEntity, "not one you own"},
		{narrowed.Token, `{"namespace":"finance","name":"ci"}`, http.StatusUnprocessableEntity, "a token narrowed by a scope owns none"},
		{in.values["alice"], `{"namespace":"finance","name":"deploy"}`, http.StatusConflict, "namespace finance already has a service account deploy"},
		{in.values["alice"], `{"namespace":"finance","name":"nightly"}`, http.StatusConflict, "already has"},
		{in.values["alice"], `{"namespace":"finance","name":"agentiik"}`, http.StatusBadRequest, "built-in identity"},
		{in.values["alice"], `{"namespace":"finance","name":"runs"}`, http.StatusBadRequest, "reserved"},
		{in.values["alice"], `{"namespace":"finance","name":"Deploy"}`, http.StatusBadRequest, "lowercase words"},
		{in.values["alice"], `{"namespace":"finance","name":"a/b"}`, http.StatusBadRequest, "lowercase words"},
		{in.values["alice"], `{"namespace":"finance","name":"` + strings.Repeat("a", 256) + `"}`, http.StatusBadRequest, "at most 255"},
		{in.values["alice"], `{"namespace":"finance"}`, http.StatusBadRequest, "a service account has a name"},
		{in.values["alice"], `{"name":"ci"}`, http.StatusBadRequest, "namespace: a namespace has a name"},
		{in.values["alice"], `{"namespace":"runs","name":"ci"}`, http.StatusBadRequest, "namespace: runs is a word the API routes on"},
		{in.values["alice"], `{"namespace":"finance","name":"ci","kind":"service_account"}`, http.StatusBadRequest, "kind"},
		{in.values["alice"], ``, http.StatusBadRequest, "empty"},
		{"", `{"namespace":"finance","name":"ci"}`, http.StatusUnauthorized, ""},
	} {
		w := in.ask(t, "POST", "/api/v1/service-accounts", c.value, c.body)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("creating %s was answered %d, want %d saying %q: %s", c.body, w.Code, c.status, c.says, w.Body)
		}
	}
	var made int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), `select count(*) from service_accounts where name = 'ci'`).Scan(&made); err != nil {
		t.Fatal(err)
	}
	if made != 0 {
		t.Errorf("a refusal created %d service accounts", made)
	}
	if w := in.ask(t, "POST", "/api/v1/service-accounts", in.values["alice"], `{"namespace":"finance","name":"`+strings.Repeat("a", 70<<10)+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body past 64 KiB was answered %d", w.Code)
	}
}

// The service accounts of the namespaces the caller owns are listed, the built-in identity of each
// among them, by namespace and then by name, and nobody else's: the bootstrap token owns every
// namespace, a narrowed token none, and whoever owns nothing is answered an empty list.
func TestServiceAccountsAreListedToWhoeverOwnsTheirNamespace(t *testing.T) {
	in := withBuiltIns(t)
	narrowed := in.mint(t, in.values["alice"], `{"scope":{"within":["finance"]}}`)
	for value, want := range map[string][]string{
		in.values["alice"]: {"bob/agentiik", "finance/agentiik", "finance/nightly"},
		in.values["erin"]:  {"hr/agentiik", "hr/sync"},
		in.values["dave"]:  {"hr/agentiik", "hr/sync"},
		in.bootstrap:       {"bob/agentiik", "finance/agentiik", "finance/nightly", "hr/agentiik", "hr/sync"},
		in.values["bob"]:   nil,
		in.values["carol"]: nil,
		narrowed.Token:     nil,
	} {
		if got := in.accounts(t, value); !slices.Equal(got, want) {
			t.Errorf("%.20s lists %q, want %q", value, got, want)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/service-accounts", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("listing with no credential was answered %d", w.Code)
	}
}

// A service account is removed by an owner of its namespace, with its tokens, which open nothing
// from their next request, and its grants; anybody else, and one that is not there, is answered the
// same 404, and the built-in identity is refused with 409, since the runs nobody started are
// attributed to it.
func TestAServiceAccountIsRemovedWithItsTokensAndGrants(t *testing.T) {
	in := withBuiltIns(t)
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "finance/nightly", Scope: access.Scope{Namespace: "finance"}, Role: access.Owner, GrantedBy: "alice"})
	}); err != nil {
		t.Fatal(err)
	}
	nightly := in.values["finance/nightly"]
	if w := in.ask(t, "GET", "/api/v1/finance/probe", nightly, ""); w.Code != http.StatusNoContent {
		t.Fatalf("finance/nightly's token was refused where it owns the namespace: %d", w.Code)
	}

	for _, c := range []struct {
		value, path string
		status      int
		says        string
	}{
		{in.values["bob"], "finance/nightly", http.StatusNotFound, "no such service account, or not of a namespace you own"},
		{in.values["carol"], "finance/nightly", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "hr/sync", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "finance/ghost", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "nowhere/nightly", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "Finance/nightly", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "finance/runs", http.StatusNotFound, "not of a namespace you own"},
		{in.values["alice"], "finance/agentiik", http.StatusConflict, "finance/agentiik is the namespace's built-in identity"},
		{in.values["bob"], "finance/agentiik", http.StatusNotFound, "not of a namespace you own"},
		{"", "finance/nightly", http.StatusUnauthorized, ""},
	} {
		w := in.ask(t, "DELETE", "/api/v1/service-accounts/"+c.path, c.value, "")
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("removing %s was answered %d, want %d saying %q: %s", c.path, w.Code, c.status, c.says, w.Body)
		}
	}
	if w := in.ask(t, "DELETE", "/api/v1/service-accounts/finance/nightly", in.values["alice"], `{"cascade":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("a removal carrying a body was answered %d", w.Code)
	}

	if w := in.ask(t, "DELETE", "/api/v1/service-accounts/finance/nightly", in.values["alice"], ""); w.Code != http.StatusNoContent {
		t.Fatalf("alice removing finance/nightly was answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "GET", "/api/v1/finance/probe", nightly, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the removed service account's token was answered %d at its next request", w.Code)
	}
	var held int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select (select count(*) from api_tokens where principal = 'finance/nightly') + (select count(*) from grants where principal = 'finance/nightly')`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Errorf("%d tokens and grants outlived finance/nightly", held)
	}
	if w := in.ask(t, "DELETE", "/api/v1/service-accounts/finance/nightly", in.values["alice"], ""); w.Code != http.StatusNotFound {
		t.Errorf("removing it again was answered %d", w.Code)
	}
	if got := in.accounts(t, in.values["alice"]); !slices.Equal(got, []string{"bob/agentiik", "finance/agentiik"}) {
		t.Errorf("alice lists %q once finance/nightly is gone", got)
	}
}

// Creating and removing a service account are recorded in its namespace, with who acted; a refusal
// records nothing, and an act the audit log cannot record is not done.
func TestServiceAccountActsAreRecordedInTheirNamespace(t *testing.T) {
	in := withBuiltIns(t)
	in.ask(t, "POST", "/api/v1/service-accounts", in.values["alice"], `{"namespace":"finance","name":"deploy"}`)
	in.ask(t, "POST", "/api/v1/service-accounts", in.values["bob"], `{"namespace":"finance","name":"ci"}`)
	in.ask(t, "DELETE", "/api/v1/service-accounts/finance/agentiik", in.values["alice"], "")
	in.ask(t, "DELETE", "/api/v1/service-accounts/hr/sync", in.values["erin"], "")

	entries := audited(t, in.pool)
	if len(entries) != 2 {
		t.Fatalf("the audit log holds %d entries, and two acts were done: %+v", len(entries), entries)
	}
	for i, want := range []struct{ actor, action, namespace, target string }{
		{"alice", audit.ServiceAccountCreate, "finance", "finance/deploy"},
		{"erin", audit.ServiceAccountDelete, "hr", "hr/sync"},
	} {
		if e := entries[i]; e.Actor != want.actor || e.Action != want.action || e.Namespace != want.namespace || e.Target != want.target || e.Result != audit.Done {
			t.Errorf("entry %d reads %+v, want %+v", i, e, want)
		}
	}
	if by := detailOf(t, entries[1])["created_by"]; by != "carol" {
		t.Errorf("the removal records its creator as %v", by)
	}

	refuseAppends(t, in.super)
	if w := in.ask(t, "POST", "/api/v1/service-accounts", in.values["alice"], `{"namespace":"finance","name":"unrecorded"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("creating with an audit log that records nothing was answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/service-accounts/finance/deploy", in.values["alice"], ""); w.Code != http.StatusInternalServerError {
		t.Errorf("removing with an audit log that records nothing was answered %d", w.Code)
	}
	if got := in.accounts(t, in.values["alice"]); !slices.Equal(got, []string{"bob/agentiik", "finance/agentiik", "finance/deploy", "finance/nightly"}) {
		t.Errorf("acts the audit log refused left %q", got)
	}
}
