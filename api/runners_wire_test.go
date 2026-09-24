package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A join is held to the wire whole: what a machine sends and all of what it is answered, against
// $defs/runnerRegistration.

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredJoinCorpusIsWhatItSaysItIs(t *testing.T) {
	s := wire(t, "/$defs/runnerRegistration")
	cases, err := fixtures.RunnerRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the vendored join corpus holds nothing")
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

// joinRequest is the request half of one document of the corpus, as the wire writes it.
func joinRequest(t *testing.T, file string) map[string]any {
	t.Helper()
	body, err := fs.ReadFile(fixtures.FS, file)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Request
}

// corpusJoin creates the pool a corpus join lands in: one carrying every label the corpus claims
// and accepting the namespaces it narrows itself to, so that nothing but the document is tested.
// The token it answers permits exactly the labels the request claims.
func corpusJoin(t *testing.T, pool *db.Pool, request map[string]any) string {
	t.Helper()
	var labels, namespaces []string
	for _, l := range request["labels"].([]any) {
		labels = append(labels, l.(string))
	}
	if listed, ok := request["namespaces"].([]any); ok {
		for _, n := range listed {
			namespaces = append(namespaces, n.(string))
		}
	}
	return issueFor(t, pool, db.RunnerPool{
		Name: "from-the-corpus", Labels: labels, AcceptedNamespaces: namespaces, CreatedBy: "admin",
	}, labels)
}

// issueFor creates a pool and mints a token of it, as an administrator would.
func issueFor(t *testing.T, pool *db.Pool, made db.RunnerPool, labels []string) string {
	t.Helper()
	var token db.JoinToken
	err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		if _, err := w.RunnerPoolNamed(ctx, made.Name); err != nil {
			if err := w.CreateRunnerPool(ctx, made); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		var err error
		token, err = w.IssueJoinToken(ctx, made.Name, labels, "admin", now, now.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token.Clear
}

// A machine written from the wire joins, and is answered the four fields the wire names and no
// other, each in the shape the wire gives it. What it said of itself is what the inventory holds.
func TestAJoinAndItsAnswerAreWhatTheWireDescribes(t *testing.T) {
	cases, err := fixtures.RunnerRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	h, pool := withRunners(t)
	joined := 0
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		request := joinRequest(t, c.File)
		request["token"] = corpusJoin(t, pool, request)
		if err := conforms(t, "/$defs/runnerRegistration/properties/request", request); err != nil {
			t.Fatalf("the request built from %s is not what the wire describes: %s", c.File, err)
		}

		w, answer := call(t, h, "POST", "/api/v1/runners", "", request)
		if w.Code != http.StatusCreated {
			t.Fatalf("the join of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		joined++
		if err := conforms(t, "/$defs/runnerRegistration/properties/response", answer); err != nil {
			t.Errorf("the join of %s is not answered as the wire describes: %s: %v", c.File, err, answer)
		}
		if err := conforms(t, "/$defs/runnerRegistration", map[string]any{"request": request, "response": answer}); err != nil {
			t.Errorf("the join of %s and its answer are not the exchange the wire describes: %s", c.File, err)
		}
		if keys := slices.Sorted(maps.Keys(answer)); !slices.Equal(keys, []string{"credential", "pool", "rotate_by", "runner"}) {
			t.Errorf("the join of %s is answered with %v", c.File, keys)
		}
		if answer["pool"] != "from-the-corpus" {
			t.Errorf("the join of %s landed in %v, and its token names from-the-corpus", c.File, answer["pool"])
		}
		if rotate, _ := answer["rotate_by"].(string); !strings.HasSuffix(rotate, "Z") {
			t.Errorf("the credential rotates at %q, which is not written in UTC as every instant on the wire is", rotate)
		}

		// The inventory holds what the machine said, the host's narrowing and containment
		// included, and never the key, which proves the host rather than describes it.
		w, listing := call(t, h, "GET", "/api/v1/runners", "admin", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("the inventory answered %d: %s", w.Code, w.Body)
		}
		var held map[string]any
		for _, r := range listing["runners"].([]any) {
			if one := r.(map[string]any); one["runner"] == answer["runner"] {
				held = one
			}
		}
		if held == nil {
			t.Fatalf("the inventory does not hold %v: %v", answer["runner"], listing)
		}
		for field, want := range map[string]any{
			"labels":        request["labels"],
			"namespaces":    request["namespaces"],
			"containment":   request["containment"],
			"architecture":  request["architecture"],
			"agent_version": request["agent_version"],
			"cpu":           request["capacity"].(map[string]any)["vcpu"],
			"memory_bytes":  float64(64 << 30),
			"disk_bytes":    float64(1 << 40),
		} {
			if !reflect.DeepEqual(held[field], want) {
				t.Errorf("the inventory holds %s as %v, and the machine said %v", field, held[field], want)
			}
		}
		if _, there := held["public_key"]; there {
			t.Errorf("the inventory answers the host's key: %v", held)
		}
	}
	if joined == 0 {
		t.Fatal("the corpus holds no join to make")
	}
}

// What the corpus refuses, the API refuses, before the token is spent: the corpus's one refusal is a
// join carrying the host's private key, which is refused whatever else it says.
func TestWhatTheJoinCorpusRefusesTheAPIRefuses(t *testing.T) {
	cases, err := fixtures.RunnerRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	h, pool := withRunners(t)
	refused := 0
	for _, c := range cases {
		if c.Valid {
			continue
		}
		request := joinRequest(t, c.File)
		token := corpusJoin(t, pool, request)
		request["token"] = token
		w, answer := call(t, h, "POST", "/api/v1/runners", "", request)
		if w.Code != http.StatusBadRequest {
			t.Errorf("the join of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, "keypair") {
			t.Errorf("the join of %s was refused with %q, which does not say what was wrong with it", c.File, said)
		}
		refused++

		delete(request, "private_key")
		if w, _ := call(t, h, "POST", "/api/v1/runners", "", request); w.Code != http.StatusCreated {
			t.Errorf("the token of the refused %s answered %d: %s", c.File, w.Code, w.Body)
		}
	}
	if refused == 0 {
		t.Fatal("the corpus holds no join to refuse")
	}
}

// A machine's description of itself is held to the wire's grammar, field for field, and a join the
// wire refuses is refused with 400 and a sentence saying why, before its token is spent. A few are
// refused that the wire's patterns let through, because what the key is and how large a host is are
// not things a pattern can read.
func TestAJoinTheWireRefusesIsRefused(t *testing.T) {
	h, pool := withRunners(t)

	valid := func() map[string]any {
		return map[string]any{
			"public_key":    aKey,
			"labels":        []any{"zone=dmz"},
			"capacity":      map[string]any{"vcpu": 8, "memory": "64Gi", "disk": "1Ti"},
			"architecture":  "amd64",
			"agent_version": "0.2.0",
			"namespaces":    []any{"finance"},
			"containment":   map[string]any{"runtime": "runc", "userns_remap": true},
		}
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&ecdsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	notEd25519 := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))

	for _, c := range []struct {
		name string
		with func(map[string]any)

		// patternOnly is true of what the wire's patterns accept and the API still refuses.
		patternOnly bool
	}{
		{"no public key", func(j map[string]any) { delete(j, "public_key") }, false},
		{"a public key that is not PEM", func(j map[string]any) {
			j["public_key"] = "MCowBQYDK2VwAyEAXiy2zvWwTpj67NwwKIgCbjFcQdrNAboeffNXm+aJUcM="
		}, false},
		{"a private key where the public one goes", func(j map[string]any) {
			j["public_key"] = strings.ReplaceAll(aKey, "PUBLIC", "PRIVATE")
		}, false},
		{"two keys in one field", func(j map[string]any) { j["public_key"] = aKey + aKey }, false},
		{"a public key that is not Ed25519", func(j map[string]any) { j["public_key"] = notEd25519 }, true},
		{"a public key whose block holds no key", func(j map[string]any) {
			j["public_key"] = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
		}, true},
		{"no labels", func(j map[string]any) { delete(j, "labels") }, false},
		{"a label written twice", func(j map[string]any) { j["labels"] = []any{"zone=dmz", "zone=dmz"} }, false},
		{"a label with no value", func(j map[string]any) { j["labels"] = []any{"zone"} }, false},
		{"no capacity", func(j map[string]any) { delete(j, "capacity") }, false},
		{"capacity as null", func(j map[string]any) { j["capacity"] = nil }, false},
		{"no vCPU", func(j map[string]any) { j["capacity"].(map[string]any)["vcpu"] = 0 }, false},
		{"memory in bytes", func(j map[string]any) { j["capacity"].(map[string]any)["memory"] = "68719476736" }, false},
		{"memory in decimal units", func(j map[string]any) { j["capacity"].(map[string]any)["memory"] = "64GB" }, false},
		{"no disk", func(j map[string]any) { delete(j["capacity"].(map[string]any), "disk") }, false},
		{"a disk past what bytes count", func(j map[string]any) { j["capacity"].(map[string]any)["disk"] = "9999999Ti" }, true},
		{"a capacity field the wire does not describe", func(j map[string]any) { j["capacity"].(map[string]any)["gpus"] = 1 }, false},
		{"no architecture", func(j map[string]any) { delete(j, "architecture") }, false},
		{"an architecture in capitals", func(j map[string]any) { j["architecture"] = "AMD64" }, false},
		{"no agent version", func(j map[string]any) { delete(j, "agent_version") }, false},
		{"an agent version with a v", func(j map[string]any) { j["agent_version"] = "v0.2.0" }, false},
		{"a host narrowed to no namespace", func(j map[string]any) { j["namespaces"] = []any{} }, false},
		{"namespaces as null", func(j map[string]any) { j["namespaces"] = nil }, false},
		{"a namespace written twice", func(j map[string]any) { j["namespaces"] = []any{"finance", "finance"} }, false},
		{"a namespace in capitals", func(j map[string]any) { j["namespaces"] = []any{"Finance"} }, false},
		{"containment naming no runtime", func(j map[string]any) { delete(j["containment"].(map[string]any), "runtime") }, false},
		{"containment silent on remapping", func(j map[string]any) { delete(j["containment"].(map[string]any), "userns_remap") }, false},
		{"a runtime in capitals", func(j map[string]any) { j["containment"].(map[string]any)["runtime"] = "RunC" }, false},
		{"the pool it wants", func(j map[string]any) { j["pool"] = "dmz" }, false},
		{"its address", func(j map[string]any) { j["address"] = "10.0.0.7" }, false},
		{"the tier it claims", func(j map[string]any) { j["containment"].(map[string]any)["tier"] = "sandboxed" }, false},
	} {
		token := issue(t, pool, []string{"zone=dmz"})
		request := valid()
		request["token"] = token.Clear
		c.with(request)

		byWire := conforms(t, "/$defs/runnerRegistration/properties/request", request)
		switch {
		case c.patternOnly && byWire != nil:
			t.Errorf("a join with %s is refused by the wire too, so it belongs with the rest: %s", c.name, byWire)
		case !c.patternOnly && byWire == nil:
			t.Errorf("a join with %s is accepted by the wire, so this case tests nothing of it", c.name)
		}

		w, answer := call(t, h, "POST", "/api/v1/runners", "", request)
		if w.Code != http.StatusBadRequest {
			t.Errorf("a join with %s answered %d: %s", c.name, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); said == "" {
			t.Errorf("a join with %s was refused with %q", c.name, said)
		}

		// Refused before the token was looked at, so the same machine describing itself as
		// the wire does joins with it.
		fixed := valid()
		fixed["token"] = token.Clear
		if w, _ := call(t, h, "POST", "/api/v1/runners", "", fixed); w.Code != http.StatusCreated {
			t.Errorf("the token of a join refused for %s answered %d: %s", c.name, w.Code, w.Body)
		}
	}
}

// "It narrows and never widens." A host narrowing itself to a namespace its pool does not accept is
// refused with the answer every bad token gets, as a label its token does not permit is, since
// both are a machine asking for more than its token gives.
func TestAHostWideningItsPoolIsRefusedAsABadTokenIs(t *testing.T) {
	h, pool := withRunners(t)
	narrow := db.RunnerPool{Name: "narrow", AcceptedNamespaces: []string{"finance"}, CreatedBy: "admin"}

	wrong, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine("agkjoin_notarealtokenatallbutlongenoughtolookplausible"))
	widening := aMachine(issueFor(t, pool, narrow, nil))
	widening.Namespaces = []string{"finance", "payroll"}
	w, _ := call(t, h, "POST", "/api/v1/runners", "", widening)
	if w.Code != http.StatusUnauthorized || w.Body.String() != wrong.Body.String() {
		t.Errorf("a host widening its pool answered %d: %s, and a wrong token %d: %s", w.Code, w.Body, wrong.Code, wrong.Body)
	}

	// The same token, narrowing inside what its pool accepts, joins.
	widening.Namespaces = []string{"finance"}
	if w, _ := call(t, h, "POST", "/api/v1/runners", "", widening); w.Code != http.StatusCreated {
		t.Errorf("a host narrowing inside its pool answered %d: %s", w.Code, w.Body)
	}
}

// The runner is named in the grammar the wire answers it in, which is also what the reader of its
// results holds it to: a machine answered with a name its own results would be refused under is
// one that joins and is never heard from.
func TestARunnerIsAnsweredWithALowercaseName(t *testing.T) {
	h, pool := withRunners(t)
	w, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(issue(t, pool, nil).Clear))
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	runner, _ := answer["runner"].(string)
	if runner == "" || runner != strings.ToLower(runner) {
		t.Errorf("the runner was answered as %q", runner)
	}
	if err := conforms(t, "/$defs/runnerRegistration/properties/response/properties/runner", runner); err != nil {
		t.Errorf("the runner was answered as %q, which the wire refuses: %s", runner, err)
	}
}
