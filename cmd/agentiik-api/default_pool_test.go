package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/runner"
)

// A fresh installation as Get started makes one: migrate, a namespace created with agentiik-api
// namespace create, the bus, serve. The operator issues a join token for the pool default, which
// the installation is migrated with and which carries no label, permitting none; a host joins it
// with agk-runner's own join and no --labels, and serve reads what join wrote. The runner then
// heartbeats and takes from the pool default's consumer, which is where every step naming no
// runs_on is published. Before, join refused a host claiming no label, so the pool default had no
// runner and a first workflow without runs_on ran nowhere.
func TestAFreshInstallationTakesARunnerOfThePoolDefaultClaimingNoLabel(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := namespace(t.Context(), database.Application, "create", "finance", io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	natsURL := natsFrom(t, dir)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, servingSettings(t, database.Application, dir, natsURL), ln, slog.New(slog.DiscardHandler))
	}()
	c := client{t: t, base: "http://" + ln.Addr().String(), served: served}

	// A join token of the pool default, permitting no label.
	code, answer := c.do("POST", "/api/v1/runner-pools/default/join-tokens", theToken, api.Issue{})
	if code != http.StatusCreated {
		t.Fatalf("the join token of the pool default answered %d: %v", code, answer)
	}
	issued, _ := answer["join_token"].(map[string]any)
	joinToken, _ := issued["token"].(string)

	// The host joins with agk-runner's join, claiming no label.
	daemon, err := dockertest.NewDaemon()
	if err != nil {
		t.Fatalf("starting a fake daemon: %s", err)
	}
	defer daemon.Close()
	root := t.TempDir()
	env := filepath.Join(root, "etc", "runner.env")
	work := filepath.Join(root, "var", "work")
	joined, err := runner.Join(t.Context(), runner.Joining{
		API: c.base, Token: runner.Secret(joinToken),
		Lookup: func(name string) (string, bool) {
			if name == runner.WorkDir {
				return work, true
			}
			return "", false
		},
		Owner:   &runner.Owner{UID: os.Getuid(), GID: os.Getgid()},
		EnvPath: env, KeyPath: filepath.Join(root, "var", "runner.key"),
		MemInfo:        filepath.Join("..", "..", "runner", "testdata", "meminfo"),
		CredentialPath: filepath.Join(root, "var", "credential"),
		Socket:         daemon.Socket(),
	})
	if err != nil {
		t.Fatalf("a host claiming no label could not join the pool default: %s", err)
	}
	if joined.Pool != "default" || len(joined.Labels) != 0 {
		t.Fatalf("the host joined %+v", joined)
	}
	serving, err := runner.ReadConfig(func(name string) (string, bool) { return "", false }, env)
	if err != nil {
		t.Fatalf("serve refuses what join wrote: %s", err)
	}
	if serving.Pool != "default" || serving.Labels != nil {
		t.Errorf("serve reads pool %s claiming %q", serving.Pool, serving.Labels)
	}

	// It heartbeats, and takes from the pool default.
	credential := string(serving.Credential)
	beat := api.Beat{Runner: joined.Runner, AgentVersion: "0.2.1", State: "ready", Concurrency: 1, Tasks: []agk.TaskID{}, SentAt: time.Now()}
	if code, answer := c.do("POST", "/api/v1/runners/heartbeat", credential, beat); code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %v", code, answer)
	}
	code, answer = c.do("POST", "/api/v1/bus/token", credential, nil)
	if code != http.StatusOK || answer["consumer"] != "default" {
		t.Fatalf("the bus credential answered %d: %v", code, answer)
	}
	jwt, _ := answer["jwt"].(string)
	seed, _ := answer["seed"].(string)
	b, err := bus.OpenRunner(bus.Options{URL: natsURL, Credentials: &bus.Credentials{JWT: jwt, Seed: seed}})
	if err != nil {
		t.Fatalf("the server refused the runner's bus credential: %s", err)
	}
	defer b.Close()
	if _, err := b.Take(t.Context(), "default", 1, 200*time.Millisecond); err != nil {
		t.Errorf("the runner could not take from the pool default: %s", err)
	}
}
