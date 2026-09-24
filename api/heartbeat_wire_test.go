package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A heartbeat is held to the wire whole: what a runner sends every ten seconds and all of what it is
// answered, against $defs/runnerHeartbeat.

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredHeartbeatCorpusIsWhatItSaysItIs(t *testing.T) {
	s := wire(t, "/$defs/runnerHeartbeat")
	cases, err := fixtures.RunnerHeartbeats()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the vendored heartbeat corpus holds nothing")
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

// beatRequest is the request half of one document of the corpus, as the wire writes it.
func beatRequest(t *testing.T, file string) map[string]any {
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

// joined is a machine that has joined the dmz pool: its runner and its credential.
func joined(t *testing.T, h http.Handler, pool *db.Pool) (string, string) {
	t.Helper()
	w, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(issue(t, pool, nil).Clear))
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	runner, _ := answer["runner"].(string)
	credential, _ := answer["credential"].(string)
	return runner, credential
}

// inventoried is what the inventory holds of one runner.
func inventoried(t *testing.T, h http.Handler, runner string) map[string]any {
	t.Helper()
	w, listing := call(t, h, "GET", "/api/v1/runners", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the inventory answered %d: %s", w.Code, w.Body)
	}
	for _, r := range listing["runners"].([]any) {
		if one := r.(map[string]any); one["runner"] == runner {
			return one
		}
	}
	t.Fatalf("the inventory does not hold %s: %v", runner, listing)
	return nil
}

// A heartbeat written from the wire is taken, and answered the three fields the wire requires of
// every answer and no other while nobody has drained the runner, reason joining them once somebody
// has. What the runner said of itself is what the inventory then holds.
func TestAHeartbeatAndItsAnswerAreWhatTheWireDescribes(t *testing.T) {
	cases, err := fixtures.RunnerHeartbeats()
	if err != nil {
		t.Fatal(err)
	}
	h, pool := withRunners(t)
	beaten := 0
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		runner, credential := joined(t, h, pool)
		request := beatRequest(t, c.File)
		request["runner"] = runner
		if err := conforms(t, "/$defs/runnerHeartbeat/properties/request", request); err != nil {
			t.Fatalf("the heartbeat built from %s is not what the wire describes: %s", c.File, err)
		}

		before := time.Now().UTC()
		w, answer := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, request)
		if w.Code != http.StatusOK {
			t.Fatalf("the heartbeat of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		beaten++
		if err := conforms(t, "/$defs/runnerHeartbeat/properties/response", answer); err != nil {
			t.Errorf("the heartbeat of %s is not answered as the wire describes: %s: %v", c.File, err, answer)
		}
		if err := conforms(t, "/$defs/runnerHeartbeat", map[string]any{"request": request, "response": answer}); err != nil {
			t.Errorf("the heartbeat of %s and its answer are not the exchange the wire describes: %s", c.File, err)
		}
		if keys := slices.Sorted(maps.Keys(answer)); !slices.Equal(keys, []string{"cancel", "drain", "received_at"}) {
			t.Errorf("the heartbeat of %s is answered with %v", c.File, keys)
		}
		if answer["drain"] != false {
			t.Errorf("a ready runner nobody drained is told drain: %v", answer["drain"])
		}
		if cancel, ok := answer["cancel"].([]any); !ok || len(cancel) != 0 {
			t.Errorf("a runner holding keys no task has is told to stop %v", answer["cancel"])
		}
		received, _ := answer["received_at"].(string)
		at, err := time.Parse(time.RFC3339Nano, received)
		if err != nil || !strings.HasSuffix(received, "Z") {
			t.Errorf("the heartbeat was received at %q, which is not an instant written in UTC as every instant on the wire is", received)
		} else if at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
			t.Errorf("the heartbeat was received at %s, which is not the API's clock at the time", received)
		}

		held := inventoried(t, h, runner)
		for field, want := range map[string]any{
			"agent_version":  request["agent_version"],
			"reported_state": request["state"],
			"concurrency":    request["concurrency"],
		} {
			if held[field] != want {
				t.Errorf("the inventory holds %s as %v, and the heartbeat said %v", field, held[field], want)
			}
		}
		if held["state"] != "ready" {
			t.Errorf("the inventory holds a runner nobody drained as %v", held["state"])
		}
		if held["last_seen_at"] == nil {
			t.Errorf("the inventory does not say when the runner was last heard from: %v", held)
		}

		// Drained, it is told so, with the reason and nothing else.
		if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
			return w.Drain(ctx, runner, "pool zone=dmz is being retired")
		}); err != nil {
			t.Fatal(err)
		}
		w, answer = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, request)
		if w.Code != http.StatusOK {
			t.Fatalf("the heartbeat of a drained runner answered %d: %s", w.Code, w.Body)
		}
		if err := conforms(t, "/$defs/runnerHeartbeat/properties/response", answer); err != nil {
			t.Errorf("a drained runner is not answered as the wire describes: %s: %v", err, answer)
		}
		if keys := slices.Sorted(maps.Keys(answer)); !slices.Equal(keys, []string{"cancel", "drain", "reason", "received_at"}) {
			t.Errorf("a drained runner is answered with %v", keys)
		}
		if answer["drain"] != true || answer["reason"] != "pool zone=dmz is being retired" {
			t.Errorf("a drained runner is told %v", answer)
		}
	}
	if beaten == 0 {
		t.Fatal("the corpus holds no heartbeat to send")
	}
}

// What the corpus refuses, the API refuses, and records nothing of.
func TestWhatTheHeartbeatCorpusRefusesTheAPIRefuses(t *testing.T) {
	cases, err := fixtures.RunnerHeartbeats()
	if err != nil {
		t.Fatal(err)
	}
	h, pool := withRunners(t)
	refused := 0
	for _, c := range cases {
		if c.Valid {
			continue
		}
		runner, credential := joined(t, h, pool)
		request := beatRequest(t, c.File)
		request["runner"] = runner
		w, answer := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, request)
		if w.Code != http.StatusBadRequest {
			t.Errorf("the heartbeat of %s answered %d: %s", c.File, w.Code, w.Body)
		}
		if said, _ := answer["error"].(string); !strings.Contains(said, "idempotency key") {
			t.Errorf("the heartbeat of %s was refused with %q, which does not say what was wrong with it", c.File, said)
		}
		if seen := inventoried(t, h, runner)["last_seen_at"]; seen != nil {
			t.Errorf("a refused heartbeat of %s was recorded as the runner being heard from at %v", c.File, seen)
		}
		refused++
	}
	if refused == 0 {
		t.Fatal("the corpus holds no heartbeat to refuse")
	}
}

// A heartbeat is held to the wire's grammar, field for field, and one the wire refuses is refused
// with 400 and a sentence saying why, and recorded nowhere. A few are refused that the wire's
// patterns let through, because a pattern cannot read them.
func TestAHeartbeatTheWireRefusesIsRefused(t *testing.T) {
	h, pool := withRunners(t)
	runner, credential := joined(t, h, pool)

	valid := func() map[string]any {
		return map[string]any{
			"runner":        runner,
			"agent_version": "0.2.0",
			"state":         "ready",
			"concurrency":   8,
			"tasks":         []any{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3/8", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1"},
			"sent_at":       "2026-09-10T06:41:09.104Z",
		}
	}
	for _, c := range []struct {
		name string
		with func(map[string]any)

		// patternOnly is true of what the wire's patterns accept and the API still refuses. A
		// sent_at the wire's format refuses is among them, since the validator reads a format
		// as a note rather than a rule unless it is told otherwise.
		patternOnly bool
	}{
		{"no runner", func(b map[string]any) { delete(b, "runner") }, false},
		{"a runner in capitals", func(b map[string]any) { b["runner"] = strings.ToUpper(runner) }, false},
		{"no agent version", func(b map[string]any) { delete(b, "agent_version") }, false},
		{"an agent version with a v", func(b map[string]any) { b["agent_version"] = "v0.2.0" }, false},
		{"no state", func(b map[string]any) { delete(b, "state") }, false},
		{"a state the wire does not list", func(b map[string]any) { b["state"] = "revoked" }, false},
		{"a state in capitals", func(b map[string]any) { b["state"] = "Ready" }, false},
		{"no concurrency", func(b map[string]any) { delete(b, "concurrency") }, false},
		{"a concurrency of none", func(b map[string]any) { b["concurrency"] = 0 }, false},
		{"a concurrency that is not whole", func(b map[string]any) { b["concurrency"] = 1.5 }, false},
		{"no tasks", func(b map[string]any) { delete(b, "tasks") }, false},
		{"tasks as null", func(b map[string]any) { b["tasks"] = nil }, false},
		{"a key written twice", func(b map[string]any) {
			b["tasks"] = []any{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1"}
		}, false},
		{"a key without its attempt", func(b map[string]any) { b["tasks"] = []any{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize"} }, false},
		{"a key with a shard and no cardinality", func(b map[string]any) {
			b["tasks"] = []any{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3"}
		}, false},
		{"a key whose run is not a ULID's alphabet", func(b map[string]any) {
			b["tasks"] = []any{"01jmz8v1p9c4xq7k2n4d6f8h0a/normalize/1"}
		}, false},
		{"a key whose shard is past its cardinality", func(b map[string]any) {
			b["tasks"] = []any{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/9/8"}
		}, true},
		{"no sent_at", func(b map[string]any) { delete(b, "sent_at") }, false},
		{"a sent_at with no offset", func(b map[string]any) { b["sent_at"] = "2026-09-10T06:41:09.104" }, true},
		{"a sent_at with a comma before its fraction", func(b map[string]any) { b["sent_at"] = "2026-09-10T06:41:09,104Z" }, true},
		{"a sent_at on a day no month has", func(b map[string]any) { b["sent_at"] = "2026-02-30T06:41:09Z" }, true},
		{"a sent_at as a number", func(b map[string]any) { b["sent_at"] = 1789000000 }, false},
		{"the labels it joined with", func(b map[string]any) { b["labels"] = []any{"zone=dmz"} }, false},
		{"an interval it would like", func(b map[string]any) { b["interval_seconds"] = 60 }, false},
	} {
		request := valid()
		c.with(request)

		byWire := conforms(t, "/$defs/runnerHeartbeat/properties/request", request)
		switch {
		case c.patternOnly && byWire != nil:
			t.Errorf("a heartbeat with %s is refused by the wire too, so it belongs with the rest: %s", c.name, byWire)
		case !c.patternOnly && byWire == nil:
			t.Errorf("a heartbeat with %s is accepted by the wire, so this case tests nothing of it", c.name)
		}

		w, answer := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, request)
		if w.Code != http.StatusBadRequest {
			t.Errorf("a heartbeat with %s answered %d: %s", c.name, w.Code, w.Body)
			continue
		}
		if said, _ := answer["error"].(string); said == "" {
			t.Errorf("a heartbeat with %s was refused with %q", c.name, said)
		}
	}
	if seen := inventoried(t, h, runner)["last_seen_at"]; seen != nil {
		t.Errorf("a runner none of whose heartbeats was taken is recorded as heard from at %v", seen)
	}

	// And the heartbeat the cases were made from is taken, so what was refused was the change.
	if w, _ := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, valid()); w.Code != http.StatusOK {
		t.Errorf("the heartbeat every case changes answered %d: %s", w.Code, w.Body)
	}
}

// "a heartbeat whose named runner is not the one that credential belongs to is refused rather than
// believed". A runner speaks for itself alone, and a heartbeat speaking for another one records
// nothing: neither runner is heard from, and neither is described by what it said.
func TestAHeartbeatSpeakingForAnotherRunnerIsRefused(t *testing.T) {
	h, pool := withRunners(t)
	mine, credential := joined(t, h, pool)
	theirs, _ := joined(t, h, pool)

	speaking := aBeat(theirs)
	speaking.AgentVersion = "9.9.9"
	w, answer := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, speaking)
	if w.Code != http.StatusForbidden {
		t.Errorf("a heartbeat speaking for another runner answered %d: %s", w.Code, w.Body)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, theirs) || !strings.Contains(said, mine) {
		t.Errorf("a heartbeat speaking for another runner was refused with %q, which names neither", said)
	}
	for _, runner := range []string{mine, theirs} {
		held := inventoried(t, h, runner)
		if held["last_seen_at"] != nil || held["agent_version"] != "0.2.0" || held["reported_state"] != nil {
			t.Errorf("a heartbeat speaking for another runner was recorded against %s: %v", runner, held)
		}
	}
}

// The heartbeat's answer is the backstop for a stop on agentiik.stops, which keeps nothing: a runner
// holding the key of a run somebody cancelled is told to stop it, at its next heartbeat, whether or
// not it heard the stop go out.
func TestAHeartbeatIsToldToStopWhatItsCancelledRunHeld(t *testing.T) {
	h, pool, super := runnersOn(t, everything{who: "admin"})
	runner, credential := joined(t, h, pool)

	const run = "01M2HQ8V1P9C4XQ7K2N4D6F8H0"
	conn := dbtest.Superuser(t, super)
	for _, stmt := range []string{
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'alice', now())`,
		`insert into runs (namespace, id, workflow, commit, trigger)
		   values ('finance', '` + run + `', 'monthly-invoicing', 'a3f9c1e', 'manual')`,
		`insert into steps (namespace, run_id, step) values ('finance', '` + run + `', 'render')`,
		`insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at)
		   values ('finance', '01M2HQAAAAAAAAAAAAAAAAAAAA', '` + run + `', 'render', 1, 'running', '` + runner + `', now())`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	key := agk.NewTaskID(run, "render", 1, agk.Shard{})

	// Running, it is kept alive and nothing more.
	w, answer := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner, key))
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %s", w.Code, w.Body)
	}
	if cancel, _ := answer["cancel"].([]any); len(cancel) != 0 {
		t.Errorf("a runner holding a task still running is told to stop %v", cancel)
	}

	if err := pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		_, err := w.CancelTasks(ctx, "finance", run, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w, answer = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner, key))
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %s", w.Code, w.Body)
	}
	if cancel, _ := answer["cancel"].([]any); len(cancel) != 1 || cancel[0] != string(key) {
		t.Errorf("a runner holding a task of a cancelled run is told to stop %v, want %s", answer["cancel"], key)
	}
	if err := conforms(t, "/$defs/runnerHeartbeat/properties/response", answer); err != nil {
		t.Errorf("an answer carrying a stop is not what the wire describes: %s: %v", err, answer)
	}
}
