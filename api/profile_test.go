package api_test

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// PATCH /api/v1/me and the photo routes against a real PostgreSQL, behind the real Principals.

// profiles is somePrincipals serving the routes about the caller and the administrator's routes about
// users, with a token for each a test asks as: alice and bob, users; carol, who administers the
// installation; finance/nightly, a service account; narrowed, alice's token narrowed to run:read in
// finance; and bootstrap, the bootstrap token, not ended. The routes' clock, at, runs a minute after
// the one the principals were made by.
type profiles struct {
	principals
	h      http.Handler
	at     time.Time
	tokens map[string]string
}

func someProfiles(t *testing.T) profiles {
	t.Helper()
	in := somePrincipals(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	at := in.now.Add(time.Minute)
	now := func() time.Time { return at }
	if _, err := api.NewMe(rt, api.MeOptions{Pool: in.pool, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: in.pool, PublicURL: "https://agentiik.example.com", Now: now}); err != nil {
		t.Fatal(err)
	}
	later := in.now.Add(time.Hour)
	p := profiles{principals: in, h: rt, at: at, tokens: map[string]string{}}
	for _, who := range []string{"alice", "bob", "carol", "finance/nightly"} {
		p.tokens[who] = in.token(t, who, nil, nil, later)
	}
	p.tokens["narrowed"] = in.token(t, "alice", []string{"run:read"}, []string{"finance"}, later)
	p.tokens["bootstrap"] = "agk_bootstrap_" + strings.Repeat("b", 43)
	in.withBootstrap(t, p.tokens["bootstrap"])
	return p
}

// ask sends one request as who, with body of type kind where body is not nil.
func (in profiles) ask(t *testing.T, method, path, who, kind string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
		if kind != "" {
			r.Header.Set("Content-Type", kind)
		}
	}
	if token := in.tokens[who]; token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// patch is PATCH /api/v1/me as who with body.
func (in profiles) patch(t *testing.T, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	return in.ask(t, "PATCH", "/api/v1/me", who, "application/json", []byte(body))
}

// userIn reads the user an answer carries, under user where it is the me document, and holds it to
// $defs/user.
func userIn(t *testing.T, w *httptest.ResponseRecorder, nested bool) (api.User, json.RawMessage) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	raw := json.RawMessage(w.Body.Bytes())
	if nested {
		var me struct {
			User json.RawMessage `json:"user"`
		}
		if err := json.Unmarshal(raw, &me); err != nil {
			t.Fatal(err)
		}
		raw = me.User
	}
	valid(t, "/$defs/user", raw)
	var u api.User
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	return u, raw
}

// refusalOf is the sentence an answer refuses with, and its body where it carries none.
func refusalOf(w *httptest.ResponseRecorder) string {
	var refused struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &refused) != nil || refused.Error == "" {
		return w.Body.String()
	}
	return refused.Error
}

// entries are what the audit log recorded of action, in order, each written result actor target
// detail.
func (in profiles) entries(t *testing.T, action string) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(),
		`select result || ' ' || actor || ' ' || target || ' ' || detail from audit_log where action = $1 order by seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// A user writes what they say of themself, reads it back wherever a user is answered, and clears a
// field with the empty string, the others kept; the audit log names the fields that changed and
// never what they hold.
func TestAUserWritesAndClearsWhatTheySayOfThemself(t *testing.T) {
	in := someProfiles(t)

	// Nothing said yet but the given name she was created with: every other field empty, no
	// email address, and no photo, which is null rather than absent.
	before, raw := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true)
	if before.GivenName != "Alice" || before.DisplayName != "Alice" || before.FamilyName != "" || before.Bio != "" || before.AvatarUpdatedAt != nil ||
		!bytes.Contains(raw, []byte(`"avatar_updated_at":null`)) || !bytes.Contains(raw, []byte(`"bio":""`)) || !bytes.Contains(raw, []byte(`"email":""`)) {
		t.Errorf("alice, having said nothing, reads as %s", raw)
	}

	w := in.patch(t, "alice", `{"given_name":"Alice","family_name":"Martin","title":"Technical lead","location":"Lyon, France","timezone":"Europe/Paris","bio":"Writes the invoicing workflows, and reviews what touches payroll."}`)
	written, _ := userIn(t, w, true)
	want := api.User{
		Kind: "user", Login: "alice", DisplayName: "Alice Martin", GivenName: "Alice", FamilyName: "Martin",
		Title: "Technical lead", Location: "Lyon, France", Timezone: "Europe/Paris",
		Bio: "Writes the invoicing workflows, and reviews what touches payroll.", CreatedAt: before.CreatedAt,
	}
	if written != want {
		t.Errorf("PATCH answered alice as\n%+v\nwant\n%+v", written, want)
	}
	var me api.Me
	json.Unmarshal(w.Body.Bytes(), &me)
	if me.Principal != "alice" || me.Groups == nil || me.Permissions == nil || me.Notifications == nil {
		t.Errorf("PATCH answered %s, which is not who alice is as GET /api/v1/me answers it", w.Body)
	}
	if got, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true); got != want {
		t.Errorf("GET /api/v1/me answers alice as %+v", got)
	}
	if got, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/alice", "carol", "", nil), false); got != want {
		t.Errorf("GET /api/v1/users/alice answers her as %+v", got)
	}
	listing := in.ask(t, "GET", "/api/v1/users", "carol", "", nil)
	var users struct {
		Users []json.RawMessage `json:"users"`
	}
	if listing.Code != http.StatusOK || json.Unmarshal(listing.Body.Bytes(), &users) != nil || len(users.Users) != 4 {
		t.Fatalf("GET /api/v1/users answered %d: %s", listing.Code, listing.Body)
	}
	for _, u := range users.Users {
		valid(t, "/$defs/user", u)
	}
	if !bytes.Contains(users.Users[0], []byte(`"title":"Technical lead"`)) || !bytes.Contains(users.Users[1], []byte(`"title":""`)) {
		t.Errorf("GET /api/v1/users lists alice and bob as %s and %s", users.Users[0], users.Users[1])
	}

	// The empty string clears a field, and what the body leaves out is kept.
	cleared, _ := userIn(t, in.patch(t, "alice", `{"bio":"","location":""}`), true)
	want.Bio, want.Location = "", ""
	if cleared != want {
		t.Errorf("clearing two fields left alice as %+v", cleared)
	}
	// The same again changes nothing, and is recorded so.
	if again, _ := userIn(t, in.patch(t, "alice", `{"bio":"","title":"Technical lead"}`), true); again != want {
		t.Errorf("asking again left alice as %+v", again)
	}
	// Another user's profile is theirs: bob's is untouched.
	if bob, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "bob", "", nil), true); bob.Title != "" || bob.DisplayName != "Bob" {
		t.Errorf("bob reads as %+v", bob)
	}

	recorded := in.entries(t, "user.profile")
	wantRecorded := []string{
		`done alice alice {"fields":["family_name","title","location","timezone","bio"]}`,
		`done alice alice {"fields":["location","bio"]}`,
		`unchanged alice alice {"fields":[]}`,
	}
	if strings.Join(recorded, "\n") != strings.Join(wantRecorded, "\n") {
		t.Errorf("the audit log recorded\n%s\nwant\n%s", strings.Join(recorded, "\n"), strings.Join(wantRecorded, "\n"))
	}
	for _, e := range recorded {
		if strings.Contains(e, "Martin") || strings.Contains(e, "Lyon") {
			t.Errorf("the audit log recorded what alice wrote: %s", e)
		}
	}
}

// A display name is the given and family names, each trimmed of the spaces at its ends and joined by
// one space, either alone where only one is said, and the login where neither is: as the user writes
// them, wherever a user is answered.
func TestADisplayNameIsMadeOfTheGivenAndFamilyNames(t *testing.T) {
	in := someProfiles(t)
	for _, c := range []struct{ body, want string }{
		{`{"given_name":"Alice","family_name":"Martin"}`, "Alice Martin"},
		{`{"given_name":"  Alice ","family_name":" de la Fontaine  "}`, "Alice de la Fontaine"},
		{`{"given_name":""}`, "de la Fontaine"},
		{`{"family_name":"","given_name":"Alice"}`, "Alice"},
		{`{"given_name":""}`, "alice"},
		{`{"given_name":"   ","family_name":" "}`, "alice"},
		{`{"given_name":"アリス"}`, "アリス"},
	} {
		got, _ := userIn(t, in.patch(t, "alice", c.body), true)
		if got.DisplayName != c.want {
			t.Errorf("after %s alice's display name is %q, want %q", c.body, got.DisplayName, c.want)
		}
		if listed, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/alice", "carol", "", nil), false); listed.DisplayName != c.want {
			t.Errorf("after %s an administrator reads alice's display name as %q, want %q", c.body, listed.DisplayName, c.want)
		}
	}
}

// What a profile would not hold is refused with 400 saying which field and why, a caller with no
// profile with 403 saying so, and none of it changes anything or is recorded.
func TestAProfileTheUserMayNotWriteIsRefused(t *testing.T) {
	in := someProfiles(t)
	if w := in.patch(t, "alice", `{"given_name":"Alice","timezone":"America/Argentina/Buenos_Aires"}`); w.Code != http.StatusOK {
		t.Fatalf("alice writing her profile answered %d: %s", w.Code, w.Body)
	}
	before, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true)
	for _, c := range []struct {
		who, body string
		status    int
		says      string
	}{
		{"alice", `{"timezone":"Mars/Olympus_Mons"}`, http.StatusBadRequest, `"Mars/Olympus_Mons" is not a time zone`},
		{"alice", `{"timezone":"Local"}`, http.StatusBadRequest, "timezone: Local"},
		{"alice", `{"timezone":"` + strings.Repeat("A", 65) + `"}`, http.StatusBadRequest, "at most 64"},
		{"alice", `{"nickname":"Al"}`, http.StatusBadRequest, `"nickname", which is not a field`},
		{"alice", `{"title":"Lead\nadministrator: mallory"}`, http.StatusBadRequest, "title: it holds a line break"},
		{"alice", `{"location":"Lyon\u001b[2J"}`, http.StatusBadRequest, "location: it holds a line break or another control character"},
		{"alice", `{"bio":"` + strings.Repeat("é", 281) + `"}`, http.StatusBadRequest, "bio: it is at most 280 characters and this one is 281"},
		{"alice", `{"given_name":"` + strings.Repeat("a", 129) + `"}`, http.StatusBadRequest, "given_name: it is at most 128 characters"},
		{"alice", `{"family_name":"` + strings.Repeat("a", 129) + `"}`, http.StatusBadRequest, "family_name: it is at most 128"},
		{"alice", `{"display_name":"Alice Martin"}`, http.StatusBadRequest, "display_name: nobody writes a display name: it is made of the given and family names"},
		{"alice", `{"given_name":"Alice","display_name":"Alice Martin"}`, http.StatusBadRequest, "display_name: nobody writes"},
		{"alice", `{"email":"alice@example.com"}`, http.StatusBadRequest, "email: an email address is given by an administrator"},
		{"alice", `{}`, http.StatusBadRequest, "names nothing to change"},
		{"alice", ``, http.StatusBadRequest, "the request body is empty"},
		{"alice", `{"bio":null}`, http.StatusBadRequest, "bio: null says neither"},
		{"alice", `{"bio":"a","bio":"b"}`, http.StatusBadRequest, "twice"},
		{"alice", `{"bio":7}`, http.StatusBadRequest, "a number"},
		{"finance/nightly", `{"bio":"a service account"}`, http.StatusForbidden, "a service account has no profile"},
		{"bootstrap", `{"bio":"the operator"}`, http.StatusForbidden, "the bootstrap token is nobody"},
		{"narrowed", `{"bio":"a script"}`, http.StatusForbidden, "a token narrowed by a scope changes no profile"},
		{"", `{"bio":"nobody"}`, http.StatusUnauthorized, "no credential"},
	} {
		w := in.patch(t, c.who, c.body)
		if w.Code != c.status || !strings.Contains(refusalOf(w), c.says) {
			t.Errorf("PATCH %.40s as %q answered %d: %s, want %d saying %q", c.body, c.who, w.Code, w.Body, c.status, c.says)
		}
	}
	if after, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true); after != before {
		t.Errorf("a refusal changed alice from %+v to %+v", before, after)
	}
	if recorded := in.entries(t, "user.profile"); len(recorded) != 1 {
		t.Errorf("the audit log recorded %d profile changes, and one was made: %v", len(recorded), recorded)
	}
}

// An administrator gives a user an email address, changes it and removes it, and the user and
// administrators read it wherever the user is answered. Nothing else of a user is an administrator's
// to change here, a name or the display name refused saying whose it is; a refusal changes nothing
// and is not recorded, and the log names the field that changed and never the address.
func TestAnAdministratorGivesAUserAnEmailAddress(t *testing.T) {
	in := someProfiles(t)
	update := func(who, login, body string) *httptest.ResponseRecorder {
		return in.ask(t, "PATCH", "/api/v1/users/"+login, who, "application/json", []byte(body))
	}

	given, raw := userIn(t, update("carol", "alice", `{"email":"alice.martin@example.com"}`), false)
	if given.Login != "alice" || given.Email != "alice.martin@example.com" || given.DisplayName != "Alice" || !bytes.Contains(raw, []byte(`"email":"alice.martin@example.com"`)) {
		t.Errorf("alice given an address reads as %s", raw)
	}
	if got, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true); got != given {
		t.Errorf("alice reads herself as %+v, and her administrator as %+v", got, given)
	}
	if got, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/alice", "carol", "", nil), false); got != given {
		t.Errorf("GET /api/v1/users/alice answers %+v", got)
	}
	if bob, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "bob", "", nil), true); bob.Email != "" {
		t.Errorf("bob, given nothing, reads as %+v", bob)
	}
	// The same again changes nothing; the bootstrap token, until the first administrator signs in,
	// changes it as an administrator does; the empty string removes it.
	if again, _ := userIn(t, update("carol", "alice", `{"email":"alice.martin@example.com"}`), false); again != given {
		t.Errorf("the same address again left alice as %+v", again)
	}
	if changed, _ := userIn(t, update("bootstrap", "alice", `{"email":"a.martin@example.org"}`), false); changed.Email != "a.martin@example.org" {
		t.Errorf("the bootstrap token changing the address left alice as %+v", changed)
	}
	removed, raw := userIn(t, update("carol", "alice", `{"email":""}`), false)
	if removed.Email != "" || !bytes.Contains(raw, []byte(`"email":""`)) {
		t.Errorf("alice's address removed reads as %s", raw)
	}

	before, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/alice", "carol", "", nil), false)
	for _, c := range []struct {
		who, login, body string
		status           int
		says             string
	}{
		{"carol", "alice", `{"given_name":"Alicia"}`, http.StatusBadRequest, "given_name: what a user says of themself is theirs to write, at PATCH /api/v1/me"},
		{"carol", "alice", `{"bio":"Writes nothing."}`, http.StatusBadRequest, "bio: what a user says of themself is theirs"},
		{"carol", "alice", `{"display_name":"Alicia"}`, http.StatusBadRequest, "display_name: nobody writes a display name"},
		{"carol", "alice", `{"admin":true}`, http.StatusBadRequest, `"admin", which is not a field`},
		{"carol", "alice", `{}`, http.StatusBadRequest, "the request names nothing to change: email"},
		{"carol", "alice", ``, http.StatusBadRequest, "the request body is empty"},
		{"carol", "alice", `{"email":null}`, http.StatusBadRequest, "email: null says neither"},
		{"carol", "alice", `{"email":"alice"}`, http.StatusBadRequest, `email: "alice" is not an email address`},
		{"carol", "alice", `{"email":"alice @example.com"}`, http.StatusBadRequest, "email: it holds a space"},
		{"carol", "alice", `{"email":"` + strings.Repeat("a", 243) + `@example.com"}`, http.StatusBadRequest, "at most 254 characters and this one is 255"},
		{"carol", "nobody", `{"email":"nobody@example.com"}`, http.StatusNotFound, "no user by that login"},
		{"carol", "Alice", `{"email":"alice@example.com"}`, http.StatusNotFound, "no user by that login"},
		{"carol", "finance%2Fnightly", `{"email":"nightly@example.com"}`, http.StatusNotFound, ""},
		{"alice", "alice", `{"email":"alice@example.com"}`, http.StatusForbidden, ""},
		{"bob", "alice", `{"email":"bob@example.com"}`, http.StatusForbidden, ""},
		{"", "alice", `{"email":"alice@example.com"}`, http.StatusUnauthorized, "no credential"},
	} {
		w := update(c.who, c.login, c.body)
		if w.Code != c.status || !strings.Contains(refusalOf(w), c.says) {
			t.Errorf("PATCH /api/v1/users/%s %.40s as %q answered %d: %s, want %d saying %q", c.login, c.body, c.who, w.Code, w.Body, c.status, c.says)
		}
	}
	if after, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/alice", "carol", "", nil), false); after != before {
		t.Errorf("a refusal changed alice from %+v to %+v", before, after)
	}

	recorded := in.entries(t, "user.update")
	wantRecorded := []string{
		`done carol alice {"fields":["email"]}`,
		`unchanged carol alice {"fields":[]}`,
		`done operator alice {"fields":["email"]}`,
		`done carol alice {"fields":["email"]}`,
	}
	if strings.Join(recorded, "\n") != strings.Join(wantRecorded, "\n") {
		t.Errorf("the audit log recorded\n%s\nwant\n%s", strings.Join(recorded, "\n"), strings.Join(wantRecorded, "\n"))
	}
	for _, e := range recorded {
		if strings.Contains(e, "martin") || strings.Contains(e, "@") {
			t.Errorf("the audit log recorded the address: %s", e)
		}
	}
}

// A photo is stored as a PNG of its pixels alone and read by its owner and by an administrator,
// with what a browser keeps it by; refused for a type, a weight or a size in pixels it may not have,
// and by whoever has no photo to set; removed by its owner as often as they like, and by an
// administrator once. Every set and removal is recorded, and never the photo.
func TestAPhotoIsReadByItsOwnerAndAnAdministratorAlone(t *testing.T) {
	in := someProfiles(t)
	sent := withExif(t, aJPEG(t, quarters(600, 300)), 1)
	if w := in.ask(t, "PUT", "/api/v1/me/avatar", "alice", "image/jpeg", sent); w.Code != http.StatusNoContent {
		t.Fatalf("alice setting her photo answered %d: %s", w.Code, w.Body)
	}

	// Read by alice: a PNG of 512 by 256, with nothing of the file but its pixels, kept a day by
	// her browser alone.
	w := in.ask(t, "GET", "/api/v1/me/avatar?v=whatever", "alice", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("alice reading her photo answered %d: %s", w.Code, w.Body)
	}
	for header, want := range map[string]string{"Content-Type": "image/png", "Cache-Control": "private, max-age=86400", "X-Content-Type-Options": "nosniff"} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("the photo is answered with %s %q, want %q", header, got, want)
		}
	}
	stored := w.Body.Bytes()
	picture, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the photo answered is no PNG: %s", err)
	}
	if b := picture.Bounds(); b.Dx() != 512 || b.Dy() != 256 {
		t.Errorf("a photo of 600 by 300 is answered at %d by %d", b.Dx(), b.Dy())
	}
	if bytes.Contains(stored, []byte(whereabouts)) || bytes.Contains(stored, []byte("Exif")) {
		t.Error("the photo answered carries the Exif of the file sent")
	}
	tag := w.Header().Get("ETag")
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/me/avatar", nil)
	r.Header.Set("Authorization", "Bearer "+in.tokens["alice"])
	r.Header.Set("If-None-Match", tag)
	revalidated := httptest.NewRecorder()
	in.h.ServeHTTP(revalidated, r)
	if tag == "" || revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
		t.Errorf("the photo asked again by its tag %q answered %d with %d bytes", tag, revalidated.Code, revalidated.Body.Len())
	}

	// alice reads when she set it, as the console adds it to the photo's address.
	if alice, _ := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true); alice.AvatarUpdatedAt == nil || !alice.AvatarUpdatedAt.Equal(in.at) {
		t.Errorf("alice set her photo at %s, and reads as setting it at %v", in.at, alice.AvatarUpdatedAt)
	}
	// Her narrowed token reads her photo, as it reads who she is.
	if w := in.ask(t, "GET", "/api/v1/me/avatar", "narrowed", "", nil); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), stored) {
		t.Errorf("alice's narrowed token reading her photo answered %d", w.Code)
	}
	// carol, administering the installation, reads it by login; bob does not, and is refused as
	// every route about a user refuses him, whoever he names.
	if w := in.ask(t, "GET", "/api/v1/users/alice/avatar", "carol", "", nil); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), stored) || w.Header().Get("Cache-Control") != "private, max-age=86400" {
		t.Errorf("carol reading alice's photo answered %d: %.100s", w.Code, w.Body)
	}
	for _, login := range []string{"alice", "bob", "nobody"} {
		if w := in.ask(t, "GET", "/api/v1/users/"+login+"/avatar", "bob", "", nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "you do not hold what this needs") {
			t.Errorf("bob reading %s's photo answered %d: %s", login, w.Code, w.Body)
		}
	}
	// No user, a login nobody can hold and a user holding no photo are one 404; and so is a caller
	// holding none.
	for _, login := range []string{"bob", "nobody", "Not_A_Login"} {
		if w := in.ask(t, "GET", "/api/v1/users/"+login+"/avatar", "carol", "", nil); w.Code != http.StatusNotFound || w.Body.String() != `{"error":"no user by that login, or they hold no photo"}`+"\n" {
			t.Errorf("carol reading %s's photo answered %d: %s", login, w.Code, w.Body)
		}
	}
	for _, who := range []string{"bob", "finance/nightly", "bootstrap"} {
		if w := in.ask(t, "GET", "/api/v1/me/avatar", who, "", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "you hold no photo") {
			t.Errorf("%s reading their own photo answered %d: %s", who, w.Code, w.Body)
		}
	}

	// What is refused leaves the photo as it was.
	for _, c := range []struct {
		who, kind string
		body      []byte
		status    int
		says      string
	}{
		{"alice", "text/plain", []byte("a photo"), http.StatusUnsupportedMediaType, `Content-Type is "text/plain"`},
		{"alice", "application/json", []byte(`{}`), http.StatusUnsupportedMediaType, "image/png or image/jpeg"},
		{"alice", "", []byte("a photo"), http.StatusUnsupportedMediaType, "image/png or image/jpeg"},
		{"alice", "image/gif", []byte("GIF89a"), http.StatusUnsupportedMediaType, `"image/gif"`},
		{"alice", "image/png", bytes.Repeat([]byte("x"), 1<<20+1), http.StatusRequestEntityTooLarge, "at most 1048576 bytes"},
		{"alice", "image/png", pngAnnouncing(4096, 4096), http.StatusUnprocessableEntity, "4096 by 4096 pixels"},
		{"alice", "image/png", []byte("not a photo at all"), http.StatusUnprocessableEntity, "not a PNG or a JPEG"},
		{"alice", "image/png", []byte{}, http.StatusBadRequest, "the request body is empty"},
		{"finance/nightly", "image/png", sent, http.StatusForbidden, "a service account has no profile or photo"},
		{"bootstrap", "image/png", sent, http.StatusForbidden, "the bootstrap token is nobody"},
		{"narrowed", "image/png", sent, http.StatusForbidden, "a token narrowed by a scope changes no profile or photo"},
	} {
		w := in.ask(t, "PUT", "/api/v1/me/avatar", c.who, c.kind, c.body)
		if w.Code != c.status || !strings.Contains(refusalOf(w), c.says) {
			t.Errorf("PUT a photo of %d bytes as %s, %q, answered %d: %s, want %d saying %q", len(c.body), c.who, c.kind, w.Code, w.Body, c.status, c.says)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/me/avatar", "alice", "", nil); !bytes.Equal(w.Body.Bytes(), stored) {
		t.Error("a refusal changed alice's photo")
	}

	// alice removes hers, twice, the second changing nothing; a body is refused, and so is
	// whoever has no photo to remove.
	if w := in.ask(t, "DELETE", "/api/v1/me/avatar", "alice", "application/json", []byte(`{"all":true}`)); w.Code != http.StatusBadRequest {
		t.Errorf("removing a photo with a body answered %d: %s", w.Code, w.Body)
	}
	for _, who := range []string{"finance/nightly", "bootstrap", "narrowed"} {
		if w := in.ask(t, "DELETE", "/api/v1/me/avatar", who, "", nil); w.Code != http.StatusForbidden {
			t.Errorf("%s removing a photo answered %d: %s", who, w.Code, w.Body)
		}
	}
	for range 2 {
		if w := in.ask(t, "DELETE", "/api/v1/me/avatar", "alice", "", nil); w.Code != http.StatusNoContent {
			t.Errorf("alice removing her photo answered %d: %s", w.Code, w.Body)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/me/avatar", "alice", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("alice's photo, removed, answered %d", w.Code)
	}
	if alice, raw := userIn(t, in.ask(t, "GET", "/api/v1/me", "alice", "", nil), true); alice.AvatarUpdatedAt != nil || !bytes.Contains(raw, []byte(`"avatar_updated_at":null`)) {
		t.Errorf("alice, her photo removed, reads as %s", raw)
	}

	// carol removes the one alice sets again; once, since there is none the second time, and bob
	// removes none.
	if w := in.ask(t, "PUT", "/api/v1/me/avatar", "alice", "image/jpeg", sent); w.Code != http.StatusNoContent {
		t.Fatalf("alice setting her photo again answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/users/alice/avatar", "bob", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("bob removing alice's photo answered %d: %s", w.Code, w.Body)
	}
	for i, want := range []int{http.StatusNoContent, http.StatusNotFound} {
		if w := in.ask(t, "DELETE", "/api/v1/users/alice/avatar", "carol", "", nil); w.Code != want {
			t.Errorf("carol removing alice's photo the %d time answered %d: %s", i+1, w.Code, w.Body)
		}
	}
	if w := in.ask(t, "DELETE", "/api/v1/users/nobody/avatar", "carol", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("carol removing nobody's photo answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "GET", "/api/v1/me/avatar", "alice", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("alice's photo, removed by carol, answered %d", w.Code)
	}

	recorded := in.entries(t, "user.avatar")
	wantRecorded := []string{
		`done alice alice {"height":256,"removed":false,"width":512}`,
		`done alice alice {"removed":true}`,
		`unchanged alice alice {"removed":true}`,
		`done alice alice {"height":256,"removed":false,"width":512}`,
		`done carol alice {"removed":true}`,
	}
	if strings.Join(recorded, "\n") != strings.Join(wantRecorded, "\n") {
		t.Errorf("the audit log recorded\n%s\nwant\n%s", strings.Join(recorded, "\n"), strings.Join(wantRecorded, "\n"))
	}
}
