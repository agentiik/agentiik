package api_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"reflect"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A pool and a join token are held to the wire whole, as a redemption is: what an administrator
// sends and all of what the API answers, against $defs/runnerPool.

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredPoolCorpusIsWhatItSaysItIs(t *testing.T) {
	s := wire(t, "/$defs/runnerPool")
	cases, err := fixtures.RunnerPools()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the vendored pool corpus holds nothing")
	}
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Validate(v)
		switch {
		case c.Valid && err != nil:
			t.Errorf("%s should be accepted: %s", c.File, err)
		case !c.Valid && err == nil:
			t.Errorf("%s should be refused: %s", c.File, c.Rule)
		}
	}
}

// pooled is one document of the corpus, read as the wire writes it.
type pooled struct {
	Pool      map[string]any `json:"pool"`
	JoinToken map[string]any `json:"join_token"`
}

func corpusPool(t *testing.T, file string) pooled {
	t.Helper()
	body, err := fs.ReadFile(fixtures.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	var p pooled
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A pool created from the corpus is answered as the corpus writes it, and the token issued from
// it is answered beside it, each of them the document the wire describes.
func TestAPoolAndItsTokenAreWhatTheWireDescribes(t *testing.T) {
	cases, err := fixtures.RunnerPools()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := withRunners(t)
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		fixture := corpusPool(t, c.File)
		// The corpus's pool is dmz, which withRunners already made, so it is created under a
		// name of its own: everything else about it is as the corpus writes it.
		fixture.Pool["name"] = "from-the-corpus"
		fixture.JoinToken["pool"] = "from-the-corpus"

		// What an administrator sends is the document with no token half, which the wire
		// accepts on its own: "a shape demanding both would refuse a pool the documentation
		// prints on its own".
		ask := map[string]any{"pool": fixture.Pool}
		if err := conforms(t, "/$defs/runnerPool", ask); err != nil {
			t.Fatalf("the request built from %s is not what the wire describes: %s", c.File, err)
		}
		w, made := call(t, h, "POST", "/api/v1/runner-pools", "admin", ask)
		if w.Code != http.StatusCreated {
			t.Fatalf("creating the pool of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		if err := conforms(t, "/$defs/runnerPool", made); err != nil {
			t.Errorf("the pool is not answered as the wire describes it: %s", err)
		}
		if !reflect.DeepEqual(made, ask) {
			t.Errorf("the pool of %s was answered as %v", c.File, made)
		}

		labels := []string{}
		for _, l := range fixture.JoinToken["labels"].([]any) {
			labels = append(labels, l.(string))
		}
		w, issued := call(t, h, "POST", "/api/v1/runner-pools/from-the-corpus/join-tokens", "admin", api.Issue{Labels: labels})
		if w.Code != http.StatusCreated {
			t.Fatalf("issuing a token of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		if err := conforms(t, "/$defs/runnerPool", issued); err != nil {
			t.Errorf("the token is not answered as the wire describes it: %s", err)
		}
		// Every field the corpus writes and no other, the same where the value is the
		// pool's or the token's to say, and minted fresh where it is the installation's.
		if !reflect.DeepEqual(issued["pool"], fixture.Pool) {
			t.Errorf("the token of %s was answered beside the pool %v", c.File, issued["pool"])
		}
		token, _ := issued["join_token"].(map[string]any)
		if len(token) != len(fixture.JoinToken) {
			t.Errorf("the token is answered as %v, and the corpus writes %v", token, fixture.JoinToken)
		}
		for _, same := range []string{"pool", "labels", "single_use"} {
			if !reflect.DeepEqual(token[same], fixture.JoinToken[same]) {
				t.Errorf("the token's %s is %v, and the corpus writes %v", same, token[same], fixture.JoinToken[same])
			}
		}
		for _, minted := range []string{"id", "token", "issued_at", "expires_at"} {
			if token[minted] == nil || reflect.DeepEqual(token[minted], fixture.JoinToken[minted]) {
				t.Errorf("the token's %s is %v, where the installation mints one", minted, token[minted])
			}
		}
	}

	// The listing is the documents with no token half: "which is what a listing of pools is".
	w, listing := call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the pools answered %d: %s", w.Code, w.Body)
	}
	// The pool default is listed too, which the installation creates, and in a shape the wire
	// accepts like any other.
	pools, _ := listing["runner_pools"].([]any)
	if len(pools) != 3 {
		t.Fatalf("the listing holds %v", listing)
	}
	for _, p := range pools {
		if err := conforms(t, "/$defs/runnerPool", p); err != nil {
			t.Errorf("%v is listed in a shape the wire refuses: %s", p, err)
		}
		if one, _ := p.(map[string]any); one["join_token"] != nil {
			t.Errorf("a listing carries a token: %v", one)
		}
	}
}

// What the corpus refuses, the API refuses. The corpus's one refusal is a token permitting a
// label with no value, which the API refuses on the pool the label would have to be written on
// first, and on the token.
func TestWhatThePoolCorpusRefusesTheAPIRefuses(t *testing.T) {
	cases, err := fixtures.RunnerPools()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := withRunners(t)
	for _, c := range cases {
		if c.Valid {
			continue
		}
		fixture := corpusPool(t, c.File)
		labels := []string{}
		for _, l := range fixture.JoinToken["labels"].([]any) {
			labels = append(labels, l.(string))
		}
		w, _ := call(t, h, "POST", "/api/v1/runner-pools", "admin", api.RunnerPool{Pool: api.Pool{
			Name: "refused", Labels: labels, Namespaces: []string{}, Ceilings: &api.Ceilings{},
		}})
		if w.Code != http.StatusBadRequest {
			t.Errorf("a pool carrying the labels of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin", api.Issue{Labels: labels})
		if w.Code != http.StatusBadRequest {
			t.Errorf("a token permitting the labels of %s answered %d: %s", c.File, w.Code, w.Body)
		}
	}
}
