package api_test

import (
	"net/http"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// The administrator's half: a pool is created, then a token is issued into it, and only then is
// there a machine.

func TestAPoolIsCreatedAndThenRead(t *testing.T) {
	h, pool := withRunners(t)

	w, made := call(t, h, "POST", "/api/v1/runner-pools", "admin", api.Pool{
		Name: "sandboxed", Labels: []string{"runtime=runsc"},
		AcceptedNamespaces: []string{"finance"},
		MaxCPU:             4, MaxMemoryBytes: 1 << 33, MaxDiskBytes: 1 << 36,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a pool answered %d: %s", w.Code, w.Body)
	}
	if made["created_by"] != "admin" {
		t.Errorf("the pool says it was created by %v", made["created_by"])
	}

	w, listing := call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the pools answered %d: %s", w.Code, w.Body)
	}
	pools, _ := listing["runner_pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("the listing holds %d pools: %v", len(pools), listing)
	}
	found := map[string]map[string]any{}
	for _, p := range pools {
		one, _ := p.(map[string]any)
		name, _ := one["pool"].(string)
		found[name] = one
	}
	switch one := found["sandboxed"]; {
	case one == nil:
		t.Fatalf("the listing holds %v", found)
	case one["max_cpu"] != float64(4):
		t.Errorf("the ceiling came back as %v", one["max_cpu"])
	case one["runners"] != float64(0):
		t.Errorf("a pool nothing has joined holds %v runners", one["runners"])
	}

	// A pool with no ceiling of a kind says nothing rather than saying zero, because a
	// ceiling of zero would be a pool that runs nothing.
	if _, said := found["dmz"]["max_cpu"]; said {
		t.Errorf("a pool with no ceiling answered one: %v", found["dmz"])
	}

	// And what a pool holds is what it holds, counted rather than remembered.
	token := issue(t, pool, []string{"zone=dmz"})
	if w, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz")); w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d", w.Code)
	}
	_, listing = call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	pools, _ = listing["runner_pools"].([]any)
	for _, p := range pools {
		one, _ := p.(map[string]any)
		if one["pool"] == "dmz" && (one["runners"] != float64(1) || one["ready"] != float64(1)) {
			t.Errorf("the pool a machine joined reads %v", one)
		}
	}
}

// "It is shown once, here, and stored hashed, so this answer is the only moment the value exists
// outside the machine that will hold it."
func TestAJoinTokenIsIssuedIntoAPoolAndShownOnce(t *testing.T) {
	h, _ := withRunners(t)

	w, issued := call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{Labels: []string{"zone=dmz"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("issuing answered %d: %s", w.Code, w.Body)
	}
	clear, _ := issued["token"].(string)
	if clear == "" || issued["pool"] != "dmz" || issued["expires_at"] == nil {
		t.Fatalf("issuing answered %v", issued)
	}

	// It is a real token, which is the only way to tell that the answer is the value and
	// not a description of it.
	if w, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine(clear, "zone=dmz")); w.Code != http.StatusCreated {
		t.Fatalf("the token answered %d when a machine presented it", w.Code)
	}

	// A label the pool does not carry cannot be put on a token, which is where "Labels are
	// not self-asserted" starts.
	w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{Labels: []string{"zone=lan"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a token permitting a label its pool does not carry answered %d", w.Code)
	}

	// A pool nobody created is a 404 rather than a refusal, because the caller is already
	// an administrator and there is nothing about a pool to hide from them.
	w, _ = call(t, h, "POST", "/api/v1/runner-pools/imaginary/join-tokens", "admin", api.Issue{})
	if w.Code != http.StatusNotFound {
		t.Errorf("a token for a pool nobody created answered %d", w.Code)
	}

	// And it cannot be asked to live a long time: "the token only has to survive the minutes
	// between an administrator copying it and a machine presenting it".
	w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{ExpiresInSeconds: 30 * 24 * 60 * 60})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a token asked to live a month answered %d", w.Code)
	}
}

// The pools are the installation's, like the inventory, and a runner reaches neither.
func TestThePoolsAreNotSomethingARunnerReads(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz"})
	_, joined := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz"))
	credential, _ := joined["credential"].(string)

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/runner-pools", nil},
		{"POST", "/api/v1/runner-pools", api.Pool{Name: "mine"}},
		{"POST", "/api/v1/runner-pools/dmz/join-tokens", api.Issue{}},
	} {
		w, _ := call(t, h, c.method, c.path, credential, c.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("a runner reaching %s %s answered %d", c.method, c.path, w.Code)
		}
	}
}
