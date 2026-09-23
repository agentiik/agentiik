package artifact_test

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// A presigned URL does one thing, to one object, for one run, until one instant.

const base = "https://agentiik.example.com/objects"

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

// parts takes a URL apart the way whoever serves it does: a key out of the path, and the rest out
// of the query.
func parts(t *testing.T, raw string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/objects/"), u.Query()
}

func TestAPresignedURLStoresWhatItNamesAndThenHandsItBack(t *testing.T) {
	s, _ := signed(t)
	const content = "the whole of an invoice"
	key := artifact.Key("finance", digestOf([]byte(content)))
	until := time.Now().UTC().Add(time.Hour)

	put, err := s.Presign(context.Background(), artifact.MethodPut, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}
	path, query := parts(t, put)
	if _, err := s.Check(artifact.MethodPut, path, query); err != nil {
		t.Fatalf("the URL it minted was refused: %s", err)
	}
	if err := s.Store(context.Background(), path, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	get, err := s.Presign(context.Background(), artifact.MethodGet, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}
	path, query = parts(t, get)
	run, err := s.Check(artifact.MethodGet, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if run != "01K5RUNIDENTIFIER" {
		t.Errorf("the URL is signed for run %q", run)
	}
	rc, err := s.Fetch(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	back, _ := io.ReadAll(rc)
	if string(back) != content {
		t.Errorf("what came back is %q", back)
	}
}

// The key is the digest of the content, so a URL for one object cannot be used to store other
// bytes under a digest somebody else's envelope already names.
func TestAPresignedPutRefusesBytesThatAreNotTheObject(t *testing.T) {
	s, objects := signed(t)
	key := artifact.Key("finance", digestOf([]byte("the whole of an invoice")))

	err := s.Store(context.Background(), key, strings.NewReader("something else entirely"))
	if !errors.Is(err, artifact.ErrWrongDigest) {
		t.Fatalf("storing the wrong bytes answered %v", err)
	}
	// And nothing was left behind for the next reader to fetch and reject.
	if held, err := objects.Has(context.Background(), key); err != nil || held {
		t.Errorf("the object is held after a refused write: %v %v", held, err)
	}
}

func TestAURLDoesOneThingToOneObject(t *testing.T) {
	s, _ := signed(t)
	key := artifact.Key("finance", digestOf([]byte("the whole of an invoice")))
	other := artifact.Key("finance", digestOf([]byte("another invoice")))
	until := time.Now().UTC().Add(time.Hour)

	get, err := s.Presign(context.Background(), artifact.MethodGet, key, "01K5RUNIDENTIFIER", until)
	if err != nil {
		t.Fatal(err)
	}
	path, query := parts(t, get)

	edited := func(name, value string) url.Values {
		q := url.Values{}
		for k, v := range query {
			q[k] = append([]string(nil), v...)
		}
		q.Set(name, value)
		return q
	}

	for _, c := range []struct {
		name   string
		method string
		key    string
		query  url.Values
	}{
		{"a fetching URL used to store", artifact.MethodPut, path, query},
		{"the same signature against another key", artifact.MethodGet, other, query},
		{"a signature somebody edited", artifact.MethodGet, path,
			edited("signature", strings.Repeat("0", 64))},
		{"a URL with no signature at all", artifact.MethodGet, path, url.Values{}},
		{"a URL whose expiry somebody moved", artifact.MethodGet, path,
			edited("expires", "9999999999")},
		{"a URL claiming another run", artifact.MethodGet, path, edited("run", "01K5OTHERRUN")},
	} {
		if _, err := s.Check(c.method, c.key, c.query); !errors.Is(err, artifact.ErrNotSigned) {
			t.Errorf("%s answered %v", c.name, err)
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

	get, err := s.Presign(context.Background(), artifact.MethodGet, key, "01K5RUNIDENTIFIER", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	path, query := parts(t, get)
	if _, err := s.Check(artifact.MethodGet, path, query); err != nil {
		t.Fatalf("a live URL was refused: %s", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Check(artifact.MethodGet, path, query); !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("an expired URL answered %v", err)
	}
}

// A policy is one form for everything a task makes: it writes any object under its namespace's
// prefix, for its run, until its instant, and nothing anywhere else.
func TestAPolicyAllowsItsPrefixAndNothingElse(t *testing.T) {
	objects := artifact.Dir(t.TempDir())
	now := time.Now().UTC()
	s, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: signingKey, Base: base, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Policy(context.Background(), "finance", "01K5RUNIDENTIFIER", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != base+"/finance" || p.KeyPrefix != "finance/sha256/" {
		t.Errorf("the policy is posted to %s under %s", p.URL, p.KeyPrefix)
	}
	fields := url.Values{}
	for name, value := range p.Fields {
		fields.Set(name, value)
	}

	// Any object under the prefix, because none of them existed when the policy was signed.
	for _, content := range []string{"the whole of an invoice", "another invoice"} {
		key := artifact.Key("finance", digestOf([]byte(content)))
		run, err := s.CheckPolicy("finance", key, fields)
		if err != nil {
			t.Fatalf("the policy it signed was refused for %s: %s", key, err)
		}
		if run != "01K5RUNIDENTIFIER" {
			t.Errorf("the policy is signed for run %q", run)
		}
		if err := s.Store(context.Background(), key, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}

	digest := digestOf([]byte("the whole of an invoice"))
	key := artifact.Key("finance", digest)
	edited := func(name, value string) url.Values {
		q := url.Values{}
		for k, v := range fields {
			q[k] = append([]string(nil), v...)
		}
		q.Set(name, value)
		return q
	}
	for _, c := range []struct {
		name      string
		namespace string
		key       string
		fields    url.Values
	}{
		{"a key outside the prefix", "finance", "finance/other/" + digest, fields},
		{"a key in another namespace", "finance", artifact.Key("ops", digest), fields},
		{"the form posted for another namespace", "ops", artifact.Key("ops", digest), fields},
		{"a key that is more than a digest", "finance", key + "/x", fields},
		{"a key that is less than a digest", "finance", artifact.Key("finance", digest[:63]), fields},
		{"a digest written in capitals", "finance", artifact.Key("finance", strings.ToUpper(digest)), fields},
		{"a key that climbs out of the prefix", "finance", "finance/sha256/../../ops/sha256/" + digest, fields},
		{"a form claiming another run", "finance", key, edited("run", "01K5OTHERRUN")},
		{"a form whose expiry somebody moved", "finance", key, edited("expires", "9999999999")},
		{"a signature somebody edited", "finance", key, edited("signature", strings.Repeat("0", 64))},
		{"a form with no signature at all", "finance", key, url.Values{}},
	} {
		if _, err := s.CheckPolicy(c.namespace, c.key, c.fields); !errors.Is(err, artifact.ErrNotSigned) {
			t.Errorf("%s answered %v", c.name, err)
		}
	}

	// A URL is not a policy and a policy is not a URL, although both are signed with one key.
	put, err := s.Presign(context.Background(), artifact.MethodPut, key, "01K5RUNIDENTIFIER", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	path, query := parts(t, put)
	if _, err := s.CheckPolicy("finance", path, query); !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("a URL's query posted as a policy answered %v", err)
	}
	for _, method := range []string{artifact.MethodPut, artifact.MethodPost} {
		if _, err := s.Check(method, key, fields); !errors.Is(err, artifact.ErrNotSigned) {
			t.Errorf("a policy followed as a %s URL answered %v", method, err)
		}
	}

	// And it stops working with the grant it was answered with.
	now = now.Add(2 * time.Minute)
	if _, err := s.CheckPolicy("finance", key, fields); !errors.Is(err, artifact.ErrNotSigned) {
		t.Errorf("an expired policy answered %v", err)
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
		{"a method that is neither", "DELETE", key, "01K5RUNIDENTIFIER", until},
		{"no run", artifact.MethodGet, key, "", until},
		{"no expiry", artifact.MethodGet, key, "01K5RUNIDENTIFIER", time.Time{}},
		{"a key that leaves its root", artifact.MethodGet, "finance/../other/sha256/x", "01K5RUNIDENTIFIER", until},
	} {
		if _, err := s.Presign(context.Background(), c.method, c.key, c.run, c.until); err == nil {
			t.Errorf("%s was signed", c.name)
		}
	}

	// A policy is refused for the same reasons, and for a namespace that could not be the first
	// segment of a key, since its prefix would reach into another namespace's.
	for _, c := range []struct {
		name      string
		namespace string
		run       agk.RunID
		until     time.Time
	}{
		{"a policy for no run", "finance", "", until},
		{"a policy with no expiry", "finance", "01K5RUNIDENTIFIER", time.Time{}},
		{"a policy for no namespace", "", "01K5RUNIDENTIFIER", until},
		{"a policy for a namespace carrying a separator", "finance/sha256/..", "01K5RUNIDENTIFIER", until},
	} {
		if _, err := s.Policy(context.Background(), c.namespace, c.run, c.until); err == nil {
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
		{"a base that is not a URL", artifact.SignedOptions{Key: signingKey, Base: "/objects"}},
	} {
		if _, err := artifact.NewSigned(artifact.Dir(t.TempDir()), c.o); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}
