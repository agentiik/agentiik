package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// A revocation on a real installation, against a real PostgreSQL and a real NATS under the
// installation's operator: the operator revokes a runner, and at its next heartbeat the runner is
// told to drain and until when its results are taken. The bus credential it is then given publishes
// a result the controller hears as that runner's, and pulls nothing, and it redeems nothing.
func TestARevokedRunnerFinishesItsGraceOnTheInstallationsBus(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(t.Context(), database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(t.Context()))
	if _, err := admin.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, &stderr); code != exitStopped {
		t.Fatalf("bus-init exited %d: %s", code, stderr.String())
	}
	natsURL := natsFrom(t, dir)

	s := servingSettings(t, database.Application, dir, natsURL)
	s.RevocationGrace = 10 * time.Minute
	ctx, stop := context.WithCancel(t.Context())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serve(ctx, s, ln, slog.New(slog.DiscardHandler)) }()
	// Stopped and waited for however the test ends, so that nothing is still connected to the
	// database when it is dropped.
	defer func() {
		stop()
		select {
		case <-served:
		case <-time.After(shutdownGrace + 10*time.Second):
			t.Error("serve did not return once stopped")
		}
	}()
	c := client{t: t, base: "http://" + ln.Addr().String(), served: served}

	// The controller's side of the bus, which reads the results back.
	control, err := bus.Open(t.Context(), bus.Options{
		URL: natsURL, Name: "controller", Credentials: &bus.Credentials{JWT: s.Bus.JWT, Seed: string(s.Bus.Seed)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	// A pool, a machine in it, and a heartbeat from it.
	if code, answer := c.do("POST", "/api/v1/runner-pools", theToken, aPool()); code != http.StatusCreated {
		t.Fatalf("the operator's pool answered %d: %v", code, answer)
	}
	code, answer := c.do("POST", "/api/v1/runner-pools/dmz/join-tokens", theToken, api.Issue{Labels: []string{"zone=dmz"}})
	if code != http.StatusCreated {
		t.Fatalf("the operator's join token answered %d: %v", code, answer)
	}
	joinToken, _ := answer["join_token"].(map[string]any)["token"].(string)
	code, answer = c.do("POST", "/api/v1/runners", "", aMachine(joinToken))
	if code != http.StatusCreated {
		t.Fatalf("joining answered %d: %v", code, answer)
	}
	credential, _ := answer["credential"].(string)
	runner, _ := answer["runner"].(string)
	beat := api.Beat{Runner: runner, AgentVersion: "0.2.0", State: "ready", Concurrency: 4, Tasks: []agk.TaskID{}, SentAt: time.Now()}
	if code, answer := c.do("POST", "/api/v1/runners/heartbeat", credential, beat); code != http.StatusOK || answer["drain"] != false {
		t.Fatalf("the heartbeat answered %d: %v", code, answer)
	}

	// Revoked, it is told at its next heartbeat to drain, and until when.
	revokedAt := time.Now()
	if code, answer := c.do("POST", "/api/v1/runners/"+runner+"/revoke", theToken, api.Order{Reason: "the credential leaked"}); code != http.StatusOK || answer["state"] != "revoked" {
		t.Fatalf("the operator's revocation answered %d: %v", code, answer)
	}
	code, answer = c.do("POST", "/api/v1/runners/heartbeat", credential, beat)
	if code != http.StatusOK || answer["drain"] != true || answer["reason"] != "the credential leaked" {
		t.Fatalf("the revoked runner's heartbeat answered %d: %v", code, answer)
	}
	written, _ := answer["results_accepted_until"].(string)
	until, err := time.Parse(time.RFC3339Nano, written)
	if err != nil {
		t.Fatalf("the revoked runner was told its results are accepted until %q", written)
	}
	if grace := until.Sub(revokedAt); grace < 10*time.Minute-time.Minute || grace > 10*time.Minute+time.Minute {
		t.Errorf("the revoked runner was given a grace of %s, and the installation gives ten minutes", grace)
	}

	// The bus credential it is given now publishes its results, and the controller hears them
	// as that runner's.
	code, answer = c.do("POST", "/api/v1/bus/token", credential, nil)
	if code != http.StatusOK {
		t.Fatalf("the revoked runner's bus credential answered %d: %v", code, answer)
	}
	jwt, _ := answer["jwt"].(string)
	seed, _ := answer["seed"].(string)
	expires, _ := time.Parse(time.RFC3339Nano, answer["expires_at"].(string))
	if expires.After(until) {
		t.Errorf("the revoked runner's bus credential expires at %s, after its grace ends at %s", expires, until)
	}
	b, err := bus.OpenRunner(bus.Options{URL: natsURL, Credentials: &bus.Credentials{JWT: jwt, Seed: seed}})
	if err != nil {
		t.Fatalf("the server refused the revoked runner's bus credential: %s", err)
	}
	defer b.Close()

	heard := make(chan string, 4)
	listening, stopListening := context.WithCancel(t.Context())
	defer stopListening()
	go control.Reports(listening, func(_ context.Context, sender string, r bus.TaskResult) error {
		heard <- sender + " " + r.TaskID
		return nil
	})
	key := agk.TaskID("01JMZ8V1P9C4XQ7K2N4D6F8H0A/render/1")
	log, _ := agk.NewLogURI(key)
	exit := 0
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	result := bus.TaskResult{
		TaskID: ulid.New(), IdempotencyKey: string(key), Runner: runner,
		State: agk.TaskSucceeded, ExitCode: &exit, StartedAt: started, FinishedAt: started.Add(30 * time.Second),
		Outputs: []bus.Output{},
		Log:     &bus.Log{URI: log.String(), Lines: 1},
	}
	if err := b.Report(t.Context(), result); err != nil {
		t.Fatalf("the revoked runner's result was refused: %s", err)
	}
	select {
	case got := <-heard:
		if got != runner+" "+result.TaskID {
			t.Errorf("the controller heard %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked runner's result never reached the controller")
	}

	// It pulls nothing from its pool, and redeems nothing.
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := b.Take(short, "dmz", 1, 200*time.Millisecond); err == nil {
		t.Error("the revoked runner took from its pool")
	}
	code, answer = c.do("POST", "/api/v1/tasks/redeem", credential, api.Redemption{
		Grant: "agkgrant_notarealgrantbutlongenoughtolookplausible", TaskID: "01M2T1AAAAAAAAAAAAAAAAAAAA", IdempotencyKey: key,
	})
	if code != http.StatusForbidden {
		t.Errorf("the revoked runner's redemption answered %d: %v", code, answer)
	}
	if said, _ := answer["error"].(string); !strings.Contains(said, "put the message back") {
		t.Errorf("the revoked runner was told %q", said)
	}
}
