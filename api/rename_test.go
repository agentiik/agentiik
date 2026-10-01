package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// A namespace renamed keeps its old name as a former name, which every address naming the namespace
// still reaches: a repository's clone and push URL, a webhook's, and the routes under /api/v1/{ns}/.
// What it stored is read where it was stored, under the name it was created with: the packs a clone
// fetches, the files of its versions, and the secret its webhook is signed with, sealed before the
// rename and opened after it. A push of a file whose metadata.namespace is the old name is taken.
func TestARenamedNamespaceAnswersAtItsFormerName(t *testing.T) {
	auth := hooking().(granted)
	accounting := api.Target{Namespace: "accounting"}
	auth["alice"] = append(auth["alice"], grant{api.WorkflowRead, accounting}, grant{api.WorkflowWrite, accounting})
	g := servingGit(t, auth, func(o *api.ServerOptions) { o.Hooks = clearHooks{} })
	work := g.newClone("alice")
	work.write("agentiik.yaml", triggered(`  webhook:
    - path: /invoicing
      map:
        orders: ${{ trigger.body.orders }}
`))
	first := work.commit("hooked")
	work.must("push", "-q", "origin", "main")
	key := bytes.Repeat([]byte{7}, 32)
	g.writeSecret(key)

	if err := g.pool.Installation(t.Context(), db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		_, err := w.RenameNamespace(ctx, "finance", "accounting")
		return err
	}); err != nil {
		t.Fatalf("the rename answered %v", err)
	}

	// The repository, cloned and pushed at the address it had: its packs are read under the name
	// the namespace was created with, and a version whose file names the old name is taken.
	clone := g.cloned("alice")
	if got := strings.TrimSpace(clone.must("rev-parse", "HEAD")); got != first {
		t.Errorf("a clone at the old address holds %s, and the repository %s", got, first)
	}
	work.write("README.md", "renamed\n")
	second := work.commit("after the rename")
	work.must("push", "-q", "origin", "main")
	var commit string
	if err := g.pool.In(t.Context(), "accounting", func(ctx context.Context, ns *db.NS) error {
		r, err := ns.Repository(ctx, "monthly-invoicing")
		for _, ref := range r.Refs {
			if ref.Name == "refs/heads/main" {
				commit = ref.Target()
			}
		}
		return err
	}); err != nil || commit != second {
		t.Errorf("the push at the old address moved main to %q, want %s: %v", commit, second, err)
	}

	// A file of a version, read at the old address and at the new.
	for _, path := range []string{
		"/api/v1/finance/workflows/monthly-invoicing/tree/main?path=README.md",
		"/api/v1/accounting/workflows/monthly-invoicing/tree/main?path=README.md",
	} {
		if w, _ := call(t, g.h, "GET", path, "alice", nil); w.Code != http.StatusOK || w.Body.String() != "renamed\n" {
			t.Errorf("GET %s answered %d %q", path, w.Code, w.Body)
		}
	}

	// The webhook, at the old address and at the new, signed with the secret written before the
	// rename: each starts a run of the namespace under its new name.
	for i, path := range []string{"/hooks/finance/invoicing", "/hooks/accounting/invoicing"} {
		d := signed(key, "msg_"+string(rune('a'+i)), time.Now(), `{"orders":[]}`)
		d.path = path
		w := g.deliver(d)
		if w.Code != http.StatusAccepted || !strings.HasPrefix(w.Header().Get("Location"), "/api/v1/accounting/runs/") {
			t.Errorf("a delivery at %s answered %d at %q: %s", path, w.Code, w.Header().Get("Location"), w.Body)
		}
	}
	var runs int
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
		`select count(*) from runs where namespace = 'accounting' and triggered_by = 'accounting/agentiik'`).Scan(&runs); err != nil || runs != 2 {
		t.Errorf("%d runs of the renamed namespace were started by its built-in identity: %v", runs, err)
	}
}

// renaming is the namespaces' routes with tokens for dave, who owns finance, and for a user with a
// personal namespace.
type renaming struct {
	namespaces
	dave, frank string
}

func someRenaming(t *testing.T) renaming {
	t.Helper()
	in := someNamespaces(t)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "frank", DisplayName: "Frank"}); err != nil {
			return err
		}
		if _, err := w.CreateNamespace(ctx, db.Namespace{Name: "frank", Kind: db.NamespacePersonal, Owner: "frank"}); err != nil {
			return err
		}
		return w.GrantAccess(ctx, access.Grant{
			ID: ulid.New(), Principal: "frank", Scope: access.Scope{Namespace: "frank"}, Role: access.Owner, GrantedBy: "frank",
		})
	}); err != nil {
		t.Fatal(err)
	}
	// dave owns finance, and acts in it here, so he is no longer suspended.
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `update users set suspended = false where login = 'dave'`); err != nil {
		t.Fatal(err)
	}
	later := in.now.Add(time.Hour)
	return renaming{namespaces: in, dave: in.token(t, "dave", nil, nil, later), frank: in.token(t, "frank", nil, nil, later)}
}

// The owner of a shared namespace renames it, and the answer is its record under the new name, the
// old one among its former names; the old name reaches it on every route naming it, and a
// namespace is read under its new name in a listing. The rename is recorded as namespace.rename,
// with the name it left and the one it took, and a rename to its own name changes nothing and is
// recorded as unchanged.
func TestAnOwnerRenamesANamespace(t *testing.T) {
	in := someRenaming(t)
	w := in.ask(t, "PATCH", "/api/v1/namespaces/finance", in.dave, `{"name":"accounting"}`)
	want := `{"name":"accounting","kind":"shared","quotas":{"max_concurrent_tasks":20,"max_retention_days":90},"former_names":["finance"],"avatar_updated_at":null}`
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("the rename answered %d %s, want %s", w.Code, w.Body, want)
	}
	valid(t, "/$defs/namespaceRecord", w.Body.Bytes())

	for _, c := range []struct{ method, path, token string }{
		{"GET", "/api/v1/namespaces/finance", in.alice},
		{"GET", "/api/v1/namespaces/accounting", in.alice},
		{"GET", "/api/v1/namespaces/finance/quotas", in.alice},
		{"GET", "/api/v1/finance/stats/quotas", in.alice},
		{"GET", "/api/v1/accounting/stats/quotas", in.alice},
	} {
		if w := in.ask(t, c.method, c.path, c.token, ""); w.Code != http.StatusOK {
			t.Errorf("%s %s after the rename answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces/finance", in.alice, ""); !strings.Contains(w.Body.String(), `"name":"accounting"`) {
		t.Errorf("the old name reads %s", w.Body)
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces/finance", in.erin, ""); w.Code != http.StatusNotFound {
		t.Errorf("the old name read by somebody holding nothing there answered %d", w.Code)
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces", in.alice, ""); !strings.Contains(w.Body.String(), `"name":"accounting"`) || strings.Contains(w.Body.String(), `"name":"finance"`) {
		t.Errorf("the listing reads %s", w.Body)
	}
	if !in.holds(t, "accounting/nightly", api.WorkflowRun, api.Target{Namespace: "accounting", Workflow: "payroll"}) {
		t.Error("the service account of the renamed namespace holds nothing in it under its new name")
	}
	if n := in.count(t, `select count(*) from principals where id = 'accounting/nightly'`); n != 1 {
		t.Error("the service account is not written under the namespace's new name")
	}

	// Its own name, by the old one: nothing changes.
	if w := in.ask(t, "PATCH", "/api/v1/namespaces/finance", in.dave, `{"name":"accounting"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"former_names":["finance"]`) {
		t.Errorf("a rename to its own name answered %d %s", w.Code, w.Body)
	}
	// An administrator renames it back, taking the former name back.
	if w := in.ask(t, "PATCH", "/api/v1/namespaces/accounting", in.carol, `{"name":"finance"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"finance"`) || !strings.Contains(w.Body.String(), `"former_names":["accounting"]`) {
		t.Errorf("an administrator renaming it back answered %d %s", w.Code, w.Body)
	}
	got := in.entries(t)
	if len(got) != 3 || got[0] != "dave namespace.rename accounting done" || got[1] != "dave namespace.rename accounting unchanged" || got[2] != "carol namespace.rename finance done" {
		t.Errorf("the audit log holds %q", got)
	}
	var detail string
	in.query(t, &detail, `select detail from audit_log where action = 'namespace.rename' order by seq limit 1`)
	if detail != `{"from":"finance","to":"accounting"}` {
		t.Errorf("the rename is recorded with %s", detail)
	}
}

// A rename is refused, saying why, where it cannot be made: a personal namespace, a name a login, a
// namespace, a word the API routes on or another namespace's former name holds, a namespace with a
// run going, and a body that names nothing or what is no name. Anybody but its owner or an
// administrator is answered the absence a namespace's routes answer, and nothing changes.
func TestARenameIsRefusedWhatItCannotMake(t *testing.T) {
	in := someRenaming(t)
	if w := in.ask(t, "PATCH", "/api/v1/namespaces/hr", in.carol, `{"name":"people"}`); w.Code != http.StatusOK {
		t.Fatalf("renaming hr answered %d: %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		path, token, body string
		want              int
		says              string
	}{
		{"/api/v1/namespaces/frank", in.frank, `{"name":"francis"}`, http.StatusConflict, "never renamed"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":"alice"}`, http.StatusConflict, "is a user's login"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":"people"}`, http.StatusConflict, "is already a namespace"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":"hr"}`, http.StatusConflict, "is a name namespace people held before it was renamed"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":"runs"}`, http.StatusConflict, "routes on"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":"Accounting"}`, http.StatusBadRequest, "not a namespace"},
		{"/api/v1/namespaces/finance", in.dave, `{}`, http.StatusBadRequest, "names nothing to change"},
		{"/api/v1/namespaces/finance", in.dave, `{"name":null}`, http.StatusBadRequest, "null"},
		{"/api/v1/namespaces/finance", in.dave, `null`, http.StatusBadRequest, "null"},
		{"/api/v1/namespaces/finance", in.dave, `{"title":"Accounting"}`, http.StatusBadRequest, "not a field"},
		{"/api/v1/namespaces/finance", in.alice, `{"name":"accounting"}`, http.StatusNotFound, "no such thing"},
		{"/api/v1/namespaces/finance", in.erin, `{"name":"accounting"}`, http.StatusNotFound, "no such thing"},
		{"/api/v1/namespaces/nowhere", in.carol, `{"name":"accounting"}`, http.StatusNotFound, "no namespace"},
	} {
		if w := in.ask(t, "PATCH", c.path, c.token, c.body); w.Code != c.want || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("PATCH %s %s answered %d %s, want %d saying %q", c.path, c.body, w.Code, w.Body, c.want, c.says)
		}
	}

	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `
		insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		  values ('finance', 'payroll', 'a3f9c1e', '{}', 'dave', now());
		insert into runs (namespace, id, workflow, commit, trigger) values ('finance', '01JMZ8V1P9C4XQ7K2N4D6F8H0A', 'payroll', 'a3f9c1e', 'manual')`); err != nil {
		t.Fatal(err)
	}
	if w := in.ask(t, "PATCH", "/api/v1/namespaces/finance", in.dave, `{"name":"accounting"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "carry its name to their runners") {
		t.Errorf("a rename while a run is queued answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from namespaces where name in ('finance', 'frank', 'people')`); n != 3 {
		t.Error("a refused rename changed a namespace")
	}
}

// Any user creates a namespace and owns it, so they remove it once it holds nothing; a namespace
// they do not own is answered as absent, an administrator removes any, and a personal namespace is
// removed with its user and never on its own.
func TestAnOwnerRemovesTheirNamespace(t *testing.T) {
	in := someRenaming(t)
	if w := in.ask(t, "POST", "/api/v1/namespaces", in.alice, `{"name":"alice-lab"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/alice-lab", in.dave, ""); w.Code != http.StatusNotFound {
		t.Errorf("somebody else removing the namespace answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/alice-lab", in.alice, ""); w.Code != http.StatusNoContent {
		t.Errorf("its owner removing the namespace answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/finance", in.dave, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "holds 2 workflows") {
		t.Errorf("its owner removing a namespace holding workflows answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/frank", in.frank, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "personal namespace") {
		t.Errorf("a user removing their personal namespace answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "POST", "/api/v1/namespaces", in.alice, `{"name":"alice-lab"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating again answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/alice-lab", in.carol, ""); w.Code != http.StatusNoContent {
		t.Errorf("an administrator removing the namespace answered %d: %s", w.Code, w.Body)
	}
	got := in.entries(t)
	if len(got) != 6 || got[2] != "alice namespace.delete alice-lab done" || got[5] != "carol namespace.delete alice-lab done" {
		t.Errorf("the audit log holds %q", got)
	}
}

// A namespace's picture is held to a user's photo's rules: set by its owner, a personal namespace's
// user among them, or an administrator, stored as a PNG of its pixels alone, read by whoever reads
// the namespace's record with its instant as its tag, and removed idempotently. Anybody else is
// answered the absence a namespace's routes answer.
func TestANamespacesPictureIsSetByItsOwnerAndReadByItsMembers(t *testing.T) {
	in := someRenaming(t)
	picture := aJPEG(t, quarters(1024, 512))
	put := func(path, token, kind string, body []byte) int {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), "PUT", path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", kind)
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		return w.Code
	}
	if code := put("/api/v1/namespaces/finance/avatar", in.dave, "image/jpeg", picture); code != http.StatusNoContent {
		t.Fatalf("its owner setting the picture answered %d", code)
	}
	w := in.ask(t, "GET", "/api/v1/namespaces/finance/avatar", in.alice, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "private, max-age=86400" || w.Header().Get("ETag") == "" {
		t.Fatalf("a member reading the picture answered %d %v", w.Code, w.Header())
	}
	decoded, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || decoded.Bounds().Dx() != 512 || decoded.Bounds().Dy() != 256 {
		t.Errorf("the picture read back is %v, %v", decoded.Bounds(), err)
	}
	var record api.NamespaceRecord
	json.Unmarshal(in.ask(t, "GET", "/api/v1/namespaces/finance", in.alice, "").Body.Bytes(), &record)
	if record.AvatarUpdatedAt == nil {
		t.Error("the record says nothing of the picture set")
	}
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/api/v1/namespaces/finance/avatar", in.erin, http.StatusNotFound},
		{"GET", "/api/v1/namespaces/hr/avatar", in.carol, http.StatusNotFound},
		{"DELETE", "/api/v1/namespaces/finance/avatar", in.alice, http.StatusNotFound},
	} {
		if w := in.ask(t, c.method, c.path, c.token, ""); w.Code != c.want {
			t.Errorf("%s %s answered %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
	for _, c := range []struct {
		token, kind string
		body        []byte
		want        int
	}{
		{in.alice, "image/jpeg", picture, http.StatusNotFound},
		{in.dave, "image/gif", picture, http.StatusUnsupportedMediaType},
		{in.dave, "image/png", []byte("not a picture"), http.StatusUnprocessableEntity},
		{in.dave, "image/png", pngAnnouncing(4096, 16), http.StatusUnprocessableEntity},
		{in.dave, "image/png", bytes.Repeat([]byte{0}, 1<<20+1), http.StatusRequestEntityTooLarge},
		{in.carol, "image/jpeg", picture, http.StatusNoContent},
		{in.frank, "image/jpeg", picture, http.StatusNotFound},
	} {
		if code := put("/api/v1/namespaces/finance/avatar", c.token, c.kind, c.body); code != c.want {
			t.Errorf("a picture of %s sent as %s answered %d, want %d", c.kind, c.token[:14], code, c.want)
		}
	}
	// A personal namespace's user gives it a picture as an owner does.
	if code := put("/api/v1/namespaces/frank/avatar", in.frank, "image/jpeg", picture); code != http.StatusNoContent {
		t.Errorf("a user setting their personal namespace's picture answered %d", code)
	}

	for range 2 {
		if w := in.ask(t, "DELETE", "/api/v1/namespaces/finance/avatar", in.dave, ""); w.Code != http.StatusNoContent {
			t.Errorf("removing the picture answered %d: %s", w.Code, w.Body)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces/finance/avatar", in.alice, ""); w.Code != http.StatusNotFound {
		t.Errorf("the picture removed reads %d", w.Code)
	}
	got := in.entries(t)
	if len(got) != 5 || got[0] != "dave namespace.avatar finance done" || got[3] != "dave namespace.avatar finance done" || got[4] != "dave namespace.avatar finance unchanged" {
		t.Errorf("the audit log holds %q", got)
	}
}
