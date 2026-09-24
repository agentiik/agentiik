package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Draining and revoking a runner: two orders an administrator gives, and what each leaves the
// runner able to do. "Revoking a credential never destroys work already done."

// withGrace stands up the runner routes with a clock the test moves, runners whose credentials are
// accepted for rotation, and revocations whose grace is grace.
func withGrace(t *testing.T, rotation, grace time.Duration) (rotations, *consumers) {
	t.Helper()
	return withGraceAndBefore(t, rotation, grace, new(func()))
}

// withGraceAndBefore is withGrace, running what before holds, once, the next time the API reads its
// clock, so that a test can land something between two moments of one request.
func withGraceAndBefore(t *testing.T, rotation, grace time.Duration, before *func()) (rotations, *consumers) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "admin"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	clock := new(time.Time)
	*clock = time.Now().UTC().Truncate(time.Millisecond)
	minted, made := &minting{}, &consumers{}
	if _, err := api.NewRunners(rt, api.RunnerOptions{
		Pool: pool, JoinRotation: rotation, RevocationGrace: grace,
		BusIssuer: minted, BusConsumers: made,
		Now: func() time.Time {
			if f := *before; f != nil {
				*before = nil
				f()
			}
			return *clock
		},
	}); err != nil {
		t.Fatal(err)
	}
	return rotations{handler: rt, pool: pool, clock: clock, minted: minted}, made
}

// order gives one, as the administrator, and answers the runner as the route answers it.
func (ro rotations) order(t *testing.T, runner, verb, reason string) map[string]any {
	t.Helper()
	w, answer := call(t, ro.handler, "POST", "/api/v1/runners/"+runner+"/"+verb, "admin", api.Order{Reason: reason})
	if w.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", verb, w.Code, w.Body)
	}
	return answer
}

// An order says why in one line, names a runner that is there, and is answered with the runner as
// the inventory lists it, who ordered it and when. Given twice, the first stands: a drain is not
// written over, a revocation is given no more time, and a drain is not a way back from one.
func TestDrainingAndRevokingARunnerAreOrdersWithAReason(t *testing.T) {
	ro, _ := withGrace(t, 30*24*time.Hour, 10*time.Minute)
	runner, _ := ro.joinedAs(t, host(1))

	for name, c := range map[string]struct {
		path string
		body any
		want int
	}{
		"with no reason":                   {"/api/v1/runners/" + runner + "/drain", api.Order{}, http.StatusBadRequest},
		"with a reason of two lines":       {"/api/v1/runners/" + runner + "/drain", api.Order{Reason: "retired\nfor good"}, http.StatusBadRequest},
		"with a reason longer than a line": {"/api/v1/runners/" + runner + "/revoke", api.Order{Reason: strings.Repeat("é", 257)}, http.StatusBadRequest},
		"with a field it does not read":    {"/api/v1/runners/" + runner + "/revoke", map[string]any{"reason": "leaked", "grace": "1h"}, http.StatusBadRequest},
		"with no body":                     {"/api/v1/runners/" + runner + "/revoke", nil, http.StatusBadRequest},
		"for a runner that is not there":   {"/api/v1/runners/nobody/drain", api.Order{Reason: "retired"}, http.StatusNotFound},
		"for a name no runner can have":    {"/api/v1/runners/Runner%20One/revoke", api.Order{Reason: "leaked"}, http.StatusNotFound},
	} {
		if w, _ := call(t, ro.handler, "POST", c.path, "admin", c.body); w.Code != c.want {
			t.Errorf("an order %s answered %d, want %d: %s", name, w.Code, c.want, w.Body)
		}
	}
	if held := inventoried(t, ro.handler, runner); held["state"] != "ready" {
		t.Fatalf("refused orders left the runner %v", held["state"])
	}
	if w, _ := call(t, ro.handler, "POST", "/api/v1/runners/"+runner+"/drain", "admin", api.Order{Reason: strings.Repeat("é", 256)}); w.Code != http.StatusOK {
		t.Errorf("a reason of 256 characters answered %d: %s", w.Code, w.Body)
	}

	drainedAt := *ro.clock
	*ro.clock = ro.clock.Add(time.Minute)
	drained := ro.order(t, runner, "drain", "the host is being retired")
	if drained["state"] != "draining" || drained["drained_by"] != "admin" || !when(t, drained["drained_at"]).Equal(drainedAt) {
		t.Errorf("a runner drained twice is answered %v, and the first order stands", drained)
	}

	revokedAt := *ro.clock
	revoked := ro.order(t, runner, "revoke", "the credential leaked")
	if revoked["state"] != "revoked" || revoked["revoked_by"] != "admin" || revoked["drain_reason"] != "the credential leaked" {
		t.Errorf("a revoked runner is answered %v", revoked)
	}
	until := revokedAt.Add(10 * time.Minute)
	if !when(t, revoked["results_accepted_until"]).Equal(until) || !when(t, revoked["revoked_at"]).Equal(revokedAt) {
		t.Errorf("a runner revoked with a grace of ten minutes has its results accepted until %v, want %s", revoked["results_accepted_until"], until)
	}

	*ro.clock = ro.clock.Add(5 * time.Minute)
	if again := ro.order(t, runner, "revoke", "revoked again"); !when(t, again["results_accepted_until"]).Equal(until) {
		t.Errorf("revoking again moved the end of the grace to %v, from %s", again["results_accepted_until"], until)
	}
	w, _ := call(t, ro.handler, "POST", "/api/v1/runners/"+runner+"/drain", "admin", api.Order{Reason: "back to draining"})
	if w.Code != http.StatusConflict {
		t.Errorf("draining a revoked runner answered %d: %s", w.Code, w.Body)
	}
	if held := inventoried(t, ro.handler, runner); held["state"] != "revoked" || !when(t, held["results_accepted_until"]).Equal(until) {
		t.Errorf("the inventory holds the revoked runner as %v", held)
	}
}

// In its grace a revoked runner is heard, told to drain and until when, given a bus credential that
// publishes results and pulls nothing, and renews nothing. From the end of the grace every call it
// makes is refused.
func TestARevokedRunnerIsHeardUntilItsGraceEnds(t *testing.T) {
	ro, made := withGrace(t, 30*24*time.Hour, 10*time.Minute)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	revokedAt := *ro.clock
	ro.order(t, runner, "revoke", "the credential leaked")
	end := revokedAt.Add(10 * time.Minute)

	*ro.clock = end.Add(-time.Millisecond)
	w, said := call(t, ro.handler, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	if w.Code != http.StatusOK {
		t.Fatalf("a revoked runner's heartbeat in its grace answered %d: %s", w.Code, w.Body)
	}
	if err := conforms(t, "/$defs/runnerHeartbeat/properties/response", said); err != nil {
		t.Errorf("a revoked runner is not answered as the wire describes: %s: %v", err, said)
	}
	if said["drain"] != true || said["results_accepted_until"] != end.Format(time.RFC3339Nano) {
		t.Errorf("a revoked runner in its grace was told %v", said)
	}

	w, answer := call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a revoked runner's bus credential answered %d: %s", w.Code, w.Body)
	}
	if !slices.Equal(ro.minted.revoked, []string{runner}) || len(ro.minted.names) != 0 {
		t.Errorf("a revoked runner was minted %v and %v, want the narrower credential alone", ro.minted.revoked, ro.minted.names)
	}
	if !ro.minted.until[0].Equal(end) {
		t.Errorf("a revoked runner's bus credential lasts until %s, past the end of its grace, %s", ro.minted.until[0], end)
	}
	if answer["jwt"] != "a.narrower.credential" {
		t.Errorf("a revoked runner was answered %v", answer)
	}
	if len(made.made) != 0 {
		t.Errorf("a queue was made ready for a runner that may take nothing from it: %v", made.made)
	}

	w, _ = call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, ro.signed(runner, key))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "revoked") {
		t.Errorf("a revoked runner's rotation in its grace answered %d: %s", w.Code, w.Body)
	}

	*ro.clock = end
	for _, r := range []struct {
		path string
		body any
	}{
		{"/api/v1/runners/heartbeat", aBeat(runner)},
		{"/api/v1/bus/token", nil},
		{"/api/v1/runners/rotate", ro.signed(runner, key)},
		{"/api/v1/tasks/redeem", asking("agkgrant_whatever")},
	} {
		if w, _ := call(t, ro.handler, "POST", r.path, credential, r.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s by a revoked runner at the end of its grace answered %d: %s", r.path, w.Code, w.Body)
		}
	}
}

// A revoked runner rotates nothing, so the grace it is told of ends no later than the credential it
// holds: "the grace period is stated on the wire rather than left to each side's arithmetic".
func TestTheGraceAnsweredEndsNoLaterThanTheCredential(t *testing.T) {
	ro, _ := withGrace(t, 30*time.Minute, time.Hour)
	runner, credential := ro.joinedAs(t, host(1))
	rotateBy := ro.clock.Add(30 * time.Minute)
	ro.order(t, runner, "revoke", "the credential leaked")

	_, said := call(t, ro.handler, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	if said["results_accepted_until"] != rotateBy.Format(time.RFC3339Nano) {
		t.Errorf("a revoked runner whose credential ends in thirty minutes is told its results are accepted until %v, want %s", said["results_accepted_until"], rotateBy.Format(time.RFC3339Nano))
	}
	call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil)
	if len(ro.minted.until) != 1 || !ro.minted.until[0].Equal(rotateBy) {
		t.Errorf("its bus credential lasts until %v, want %s", ro.minted.until, rotateBy)
	}
}

// A rotation the hook let through a moment before the runner was revoked is told the runner is
// revoked, as one after it is, rather than that its credential opens nothing, which would send the
// runner off to join again in the middle of its grace.
func TestARotationCrossingARevocationIsToldTheRunnerIsRevoked(t *testing.T) {
	before := new(func())
	ro, _ := withGraceAndBefore(t, 30*24*time.Hour, time.Hour, before)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	*ro.clock = ro.clock.Add(time.Second)

	// The hook reads the clock first, and the rotation next: the revocation lands between them.
	*before = func() {
		*before = func() {
			if err := ro.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
				_, err := w.Revoke(ctx, runner, "admin", "the credential leaked", *ro.clock, time.Hour)
				return err
			}); err != nil {
				t.Error(err)
			}
		}
	}
	w, _ := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, ro.signed(runner, key))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "revoked") {
		t.Errorf("a rotation crossing a revocation answered %d: %s", w.Code, w.Body)
	}
	if held := inventoried(t, ro.handler, runner); held["state"] != "revoked" {
		t.Fatalf("the revocation did not land: %v", held["state"])
	}
	if code := ro.beats(t, runner, credential); code != http.StatusOK {
		t.Errorf("after the refused rotation the revoked runner's heartbeat answered %d", code)
	}
}

// A NATS credential is not revoked but runs out, so every bus credential runs out no later than a
// revocation's grace would: one minted a moment before a revocation is gone by the grace's end.
func TestNoBusCredentialOutlivesAGrace(t *testing.T) {
	ro, _ := withGrace(t, 30*24*time.Hour, 10*time.Minute)
	_, credential := ro.joinedAs(t, host(1))
	if w, _ := call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil); w.Code != http.StatusOK {
		t.Fatalf("a ready runner's bus credential answered %d: %s", w.Code, w.Body)
	}
	want := ro.clock.Add(10 * time.Minute)
	if got := mintedUntil(t, ro); !got.Equal(want) {
		t.Errorf("a ready runner's bus credential lasts until %s, past a grace that would end at %s", got, want)
	}
}

// A draining runner is only told to take nothing new: its bus credential is the one it always had,
// and it renews its runner credential, since it stays up for as long as it is left drained.
func TestADrainingRunnerKeepsItsCredentials(t *testing.T) {
	ro, made := withGrace(t, 30*24*time.Hour, time.Hour)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	ro.order(t, runner, "drain", "the host is being retired")

	if w, _ := call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil); w.Code != http.StatusOK {
		t.Fatalf("a draining runner's bus credential answered %d: %s", w.Code, w.Body)
	}
	if !slices.Equal(ro.minted.names, []string{runner}) || len(ro.minted.revoked) != 0 || !slices.Equal(made.made, []string{"dmz"}) {
		t.Errorf("a draining runner was minted %v and %v, with queues %v", ro.minted.names, ro.minted.revoked, made.made)
	}

	*ro.clock = ro.clock.Add(time.Second)
	fresh := ro.rotated(t, credential, ro.signed(runner, key))
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Errorf("a draining runner's rotated credential answered %d", code)
	}
}

// "A redemption by a draining or revoked runner gets 403, binds nothing, and the runner puts the
// message back with Again": the task is nobody's yet, and a runner that takes work redeems it. What
// a runner already holds it redeems again once drained or revoked, after a lost answer or a
// restart, since finishing it is what it is left to do.
func TestADrainingOrRevokedRunnerRedeemsNothing(t *testing.T) {
	g := withGrants(t, held{"finance/stripe": "sk_live_notreal"})
	clear, _, _ := g.dispatched(t, []string{"stripe"})

	for _, verb := range []string{"drain", "revoke"} {
		credential := g.joined(t)
		runner := runnerOf(t, g, credential)
		if w, _ := call(t, g.handler, "POST", "/api/v1/runners/"+runner+"/"+verb, "admin", api.Order{Reason: "the host is being retired"}); w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", verb, w.Code, w.Body)
		}
		w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
		if w.Code != http.StatusForbidden {
			t.Errorf("a redemption after a %s answered %d: %s", verb, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "sk_live_notreal") {
			t.Errorf("a refused redemption after a %s carried the secret", verb)
		}
		if bound := g.bound(t); bound != nil {
			t.Fatalf("a redemption after a %s bound the task to %s", verb, *bound)
		}
	}

	// The grant was good all along.
	holder := g.joined(t)
	g.redeemed(t, holder, asking(clear))
	for _, verb := range []string{"drain", "revoke"} {
		if w, _ := call(t, g.handler, "POST", "/api/v1/runners/"+runnerOf(t, g, holder)+"/"+verb, "admin", api.Order{Reason: "the host is being retired"}); w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", verb, w.Code, w.Body)
		}
		if answer := g.redeemed(t, holder, asking(clear)); len(answer.Secrets) != 1 {
			t.Errorf("the holder's redemption after a %s answered %+v", verb, answer)
		}
	}
	if bound := g.bound(t); bound == nil || *bound != runnerOf(t, g, holder) {
		t.Errorf("the task is held by %v", bound)
	}
}

// runnerOf is the runner a credential opens.
func runnerOf(t *testing.T, g grants, credential string) string {
	t.Helper()
	var runner string
	if err := g.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		r, err := w.Authenticate(ctx, credential, time.Now().UTC())
		runner = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return runner
}

// when reads an instant the way the API writes one, and fails the test for anything else.
func when(t *testing.T, written any) time.Time {
	t.Helper()
	s, _ := written.(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%v is not an instant: %s", written, err)
	}
	return at
}

// mintedUntil is until when the last full bus credential was minted.
func mintedUntil(t *testing.T, ro rotations) time.Time {
	t.Helper()
	if len(ro.minted.full) == 0 {
		t.Fatal("no bus credential was minted")
	}
	return ro.minted.full[len(ro.minted.full)-1]
}
