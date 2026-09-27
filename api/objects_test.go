package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// The object routes, whose authorisation is the URL itself.

func withObjects(t *testing.T) (http.Handler, *artifact.Signed) {
	t.Helper()
	return withObjectsUnder(t, agk.Limits{})
}

// withObjectsUnder is withObjects under the limits an installation sets, which are the defaults
// when they are the zero value.
func withObjectsUnder(t *testing.T, limits agk.Limits) (http.Handler, *artifact.Signed) {
	t.Helper()
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key:    []byte("0123456789abcdef0123456789abcdef"),
		Base:   "https://agentiik.example.com/objects",
		Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	// DenyAll, so that what works here works because of the signature and of nothing else.
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, nil); err != nil {
		t.Fatal(err)
	}
	return rt, signed
}

func TestAnObjectRouteIsAuthorisedByItsURLAndByNothingElse(t *testing.T) {
	h, signed := withObjects(t)
	const content = "the whole of an invoice"
	sum := sha256.Sum256([]byte(content))
	key := artifact.Key("finance", hex.EncodeToString(sum[:]))
	until := time.Now().UTC().Add(time.Hour)

	put, err := signed.Presign(context.Background(), "PUT", key, "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}
	get, err := signed.Presign(context.Background(), "GET", key, "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}

	// The whole request is the URL, and the installation refuses every principal there is.
	if w := follow(t, h, "PUT", put, content); w.Code != http.StatusCreated {
		t.Fatalf("storing answered %d: %s", w.Code, w.Body)
	}
	w := follow(t, h, "GET", get, "")
	if w.Code != http.StatusOK || w.Body.String() != content {
		t.Fatalf("fetching answered %d: %q", w.Code, w.Body)
	}

	// And without one there is nothing: the route is public and the signature is what
	// authorises it, so an unsigned request reaches the handler and is refused there.
	if w := follow(t, h, "GET", "https://agentiik.example.com/objects/"+key, ""); w.Code != http.StatusForbidden {
		t.Errorf("an unsigned fetch answered %d", w.Code)
	}
}

// What the store refuses, and what each refusal reads as over HTTP.
func TestWhatAnObjectRouteAnswers(t *testing.T) {
	h, signed := withObjects(t)
	sum := sha256.Sum256([]byte("an invoice nobody stored"))
	key := artifact.Key("finance", hex.EncodeToString(sum[:]))
	until := time.Now().UTC().Add(time.Hour)

	get, err := signed.Presign(context.Background(), "GET", key, "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}
	// A signed URL for an object that is not there says so. The caller already knew the
	// key, since it is in the URL it was handed, so there is no oracle in saying it.
	if w := follow(t, h, "GET", get, ""); w.Code != http.StatusNotFound {
		t.Errorf("fetching an absent object answered %d", w.Code)
	}

	put, err := signed.Presign(context.Background(), "PUT", key, "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, h, "PUT", put, "bytes that are not that object"); w.Code != http.StatusBadRequest {
		t.Errorf("storing the wrong bytes answered %d", w.Code)
	}

	// Nothing a refusal writes is worth caching, and none of them carries a body: the
	// caller is a machine following a URL.
	w := follow(t, h, "GET", "https://agentiik.example.com/objects/"+key, "")
	if w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Errorf("a refusal says %q and writes %q", w.Header().Get("Cache-Control"), w.Body)
	}
}

// A policy stores whatever the task made under its prefix, and the bytes are still held to the
// digest the key names, exactly as a presigned PUT holds them: a policy that stored any bytes under
// any digest would let a task write under a digest somebody else's envelope already names.
func TestAPostedObjectIsHashedAsItArrives(t *testing.T) {
	h, signed := withObjects(t)
	policy, err := signed.Policy(t.Context(), "finance", "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf([]byte(content)))
	if w := posted(t, h, policy.URL, policy.Fields, key, content); w.Code != http.StatusCreated {
		t.Fatalf("posting what the key names answered %d: %s", w.Code, w.Body)
	}
	// And what is stored is those bytes, fetched back through a URL of their own.
	get, err := signed.Presign(t.Context(), "GET", key, "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, h, "GET", get, ""); w.Code != http.StatusOK || w.Body.String() != content {
		t.Errorf("fetching what was posted answered %d: %q", w.Code, w.Body)
	}

	// Bytes that are not the object their key names are refused, and nothing is left under the
	// key for the next reader to fetch and reject.
	other := artifact.Key("finance", digestOf([]byte("an invoice nobody made")))
	if w := posted(t, h, policy.URL, policy.Fields, other, "something else entirely"); w.Code != http.StatusBadRequest {
		t.Errorf("posting bytes that are not the object answered %d", w.Code)
	}
	if _, err := signed.Fetch(t.Context(), other); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused post left something behind: %v", err)
	}
}

// What bounds a posted file is artifact_max_bytes and nothing else. What comes before the file is
// read before any signature is checked and is bounded far below that, and a file held to the same
// bound would be an output of a few dozen kilobytes at most, where an artifact may be gigabytes.
func TestAPostedFileIsBoundedByArtifactMaxBytesAndNotByTheFormAroundIt(t *testing.T) {
	limits := agk.DefaultLimits()
	limits.ArtifactMaxBytes = 1 << 20
	h, signed := withObjectsUnder(t, limits)
	until := time.Now().UTC().Add(time.Hour)
	policy, err := signed.Policy(t.Context(), "finance", "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}

	// A file of exactly artifact_max_bytes, many times what may come before it, is stored whole.
	largest := strings.Repeat("x", int(limits.ArtifactMaxBytes))
	key := artifact.Key("finance", digestOf([]byte(largest)))
	if w := posted(t, h, policy.URL, policy.Fields, key, largest); w.Code != http.StatusCreated {
		t.Fatalf("posting a file of artifact_max_bytes answered %d: %s", w.Code, w.Body)
	}
	get, err := signed.Presign(t.Context(), "GET", key, "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, h, "GET", get, ""); w.Code != http.StatusOK || w.Body.String() != largest {
		t.Errorf("fetching what was posted answered %d with %d bytes", w.Code, w.Body.Len())
	}

	// One byte more is refused as too large, although it is the object its key names, and
	// nothing is left under the key.
	larger := largest + "x"
	key = artifact.Key("finance", digestOf([]byte(larger)))
	if w := posted(t, h, policy.URL, policy.Fields, key, larger); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("posting a file above artifact_max_bytes answered %d", w.Code)
	}
	if _, err := signed.Fetch(t.Context(), key); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a post refused as too large left something behind: %v", err)
	}
}

// What a posted form is refused for, and what each refusal reads as over HTTP.
func TestWhatAPostedFormIsRefused(t *testing.T) {
	h, signed := withObjects(t)
	policy, err := signed.Policy(t.Context(), "finance", "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const content = "the whole of an invoice"
	digest := digestOf([]byte(content))

	// Anything the policy does not allow is the one answer a URL gets when it is not signed
	// for what it asks.
	for _, c := range []struct {
		name   string
		to     string
		fields map[string]string
		key    string
	}{
		{"a key in another namespace", policy.URL, policy.Fields, artifact.Key("ops", digest)},
		{"a form posted to another namespace", "https://agentiik.example.com/objects/ops", policy.Fields, artifact.Key("ops", digest)},
		{"a key that is the prefix and not a digest", policy.URL, policy.Fields, policy.KeyPrefix + "invoice.pdf"},
		{"a form with no policy in it", policy.URL, nil, artifact.Key("finance", digest)},
		{"a form with no key", policy.URL, policy.Fields, ""},
	} {
		w := posted(t, h, c.to, c.fields, c.key, content)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
			t.Errorf("%s says %q and writes %q", c.name, w.Header().Get("Cache-Control"), w.Body)
		}
	}

	// And a form that is not one, or that is one with nothing to store, is a bad request.
	var fields bytes.Buffer
	form := multipart.NewWriter(&fields)
	for _, name := range slices.Sorted(maps.Keys(policy.Fields)) {
		form.WriteField(name, policy.Fields[name])
	}
	form.WriteField("key", artifact.Key("finance", digest))
	form.Close()

	var padded bytes.Buffer
	crowded := multipart.NewWriter(&padded)
	crowded.WriteField("padding", strings.Repeat("x", 128<<10))
	crowded.WriteField("key", artifact.Key("finance", digest))
	file, _ := crowded.CreateFormFile("file", "object")
	file.Write([]byte(content))
	crowded.Close()

	for _, c := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"a body that is not a form", "application/octet-stream", []byte(content)},
		{"a form with no file", form.FormDataContentType(), fields.Bytes()},
		{"a form carrying more before its file than a policy and a key", crowded.FormDataContentType(), padded.Bytes()},
	} {
		r := httptest.NewRequest("POST", policy.URL, bytes.NewReader(c.body))
		r.Header.Set("Content-Type", c.contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
	}
}

// posted is a form as a runner posts it with a policy: the fields as they were given, then the key,
// then the file, last.
func posted(t *testing.T, h http.Handler, to string, fields map[string]string, key, content string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if err := form.WriteField(name, fields[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.WriteField("key", key); err != nil {
		t.Fatal(err)
	}
	file, err := form.CreateFormFile("file", "object")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", to, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The route says out loud what authorises it, which is what the guard demands of a public one.
func TestTheObjectRoutesSayWhatAuthorisesThem(t *testing.T) {
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, nil); err != nil {
		t.Fatal(err)
	}
	served := 0
	for _, r := range rt.Routes() {
		if !strings.HasPrefix(r.Pattern, "/objects") {
			continue
		}
		served++
		says := "presigned URL"
		if r.Method == "POST" {
			says = "signed policy"
		}
		if !r.Public || !strings.Contains(r.Why, says) {
			t.Errorf("%s %s says %q", r.Method, r.Pattern, r.Why)
		}
	}
	if served != 3 {
		t.Errorf("the store serves %d object routes, and a URL's GET and PUT and a policy's POST are three", served)
	}

	// A presigner is not optional: an object route with nothing to check a signature
	// against would serve every object to anybody.
	if _, err := api.NewObjects(rt, nil, nil); err == nil {
		t.Error("an object route was registered with nothing to check a signature against")
	}
}

// An installation serves the object routes on the router the rest of the API is on, since a tree
// a push stored is fetched through a URL a redemption minted, and one surface answers both. Under
// /api/v1 the object routes could not be registered beside the run list at all: net/http panicked
// at start-up, and no test had put the three sets of routes on one router to see it.
func TestTheObjectRoutesShareARouterWithTheRestOfTheAPI(t *testing.T) {
	pool, _ := dbtest.Open(t)
	objects := artifact.Dir(t.TempDir())
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "alice"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: objects}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool, Objects: objects, URLs: signed}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, nil); err != nil {
		t.Fatal(err)
	}

	// And each route answers what it is for, the path the two used to share included.
	const content = "a file of the tree"
	sum := sha256.Sum256([]byte(content))
	key := artifact.Key("finance", hex.EncodeToString(sum[:]))
	put, err := signed.Presign(t.Context(), "PUT", key, "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, rt, "PUT", put, content); w.Code != http.StatusCreated {
		t.Errorf("storing through the shared router answered %d: %s", w.Code, w.Body)
	}
	policy, err := signed.Policy(t.Context(), "finance", "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const made = "a file the task made"
	if w := posted(t, rt, policy.URL, policy.Fields, artifact.Key("finance", digestOf([]byte(made))), made); w.Code != http.StatusCreated {
		t.Errorf("posting through the shared router answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, rt, "GET", "/api/v1/objects/runs", "alice", nil); w.Code != http.StatusOK {
		t.Errorf("the runs of a namespace named objects answered %d: %s", w.Code, w.Body)
	}
}

func follow(t *testing.T, h http.Handler, method, raw, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, raw, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// "max_artifact_bytes: Total live artifact storage; beyond it, new writes are refused." The store
// holds every write to it, a policy's form and a presigned PUT alike, and answers one that would take
// the namespace past its quota 507 with no body, storing nothing of it. An object the namespace
// already holds takes no room, and a namespace that sets no quota is refused nothing.
func TestAWritePastMaxArtifactBytesIsAnswered507(t *testing.T) {
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(),
		`insert into namespaces (name, max_artifact_bytes) values ('finance', 3000), ('team-ops', null)`); err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, pool); err != nil {
		t.Fatal(err)
	}
	const run = "01JMZ8W4K2R7Q0E3N5T9"
	until := time.Now().UTC().Add(time.Hour)
	policy, err := signed.Policy(t.Context(), "finance", run, until)
	if err != nil {
		t.Fatal(err)
	}
	object := func(c byte) string { return strings.Repeat(string(c), 1000) }
	post := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		return posted(t, rt, policy.URL, policy.Fields, artifact.Key("finance", digestOf([]byte(content))), content)
	}

	// Bytes that are not the object their key names are refused, and the room made for them is
	// given back: 2,000 bytes of it, which would leave no room for what follows.
	if w := posted(t, rt, policy.URL, policy.Fields, artifact.Key("finance", digestOf([]byte("another"))), strings.Repeat("x", 2000)); w.Code != http.StatusBadRequest {
		t.Fatalf("bytes that are not their key's object answered %d", w.Code)
	}

	// Two objects of 1,000 bytes fit, each given room at its form's length and counted at its
	// size once it is in; a third would take the namespace to 3,000 and its form past it.
	for _, c := range []byte{'a', 'b'} {
		if w := post(object(c)); w.Code != http.StatusCreated {
			t.Fatalf("an object of 1,000 bytes within the quota answered %d", w.Code)
		}
	}
	third := object('c')
	w := post(third)
	if w.Code != http.StatusInsufficientStorage || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("an object past the quota answered %d, %q, %q", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	get, err := signed.Presign(t.Context(), "GET", artifact.Key("finance", digestOf([]byte(third))), run, until)
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, rt, "GET", get, ""); w.Code != http.StatusNotFound {
		t.Errorf("fetching the object refused answered %d, and nothing of it was to be stored", w.Code)
	}
	// Posting again what the namespace holds takes no room.
	if w := post(object('a')); w.Code != http.StatusCreated {
		t.Errorf("an object the namespace holds, posted again, answered %d", w.Code)
	}

	// A presigned PUT carries the object and nothing else, so it is given room at exactly its
	// size: the last 1,000 bytes fit, and one byte more does not.
	put := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		url, err := signed.Presign(t.Context(), "PUT", artifact.Key("finance", digestOf([]byte(content))), run, until)
		if err != nil {
			t.Fatal(err)
		}
		return follow(t, rt, "PUT", url, content)
	}
	if w := put(third); w.Code != http.StatusCreated {
		t.Errorf("the last 1,000 bytes of the quota, put, answered %d", w.Code)
	}
	if w := put("d"); w.Code != http.StatusInsufficientStorage {
		t.Errorf("one byte past the quota, put, answered %d", w.Code)
	}

	// team-ops sets no quota.
	elsewhere, err := signed.Policy(t.Context(), "team-ops", run, until)
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("e", 10000)
	if w := posted(t, rt, elsewhere.URL, elsewhere.Fields, artifact.Key("team-ops", digestOf([]byte(large))), large); w.Code != http.StatusCreated {
		t.Errorf("an object in a namespace with no quota answered %d", w.Code)
	}
}

// Every write is recorded as under way before its bytes are read, in a namespace with no quota as in
// one with, until the collection's grace past its policy, so that the collector leaves its object
// alone until the result that references it has been heard.
func TestEveryWriteIsKeptFromTheCollectorWhileItLasts(t *testing.T) {
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, pool); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	policy, err := signed.Policy(t.Context(), "team-ops", "01JMZ8W4K2R7Q0E3N5T9", until)
	if err != nil {
		t.Fatal(err)
	}
	content := "the bytes of an artifact"
	digest := digestOf([]byte(content))
	if w := posted(t, rt, policy.URL, policy.Fields, artifact.Key("team-ops", digest), content); w.Code != http.StatusCreated {
		t.Fatalf("the write answered %d", w.Code)
	}
	var held int64
	var lasts time.Time
	if err := conn.QueryRow(t.Context(),
		`select bytes, until from artifact_uploads where namespace = 'team-ops' and digest = 'sha256:' || $1`, digest).Scan(&held, &lasts); err != nil {
		t.Fatalf("no write of the object is recorded: %s", err)
	}
	if held != 0 || !lasts.Equal(until.Add(db.DefaultGrace)) {
		t.Errorf("the write is recorded holding %d bytes until %s, want nothing held until %s", held, lasts, until.Add(db.DefaultGrace))
	}

	// A write whose bytes are refused leaves nothing to keep from the collector.
	refused := digestOf([]byte("what the key names"))
	if w := posted(t, rt, policy.URL, policy.Fields, artifact.Key("team-ops", refused), "other bytes"); w.Code != http.StatusBadRequest {
		t.Fatalf("bytes that are not their key's object answered %d", w.Code)
	}
	var left int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from artifact_uploads where digest = 'sha256:' || $1`, refused).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("a write whose bytes were refused left %d rows", left)
	}
}

// A write of no stated length is given the room left, up to artifact_max_bytes, and held to it as
// its bytes arrive: past it, nothing is stored and the write is answered 507.
func TestAWriteOfNoStatedLengthIsHeldToTheRoomLeft(t *testing.T) {
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into namespaces (name, max_artifact_bytes) values ('finance', 1000), ('legal', 1000)`); err != nil {
		t.Fatal(err)
	}
	unstated := func(h http.Handler, signed *artifact.Signed, namespace, content string) int {
		t.Helper()
		key := artifact.Key(namespace, digestOf([]byte(content)))
		url, err := signed.Presign(t.Context(), "PUT", key, "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// A reader net/http cannot measure, so the request states no length.
		r := httptest.NewRequest("PUT", url, io.MultiReader(strings.NewReader(content)))
		if r.ContentLength != -1 {
			t.Fatalf("the request states a length of %d", r.ContentLength)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// Under the default artifact_max_bytes, 5 GiB, one byte fits a namespace of 1,000.
	defaulted, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, defaulted, pool); err != nil {
		t.Fatal(err)
	}
	if code := unstated(rt, defaulted, "legal", "a"); code != http.StatusCreated {
		t.Errorf("one byte of no stated length, under the default artifact_max_bytes, answered %d", code)
	}

	// artifact_max_bytes off, which is what leaves the room left as the only bound.
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
		Limits: agk.Limits{InlineMaxBytes: agk.DefaultInlineMaxBytes, EnvelopeMaxBytes: agk.DefaultEnvelopeMaxBytes, MaxItems: agk.DefaultMaxItems},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err = api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, pool); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour)
	for _, c := range []struct {
		size int
		want int
	}{
		{1001, http.StatusInsufficientStorage},
		{1000, http.StatusCreated},
	} {
		content := strings.Repeat("a", c.size)
		if code := unstated(rt, signed, "finance", content); code != c.want {
			t.Errorf("%d bytes of no stated length with 1,000 left answered %d, want %d", c.size, code, c.want)
		}
		get, err := signed.Presign(t.Context(), "GET", artifact.Key("finance", digestOf([]byte(content))), "01JMZ8W4K2R7Q0E3N5T9", until)
		if err != nil {
			t.Fatal(err)
		}
		if w := follow(t, rt, "GET", get, ""); (w.Code == http.StatusOK) != (c.want == http.StatusCreated) {
			t.Errorf("%d bytes answered %d, and are fetched with %d", c.size, c.want, w.Code)
		}
	}
}

// A write whose length is past artifact_max_bytes is an object the store refuses as too large,
// 413, and it is given the room of the most an object may be rather than refused as the quota's
// for a length no object reaches.
func TestAWritePastArtifactMaxBytesIsTooLargeRatherThanPastTheQuota(t *testing.T) {
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into namespaces (name, max_artifact_bytes) values ('finance', 1100)`); err != nil {
		t.Fatal(err)
	}
	limits := agk.DefaultLimits()
	limits.ArtifactMaxBytes = 1000
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects", Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed, pool); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("a", 1200)
	url, err := signed.Presign(t.Context(), "PUT", artifact.Key("finance", digestOf([]byte(content))), "01JMZ8W4K2R7Q0E3N5T9", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if w := follow(t, rt, "PUT", url, content); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("1,200 bytes past an artifact_max_bytes of 1,000, in a namespace of 1,100, answered %d", w.Code)
	}
}
