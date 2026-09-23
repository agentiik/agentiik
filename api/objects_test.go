package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	if _, err := api.NewObjects(rt, signed); err != nil {
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
	if _, err := api.NewObjects(rt, signed); err != nil {
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
	if _, err := api.NewObjects(rt, nil); err == nil {
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
	if _, err := api.NewObjects(rt, signed); err != nil {
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
