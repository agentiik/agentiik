package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	apiserver "github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
)

// objectStore is the built-in object store, the API's own /objects route over a directory, which
// is what a runner reaches through the URLs and the policy a redemption hands it.
type objectStore struct {
	signed  *artifact.Signed
	objects artifact.Objects
	gets    atomic.Int64
}

const storeRun agk.RunID = "01JMZ8V1P9C4"

func newObjectStore(t *testing.T) *objectStore {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	s := &objectStore{objects: artifact.Dir(t.TempDir())}
	signed, err := artifact.NewSigned(s.objects, artifact.SignedOptions{
		Key:  []byte("0123456789abcdef0123456789abcdef"),
		Base: "http://" + srv.Listener.Addr().String() + "/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := apiserver.NewRouter(apiserver.DenyAll{}, func(*http.Request) (apiserver.Principal, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apiserver.NewObjects(rt, signed); err != nil {
		t.Fatal(err)
	}
	s.signed = signed
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.gets.Add(1)
		}
		rt.ServeHTTP(w, r)
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return s
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// put stores bytes under their digest in finance and answers the digest.
func (s *objectStore) put(t *testing.T, b []byte) string {
	t.Helper()
	d := sha(b)
	if err := s.signed.Store(t.Context(), artifact.Key("finance", d), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	return d
}

// tamper stores other bytes under a digest, which the built-in store would never do and a store
// that broke its one promise would.
func (s *objectStore) tamper(t *testing.T, digest string, b []byte) {
	t.Helper()
	if err := s.objects.Put(t.Context(), artifact.Key("finance", digest), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
}

// url is a presigned GET for one object of finance.
func (s *objectStore) url(t *testing.T, digest string) string {
	t.Helper()
	u, err := s.signed.Presign(t.Context(), artifact.MethodGet, artifact.Key("finance", digest), storeRun, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (s *objectStore) uploads(t *testing.T) Uploads {
	t.Helper()
	p, err := s.signed.Policy(t.Context(), "finance", storeRun, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return Uploads{URL: p.URL, Fields: p.Fields, KeyPrefix: p.KeyPrefix}
}

// file is one file of a tree as a test lays it down: its bytes and its mode.
type file struct {
	content string
	mode    string
}

// taskFor is a message of one task of finance and the redemption the API would answer it with,
// every input envelope and every tree file stored in s first.
func (s *objectStore) taskFor(t *testing.T, inputs map[agk.Port]agk.Envelope, tree map[string]file, secrets []RedeemedSecret) (bus.TaskMessage, Redemption) {
	t.Helper()
	m := bus.TaskMessage{
		TaskID:         "01JMZ8V1PC7K3M0QY4B8ZR6TDN",
		IdempotencyKey: string(storeRun) + "/invoice/1",
		RunID:          string(storeRun),
		Namespace:      "finance",
		Workflow:       "monthly-invoicing@a3f9c1e",
		Step:           "invoice",
		Attempt:        1,
		Image:          "ghcr.io/acme/agk-invoice@" + imageDigest,
		Params:         map[string]any{},
		Secrets:        []bus.SecretMount{},
		Inputs:         []bus.Input{},
		Outputs:        []string{"out"},
		Resources:      bus.Resources{CPU: "0.5", Memory: "256Mi", PIDs: 128},
		Network:        "none",
		RunsOn:         []string{},
		Deadline:       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Grant:          "agkgrant_01JMZ8V1PC7K3M0QY4B8ZR6TDN_Zm9vYmFyYmF6cXV4MTIzNA",
	}
	r := Redemption{
		TaskID:    m.TaskID,
		ExpiresAt: m.Deadline,
		Inputs:    []RedeemedInput{},
		Secrets:   []RedeemedSecret{},
		Tree:      []TreeEntry{},
		Uploads:   s.uploads(t),
	}
	for port, e := range inputs {
		d, _, err := artifact.PutEnvelope(t.Context(), s.objects, "finance", e)
		if err != nil {
			t.Fatal(err)
		}
		m.Inputs = append(m.Inputs, bus.Input{Port: string(port), Digest: "sha256:" + d, Items: len(e.Items)})
		in := RedeemedInput{Port: string(port), Envelope: RedeemedEnvelope{URL: s.url(t, d), Digest: "sha256:" + d}, Artifacts: []RedeemedArtifact{}}
		for _, item := range e.Items {
			for _, f := range item.Files {
				in.Artifacts = append(in.Artifacts, RedeemedArtifact{URI: f.URI.String(), SHA256: f.SHA256, URL: s.url(t, f.SHA256)})
			}
		}
		r.Inputs = append(r.Inputs, in)
	}
	for path, f := range tree {
		d := s.put(t, []byte(f.content))
		r.Tree = append(r.Tree, TreeEntry{Path: path, Mode: f.mode, SHA256: d, URL: s.url(t, d)})
	}
	for _, secret := range secrets {
		m.Secrets = append(m.Secrets, bus.SecretMount{Name: secret.Name, Mount: secret.Mount})
		r.Secrets = append(r.Secrets, secret)
	}
	return m, r
}

// imageDigest is the digest the fake daemon resolves the brick's image to.
const imageDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
