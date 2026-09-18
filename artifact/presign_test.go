package artifact_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// A presigned URL does one thing, to one object, for one run, until one instant.

const base = "https://agentiik.example.com/api/v1/objects"

var signingKey = []byte("0123456789abcdef0123456789abcdef")

func signed(t *testing.T) (*artifact.Signed, artifact.Objects) {
	t.Helper()
	objects := artifact.Dir(t.TempDir())
	s, err := artifact.NewSigned(objects, artifact.SignedOptions{Key: signingKey, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	return s, objects
}

// served follows a URL the way a runner does: it is the whole of the request.
func served(t *testing.T, s *artifact.Signed, method, raw, body string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimPrefix(u.Path, "/api/v1/objects/")
	r := httptest.NewRequest(method, u.RequestURI(), strings.NewReader(body))
	w := httptest.NewRecorder()
	s.Serve(w, r, key)
	return w
}

func TestAPresignedURLStoresWhatItNamesAndThenHandsItBack(t *testing.T) {
	s, _ := signed(t)
	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf([]byte(content)))
	until := time.Now().UTC().Add(time.Hour)

	put, err := s.Presign(context.Background(), http.MethodPut, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}
	if w := served(t, s, http.MethodPut, put, content); w.Code != http.StatusCreated {
		t.Fatalf("storing answered %d", w.Code)
	}

	get, err := s.Presign(context.Background(), http.MethodGet, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}
	w := served(t, s, http.MethodGet, get, "")
	if w.Code != http.StatusOK || w.Body.String() != content {
		t.Fatalf("fetching answered %d: %q", w.Code, w.Body)
	}
}

// The key is the digest of the content, so a URL for one object cannot be used to store other
// bytes under a digest somebody else's envelope already names.
func TestAPresignedPutRefusesBytesThatAreNotTheObject(t *testing.T) {
	s, objects := signed(t)
	key := artifact.Key("finance", digestOf([]byte("the whole of an invoice")))

	put, err := s.Presign(context.Background(), http.MethodPut, key, "01K5RUNIDENTIFIER",
		time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if w := served(t, s, http.MethodPut, put, "something else entirely"); w.Code != http.StatusBadRequest {
		t.Fatalf("storing the wrong bytes answered %d", w.Code)
	}

	// And nothing was left behind for the next reader to fetch and reject.
	if held, err := objects.Has(context.Background(), key); err != nil || held {
		t.Errorf("the object is held after a refused write: %v %v", held, err)
	}
}

func TestAURLDoesOneThingToOneObject(t *testing.T) {
	s, _ := signed(t)
	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf([]byte(content)))
	other := artifact.Key("finance", digestOf([]byte("another invoice")))
	until := time.Now().UTC().Add(time.Hour)

	get, err := s.Presign(context.Background(), http.MethodGet, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		method string
		url    string
	}{
		{"a fetching URL used to store", http.MethodPut, get},
		{"the same signature against another key", http.MethodGet, strings.Replace(get, key, other, 1)},
		{"a signature somebody edited", http.MethodGet, get[:len(get)-4] + "0000"},
		{"a URL with no signature at all", http.MethodGet, base + "/" + key},
		{"a URL whose expiry somebody moved", http.MethodGet,
			strings.Replace(get, "expires="+expiryOf(t, get), "expires=9999999999", 1)},
	} {
		if w := served(t, s, c.method, c.url, content); w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d", c.name, w.Code)
		}
	}
}

// "scoped to one run" and to one instant: a URL that never stopped working would be the standing
// credential this exists to avoid.
func TestAPresignedURLStopsWorking(t *testing.T) {
	objects := artifact.Dir(t.TempDir())
	now := time.Now().UTC()
	s, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: signingKey, Base: base, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key("finance", digestOf([]byte("the whole of an invoice")))

	get, err := s.Presign(context.Background(), http.MethodGet, key, "01K5RUNIDENTIFIER", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Not yet: the object is simply not there.
	if w := served(t, s, http.MethodGet, get, ""); w.Code != http.StatusNotFound {
		t.Fatalf("a live URL for an absent object answered %d", w.Code)
	}
	now = now.Add(2 * time.Minute)
	if w := served(t, s, http.MethodGet, get, ""); w.Code != http.StatusForbidden {
		t.Errorf("an expired URL answered %d", w.Code)
	}
}

func TestWhatCannotBePresigned(t *testing.T) {
	s, _ := signed(t)
	key := artifact.Key("finance", digestOf([]byte("the whole of an invoice")))
	until := time.Now().UTC().Add(time.Hour)

	for _, c := range []struct {
		name   string
		method string
		key    string
		run    agk.RunID
		until  time.Time
	}{
		{"a method that is neither", http.MethodDelete, key, "01K5RUNIDENTIFIER", until},
		{"no run", http.MethodGet, key, "", until},
		{"no expiry", http.MethodGet, key, "01K5RUNIDENTIFIER", time.Time{}},
		{"a key that leaves its root", http.MethodGet, "finance/../other/sha256/x", "01K5RUNIDENTIFIER", until},
	} {
		if _, err := s.Presign(context.Background(), c.method, c.key, c.run, c.until); err == nil {
			t.Errorf("%s was signed", c.name)
		}
	}

	// And a presigner that could not sign anything worth having is refused at the start.
	for _, c := range []struct {
		name string
		o    artifact.SignedOptions
	}{
		{"a short key", artifact.SignedOptions{Key: []byte("too short"), Base: base}},
		{"no base", artifact.SignedOptions{Key: signingKey}},
		{"a base that is not a URL", artifact.SignedOptions{Key: signingKey, Base: "/api/v1/objects"}},
	} {
		if _, err := artifact.NewSigned(artifact.Dir(t.TempDir()), c.o); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

func expiryOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("expires")
}
