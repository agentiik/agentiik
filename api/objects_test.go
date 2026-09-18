package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
)

// The object routes, whose authorisation is the URL itself.

func withObjects(t *testing.T) (http.Handler, *artifact.Signed) {
	t.Helper()
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key:  []byte("0123456789abcdef0123456789abcdef"),
		Base: "https://agentiik.example.com/api/v1/objects",
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
	if w := follow(t, h, "GET", "https://agentiik.example.com/api/v1/objects/"+key, ""); w.Code != http.StatusForbidden {
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
	w := follow(t, h, "GET", "https://agentiik.example.com/api/v1/objects/"+key, "")
	if w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Errorf("a refusal says %q and writes %q", w.Header().Get("Cache-Control"), w.Body)
	}
}

// The route says out loud what authorises it, which is what the guard demands of a public one.
func TestTheObjectRoutesSayWhatAuthorisesThem(t *testing.T) {
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := artifact.NewSigned(artifact.Dir(t.TempDir()), artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/api/v1/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, signed); err != nil {
		t.Fatal(err)
	}
	for _, r := range rt.Routes() {
		if !strings.HasPrefix(r.Pattern, "/api/v1/objects") {
			continue
		}
		if !r.Public || !strings.Contains(r.Why, "presigned URL") {
			t.Errorf("%s %s says %q", r.Method, r.Pattern, r.Why)
		}
	}

	// A presigner is not optional: an object route with nothing to check a signature
	// against would serve every object to anybody.
	if _, err := api.NewObjects(rt, nil); err == nil {
		t.Error("an object route was registered with nothing to check a signature against")
	}
}

func follow(t *testing.T, h http.Handler, method, raw, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, raw, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
