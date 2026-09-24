package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// A runner renews its credential with the key it joined with, and a credential past its rotate_by
// opens nothing.

// rotations is an installation whose runner API reads the time off a clock the test moves.
type rotations struct {
	handler http.Handler
	pool    *db.Pool
	clock   *time.Time
	minted  *minting
}

// withRotations stands one up, its clock stopped at a moment of the test's choosing, and its join
// rotation left to the default.
func withRotations(t *testing.T) rotations {
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
	minted := &minting{}
	if _, err := api.NewRunners(rt, api.RunnerOptions{
		Pool: pool, BusIssuer: minted, BusConsumers: &consumers{},
		Now: func() time.Time { return *clock },
	}); err != nil {
		t.Fatal(err)
	}
	return rotations{handler: rt, pool: pool, clock: clock, minted: minted}
}

// host is a machine's keypair, one to a seed, as the agent generates one at join.
func host(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

// joinedAs joins a machine holding key, and answers its runner and its credential.
func (ro rotations) joinedAs(t *testing.T, key ed25519.PrivateKey) (string, string) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	machine := aMachine(issue(t, ro.pool, nil).Clear)
	machine.PublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	w, answer := call(t, ro.handler, "POST", "/api/v1/runners", "", machine)
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	runner, _ := answer["runner"].(string)
	credential, _ := answer["credential"].(string)
	return runner, credential
}

// signed is the rotation a runner sends, signed by key at the test's clock.
func (ro rotations) signed(runner string, key ed25519.PrivateKey) api.Rotation {
	at := ro.clock.Format(time.RFC3339Nano)
	return api.Rotation{
		Runner: runner, At: at,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, api.RotationSigned(runner, at))),
	}
}

// rotated sends a rotation and answers the new credential, failing unless it was given one.
func (ro rotations) rotated(t *testing.T, credential string, rotation api.Rotation) string {
	t.Helper()
	w, answer := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, rotation)
	if w.Code != http.StatusOK {
		t.Fatalf("rotating answered %d: %s", w.Code, w.Body)
	}
	fresh, _ := answer["credential"].(string)
	return fresh
}

// beats answers the status a heartbeat of runner, carrying credential, is answered with.
func (ro rotations) beats(t *testing.T, runner, credential string) int {
	t.Helper()
	w, _ := call(t, ro.handler, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	return w.Code
}

// A rotation signed by the key the runner joined with is answered a new credential, in the wire's
// shape, accepted for the thirty days an installation that says nothing rotates by.
func TestARunnerRotatesItsCredentialWithItsKey(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)

	rotation := ro.signed(runner, key)
	if err := conforms(t, "/$defs/runnerRotation/properties/request", rotation); err != nil {
		t.Fatalf("the rotation is not what the wire describes: %s", err)
	}
	*ro.clock = ro.clock.Add(time.Second)
	w, answer := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, rotation)
	if w.Code != http.StatusOK {
		t.Fatalf("rotating answered %d: %s", w.Code, w.Body)
	}
	if err := conforms(t, "/$defs/runnerRotation/properties/response", answer); err != nil {
		t.Errorf("the answer is not what the wire describes: %s", err)
	}
	if keys := slices.Sorted(maps.Keys(answer)); !slices.Equal(keys, []string{"credential", "rotate_by"}) {
		t.Errorf("the answer carries %v", keys)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("an answer carrying a credential says %q", w.Header().Get("Cache-Control"))
	}
	fresh, _ := answer["credential"].(string)
	if fresh == "" || fresh == credential {
		t.Fatalf("the rotation answered %q", fresh)
	}
	want := ro.clock.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	if answer["rotate_by"] != want {
		t.Errorf("the new credential is accepted until %v, want the moment of the answer plus thirty days, %s", answer["rotate_by"], want)
	}
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Errorf("the new credential's heartbeat answered %d", code)
	}
	listed, _ := inventoried(t, ro.handler, runner)["rotate_by"].(string)
	if at, err := time.Parse(time.RFC3339Nano, listed); err != nil || !at.Equal(ro.clock.Add(30*24*time.Hour)) {
		t.Errorf("the inventory says the runner rotates by %q, want %s", listed, want)
	}
}

// "The key proves the machine: a stolen credential without it cannot be renewed." A signature by
// any other key, another runner's included, or over anything but this runner and this time, renews
// nothing, and the credential presented keeps working as it did.
func TestARotationNotSignedByTheRunnersKeyRenewsNothing(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	other := host(2)
	ro.joinedAs(t, other)

	good := ro.signed(runner, key)
	overAnotherTime := good
	overAnotherTime.At = ro.clock.Add(time.Second).Format(time.RFC3339Nano)
	overAnotherSpelling := good
	overAnotherSpelling.At = ro.clock.Format("2006-01-02T15:04:05.000000Z07:00")

	for name, rotation := range map[string]api.Rotation{
		"signed by another runner's key":           ro.signed(runner, other),
		"signed by a key nobody joined with":       ro.signed(runner, host(3)),
		"signed over another time":                 overAnotherTime,
		"signed over another spelling of the time": overAnotherSpelling,
	} {
		w, answer := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, rotation)
		if w.Code != http.StatusForbidden {
			t.Errorf("a rotation %s answered %d: %s", name, w.Code, w.Body)
		}
		if _, there := answer["credential"]; there {
			t.Errorf("a rotation %s was given a credential", name)
		}
	}
	if code := ro.beats(t, runner, credential); code != http.StatusOK {
		t.Errorf("after refused rotations the credential's heartbeat answered %d", code)
	}

	// And the good one still works, so the refusals above were about the signature.
	ro.rotated(t, credential, good)
}

// "The old credential keeps working until the new one is first used, so a lost answer locks nobody
// out." Once the new one has been presented, the old one opens nothing.
func TestTheOldCredentialWorksUntilTheNewOneIsFirstUsed(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, old := ro.joinedAs(t, key)

	fresh := ro.rotated(t, old, ro.signed(runner, key))
	if code := ro.beats(t, runner, old); code != http.StatusOK {
		t.Fatalf("the old credential, before the new one was used, answered %d", code)
	}
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Fatalf("the new credential answered %d", code)
	}
	if code := ro.beats(t, runner, old); code != http.StatusUnauthorized {
		t.Errorf("the old credential, after the new one was used, answered %d", code)
	}
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Errorf("the new credential, used a second time, answered %d", code)
	}
}

// An answer that never arrived: the runner rotates again with what it still holds, and the
// credential it was never given is the one dropped.
func TestARunnerWhoseAnswerWasLostRotatesAgain(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, old := ro.joinedAs(t, key)

	lost := ro.rotated(t, old, ro.signed(runner, key))
	*ro.clock = ro.clock.Add(time.Second)
	fresh := ro.rotated(t, old, ro.signed(runner, key))

	if code := ro.beats(t, runner, lost); code != http.StatusUnauthorized {
		t.Errorf("the credential whose answer was lost answered %d", code)
	}
	if code := ro.beats(t, runner, old); code != http.StatusOK {
		t.Errorf("the credential it rotated with twice answered %d", code)
	}
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Errorf("the credential it was given the second time answered %d", code)
	}
	if code := ro.beats(t, runner, old); code != http.StatusUnauthorized {
		t.Errorf("the old credential, after the new one was used, answered %d", code)
	}
}

// A signed rotation is good once, and only near the moment it was signed, so that a copy of one is
// not a way to rotate.
func TestASignedRotationIsGoodOnceAndOnlyNearItsTime(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)

	rotation := ro.signed(runner, key)
	fresh := ro.rotated(t, credential, rotation)
	w, _ := call(t, ro.handler, "POST", "/api/v1/runners/rotate", fresh, rotation)
	if w.Code != http.StatusConflict {
		t.Errorf("the same signed rotation, sent again, answered %d: %s", w.Code, w.Body)
	}

	for name, moved := range map[string]time.Duration{
		"signed six minutes ago":      -6 * time.Minute,
		"signed six minutes from now": 6 * time.Minute,
	} {
		signedAt := ro.clock.Add(moved)
		at := signedAt.Format(time.RFC3339Nano)
		late := api.Rotation{
			Runner: runner, At: at,
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, api.RotationSigned(runner, at))),
		}
		w, _ := call(t, ro.handler, "POST", "/api/v1/runners/rotate", fresh, late)
		if w.Code != http.StatusBadRequest {
			t.Errorf("a rotation %s answered %d: %s", name, w.Code, w.Body)
		}
	}

	*ro.clock = ro.clock.Add(time.Millisecond)
	ro.rotated(t, fresh, ro.signed(runner, key))
}

// A rotation speaks for the runner its credential opens, and is written as the wire writes it.
func TestARotationTheWireRefusesIsRefused(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	other, _ := ro.joinedAs(t, host(2))

	w, _ := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, ro.signed(other, key))
	if w.Code != http.StatusForbidden {
		t.Errorf("a rotation speaking for another runner answered %d: %s", w.Code, w.Body)
	}

	good := ro.signed(runner, key)
	for name, body := range map[string]map[string]any{
		"no runner":             {"at": good.At, "signature": good.Signature},
		"no at":                 {"runner": runner, "signature": good.Signature},
		"no signature":          {"runner": runner, "at": good.At},
		"a signature too short": {"runner": runner, "at": good.At, "signature": good.Signature[4:]},
		"a signature in base64url": {"runner": runner, "at": good.At,
			"signature": base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, ed25519.SignatureSize))},
		"an at that is no instant": {"runner": runner, "at": "yesterday", "signature": good.Signature},
		"a field nobody knows":     {"runner": runner, "at": good.At, "signature": good.Signature, "public_key": aKey},
	} {
		// The wire's format: date-time is an annotation to a validator that asserts none,
		// which is why the API checks the instant's grammar itself.
		if name != "an at that is no instant" {
			if err := conforms(t, "/$defs/runnerRotation/properties/request", body); err == nil {
				t.Errorf("the wire accepts a rotation with %s, which this test says it refuses", name)
			}
		}
		w, _ := call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("a rotation with %s answered %d: %s", name, w.Code, w.Body)
		}
	}
	if code := ro.beats(t, runner, credential); code != http.StatusOK {
		t.Errorf("after refused rotations the credential's heartbeat answered %d", code)
	}
}

// "The rotation window is rotate_by itself: a credential past it is refused everywhere, and that
// host joins again." Rotation included, since renewing is what the window is there to stop.
func TestACredentialPastItsRotateByOpensNothing(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, credential := ro.joinedAs(t, key)
	joinedAt := *ro.clock

	*ro.clock = joinedAt.Add(30*24*time.Hour - time.Millisecond)
	if code := ro.beats(t, runner, credential); code != http.StatusOK {
		t.Fatalf("a credential a moment before its rotate_by answered %d", code)
	}

	*ro.clock = joinedAt.Add(30 * 24 * time.Hour)
	if code := ro.beats(t, runner, credential); code != http.StatusUnauthorized {
		t.Errorf("a heartbeat at its credential's rotate_by answered %d", code)
	}
	w, _ := call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("asking for a bus credential at its rotate_by answered %d", w.Code)
	}
	w, _ = call(t, ro.handler, "POST", "/api/v1/runners/rotate", credential, ro.signed(runner, key))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("rotating at its rotate_by answered %d", w.Code)
	}
	if len(ro.minted.names) != 0 {
		t.Errorf("a credential past its rotate_by was minted a bus credential")
	}
}

// The credential a runner rotated with stays accepted until the new one is used, and never past
// its own rotate_by: rotating is not a way for the old one to live longer.
func TestTheOldCredentialStillStopsAtItsOwnRotateBy(t *testing.T) {
	ro := withRotations(t)
	key := host(1)
	runner, old := ro.joinedAs(t, key)
	joinedAt := *ro.clock

	*ro.clock = joinedAt.Add(20 * 24 * time.Hour)
	fresh := ro.rotated(t, old, ro.signed(runner, key))

	*ro.clock = joinedAt.Add(30 * 24 * time.Hour)
	if code := ro.beats(t, runner, old); code != http.StatusUnauthorized {
		t.Errorf("the old credential past its own rotate_by answered %d", code)
	}
	if code := ro.beats(t, runner, fresh); code != http.StatusOK {
		t.Errorf("the new credential answered %d", code)
	}
}

// A bus credential lasts no longer than the runner credential it was asked for with.
func TestABusCredentialEndsNoLaterThanTheRunnerCredential(t *testing.T) {
	ro := withRotations(t)
	_, credential := ro.joinedAs(t, host(1))
	joinedAt := *ro.clock

	w, answer := call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("asking for a bus credential answered %d: %s", w.Code, w.Body)
	}
	if want := joinedAt.Add(api.BusLife).Truncate(time.Second).Format(time.RFC3339Nano); answer["expires_at"] != want {
		t.Errorf("a bus credential a month from its runner credential's end expires at %v, want %s", answer["expires_at"], want)
	}

	*ro.clock = joinedAt.Add(30*24*time.Hour - 10*time.Minute)
	w, answer = call(t, ro.handler, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("asking for a bus credential answered %d: %s", w.Code, w.Body)
	}
	if want := joinedAt.Add(30 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339Nano); answer["expires_at"] != want {
		t.Errorf("a bus credential ten minutes from its runner credential's end expires at %v, want %s", answer["expires_at"], want)
	}
}

// The wire's own examples of a rotation are rotations it accepts, and their signatures are what
// RotationSigned says a signature is over, so that a runner written from the wire alone and this
// API sign the same bytes.
func TestTheWiresRotationExamplesAreSignedAsTheAPIReadsThem(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			RunnerRotation struct {
				Examples []struct {
					Request  map[string]string `json:"request"`
					Response map[string]string `json:"response"`
				} `json:"examples"`
			} `json:"runnerRotation"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	examples := schema.Defs.RunnerRotation.Examples
	if len(examples) == 0 {
		t.Fatal("the wire gives no example of a rotation")
	}
	for seed, example := range examples {
		if err := conforms(t, "/$defs/runnerRotation", example); err != nil {
			t.Errorf("the wire's example %d is not what the wire describes: %s", seed, err)
		}
		// The examples are signed by the hosts whose seeds are 1 and 2, in order.
		key := host(byte(seed + 1))
		signature, err := base64.StdEncoding.DecodeString(example.Request["signature"])
		if err != nil {
			t.Fatal(err)
		}
		if !ed25519.Verify(key.Public().(ed25519.PublicKey), api.RotationSigned(example.Request["runner"], example.Request["at"]), signature) {
			t.Errorf("the wire's example %d is not signed over what RotationSigned composes", seed)
		}
	}
}
