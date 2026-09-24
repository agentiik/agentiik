package api_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
)

// The administrator's half: a pool is created, then a token is issued into it, and only then is
// there a machine.

// sandboxed is a pool as an administrator writes one, every part of it given.
func sandboxed() api.RunnerPool {
	return api.RunnerPool{Pool: api.Pool{
		Name: "sandboxed", Labels: []string{"runtime=runsc"}, Namespaces: []string{"finance"},
		Ceilings: &api.Ceilings{CPU: "4", Memory: "8Gi", PIDs: 512},
	}}
}

func TestAPoolIsCreatedAndThenRead(t *testing.T) {
	h, _ := withRunners(t)

	w, made := call(t, h, "POST", "/api/v1/runner-pools", "admin", sandboxed())
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a pool answered %d: %s", w.Code, w.Body)
	}
	// Answered as it was written, and with the tier it was given since it named none.
	want := map[string]any{
		"name": "sandboxed", "labels": []any{"runtime=runsc"}, "namespaces": []any{"finance"},
		"resource_ceilings": map[string]any{"cpu": "4", "memory": "8Gi", "pids": float64(512)},
		"containment":       "hardened",
	}
	if !reflect.DeepEqual(made, map[string]any{"pool": want}) {
		t.Errorf("creating a pool answered %v", made)
	}

	w, listing := call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the pools answered %d: %s", w.Code, w.Body)
	}
	pools, _ := listing["runner_pools"].([]any)
	if len(pools) != 2 {
		t.Fatalf("the listing holds %d pools: %v", len(pools), listing)
	}
	var names []string
	found := map[string]map[string]any{}
	for _, p := range pools {
		one, _ := p.(map[string]any)
		pool, _ := one["pool"].(map[string]any)
		name, _ := pool["name"].(string)
		names = append(names, name)
		found[name] = pool
	}
	if strings.Join(names, ", ") != "dmz, sandboxed" {
		t.Errorf("the listing is %v, and it is ordered by name", names)
	}
	if !reflect.DeepEqual(found["sandboxed"], want) {
		t.Errorf("the listing reads the pool as %v", found["sandboxed"])
	}

	// A pool created with no ceiling and no namespaces says so with an empty object and an
	// empty list, rather than leaving out what the wire requires.
	dmz := found["dmz"]
	if !reflect.DeepEqual(dmz["resource_ceilings"], map[string]any{}) || !reflect.DeepEqual(dmz["namespaces"], []any{}) {
		t.Errorf("a pool with no ceiling and every namespace reads %v", dmz)
	}
}

// A pool is refused, saying why, wherever the wire would refuse it, and wherever it says
// something this installation cannot give it.
func TestAPoolIsRefusedWhereTheWireRefusesIt(t *testing.T) {
	h, _ := withRunners(t)

	for _, c := range []struct {
		name  string
		body  string
		want  int
		means string
	}{
		{"a name in capitals", `{"pool":{"name":"DMZ","labels":[],"namespaces":[],"resource_ceilings":{}}}`, 400, "not a pool name"},
		{"a name with an underscore", `{"pool":{"name":"gpu_nvme","labels":[],"namespaces":[],"resource_ceilings":{}}}`, 400, "not a pool name"},
		{"a name longer than the bus names a consumer", `{"pool":{"name":"` + strings.Repeat("a", 256) + `","labels":[],"namespaces":[],"resource_ceilings":{}}}`, 400, "at most 255"},
		{"no pool at all", `{}`, 400, "names no pool"},
		{"the pool left bare", `{"pool":{"name":"lan"}}`, 400, "no labels"},
		{"no namespaces", `{"pool":{"name":"lan","labels":[],"resource_ceilings":{}}}`, 400, "accepts every namespace"},
		{"no ceilings", `{"pool":{"name":"lan","labels":[],"namespaces":[]}}`, 400, "resource_ceilings"},
		{"ceilings of null", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":null}}`, 400, "resource_ceilings"},
		{"a label with no value", `{"pool":{"name":"lan","labels":["zone"],"namespaces":[],"resource_ceilings":{}}}`, 400, "key=value"},
		{"a label written twice", `{"pool":{"name":"lan","labels":["zone=lan","zone=lan"],"namespaces":[],"resource_ceilings":{}}}`, 400, "twice"},
		{"a namespace in capitals", `{"pool":{"name":"lan","labels":[],"namespaces":["Finance"],"resource_ceilings":{}}}`, 400, "not a namespace"},
		{"a namespace written twice", `{"pool":{"name":"lan","labels":[],"namespaces":["finance","finance"],"resource_ceilings":{}}}`, 400, "twice"},
		{"cpu as a number", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"cpu":4}}}`, 400, "a string"},
		{"cpu of no cores", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"cpu":"0"}}}`, 400, "above zero"},
		{"memory in decimal units", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"memory":"8GB"}}}`, 400, "binary suffix"},
		{"pids of none", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"pids":0}}}`, 400, "one or more"},
		{"an empty cpu", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"cpu":""}}}`, 400, "cpu ceiling"},
		{"an empty memory", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"memory":""}}}`, 400, "memory ceiling"},
		{"cpu of null", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"cpu":null}}}`, 400, "leaves it out"},
		{"memory of null", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"memory":null}}}`, 400, "leaves it out"},
		{"pids of null", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"pids":null}}}`, 400, "leaves it out"},
		{"a disk ceiling", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{"disk":"100Gi"}}}`, 400, "nothing can enforce one"},
		{"the ceilings this route took before", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{}},"max_cpu":4}`, 400, "not a field"},
		{"a sandboxed pool", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{},"containment":"sandboxed"}}`, 400, "v1.0.0"},
		{"a separated pool", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{},"containment":"separated"}}`, 400, "v1.0.0"},
		{"a tier nobody named", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{},"containment":"gvisor"}}`, 400, "not a tier"},
		{"an empty tier", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{},"containment":""}}`, 400, "not a tier"},
		{"a tier of null", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{},"containment":null}}`, 400, "leaves it out"},
		{"a token written with the pool", `{"pool":{"name":"lan","labels":[],"namespaces":[],"resource_ceilings":{}},"join_token":{}}`, 400, "join-tokens"},
		{"a pool whose name is taken", `{"pool":{"name":"dmz","labels":[],"namespaces":[],"resource_ceilings":{}}}`, 409, "exists already"},
	} {
		w := sent(t, h, "POST", "/api/v1/runner-pools", "admin", c.body)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.means) {
			t.Errorf("%s answered %d: %s", c.name, w.Code, w.Body)
		}
	}

	// Nothing refused was written.
	_, listing := call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	if pools, _ := listing["runner_pools"].([]any); len(pools) != 1 {
		t.Errorf("after the refusals the listing holds %v", listing)
	}
}

// A pids ceiling is kept at any size the wire accepts, which sets a least and no most, as the
// PidsLimit it becomes is 64 bits: one past 32 bits is a pool like any other, and not a failure
// of the API's own.
func TestAPidsCeilingIsKeptAtAnySizeTheWireAccepts(t *testing.T) {
	h, _ := withRunners(t)

	w := sent(t, h, "POST", "/api/v1/runner-pools", "admin",
		`{"pool":{"name":"many","labels":[],"namespaces":[],"resource_ceilings":{"pids":3000000000}}}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"resource_ceilings":{"pids":3000000000}`) {
		t.Errorf("a pool whose pids ceiling is past 32 bits answered %d: %s", w.Code, w.Body)
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
	token, _ := issued["join_token"].(map[string]any)
	clear, _ := token["token"].(string)
	if clear == "" || token["pool"] != "dmz" || token["single_use"] != true {
		t.Fatalf("issuing answered %v", issued)
	}
	// Beside the pool it admits to, "because neither is legible alone".
	if pool, _ := issued["pool"].(map[string]any); pool["name"] != "dmz" {
		t.Errorf("the token was answered beside %v", issued["pool"])
	}

	// "One hour after it was issued is the default."
	if life := lifeOf(t, token); life != time.Hour {
		t.Errorf("a token nobody asked to live longer lives %s", life)
	}

	// It is a real token, which is the only way to tell that the answer is the value and
	// not a description of it.
	if w, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine(clear, "zone=dmz")); w.Code != http.StatusCreated {
		t.Fatalf("the token answered %d when a machine presented it", w.Code)
	}

	// An administrator who needs longer says so when issuing it.
	w, issued = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{ExpiresInSeconds: 600})
	if token, _ := issued["join_token"].(map[string]any); w.Code != http.StatusCreated || lifeOf(t, token) != 10*time.Minute {
		t.Errorf("a token asked to live ten minutes answered %d: %v", w.Code, issued)
	}

	// A label the pool does not carry cannot be put on a token, which is where "Labels are
	// not self-asserted" starts.
	w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{Labels: []string{"zone=lan"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a token permitting a label its pool does not carry answered %d", w.Code)
	}

	// Nor one label twice, since the answer would list it twice.
	w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
		api.Issue{Labels: []string{"zone=dmz", "zone=dmz"}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a token permitting one label twice answered %d", w.Code)
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

	// Nor so long that the seconds wrap as a duration, which is the same refusal and not a
	// token dead as it is issued, one that lives a third of a second, or a failure of the API's
	// own.
	for _, seconds := range []int{9223372037, 18446744074, 1 << 55} {
		w, _ = call(t, h, "POST", "/api/v1/runner-pools/dmz/join-tokens", "admin",
			api.Issue{ExpiresInSeconds: seconds})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "longer than a day") {
			t.Errorf("a token asked to live %d seconds answered %d: %s", seconds, w.Code, w.Body)
		}
	}
}

// lifeOf is how long a token was answered as living: its expiry less the moment it was issued.
func lifeOf(t *testing.T, token map[string]any) time.Duration {
	t.Helper()
	issuedAt, _ := token["issued_at"].(string)
	expiresAt, _ := token["expires_at"].(string)
	from, err := time.Parse(time.RFC3339Nano, issuedAt)
	if err != nil {
		t.Fatalf("the token was issued at %q: %s", issuedAt, err)
	}
	until, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("the token expires at %q: %s", expiresAt, err)
	}
	return until.Sub(from)
}

// The pools are the installation's, like the inventory, and a runner reaches neither.
func TestThePoolsAreNotSomethingARunnerReads(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz"})
	_, joined := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz"))
	credential, _ := joined["credential"].(string)

	for _, c := range administration() {
		w, _ := call(t, h, c.method, c.path, credential, c.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("a runner reaching %s %s answered %d", c.method, c.path, w.Code)
		}
	}
}

// ownsEveryNamespace holds every permission over every namespace and every workflow in them, and
// nothing over the installation: as much as anybody can be given and still not be an
// administrator.
type ownsEveryNamespace struct{ who api.Principal }

func (o ownsEveryNamespace) Allow(_ context.Context, who api.Principal, _ api.Permission, over api.Target) (bool, error) {
	return who == o.who && over.Namespace != "", nil
}

// "Administrator only": grant:manage at the installation, and nothing held over a namespace
// stands in for it, however much of it there is.
func TestOnlyAnAdministratorReachesThePoolsAndTheInventory(t *testing.T) {
	h, _ := withRunnersAuthorizedBy(t, ownsEveryNamespace{who: "alice"})

	for _, c := range administration() {
		w, _ := call(t, h, c.method, c.path, "alice", c.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("an owner of every namespace reaching %s %s answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
		w, _ = call(t, h, c.method, c.path, "", c.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("nobody reaching %s %s answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

// administration is every route an administrator reaches and nobody else does, each with a body
// that would succeed.
func administration() []struct {
	method, path string
	body         any
} {
	return []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/runners", nil},
		{"GET", "/api/v1/runner-pools", nil},
		{"POST", "/api/v1/runner-pools", sandboxed()},
		{"POST", "/api/v1/runner-pools/dmz/join-tokens", api.Issue{}},
	}
}
