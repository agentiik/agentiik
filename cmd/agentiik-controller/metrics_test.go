package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The metrics as a scraper reads them, from two instances of the program on a real PostgreSQL and a
// real NATS: the one that leads reports every figure, the standby only that it stands by, and
// neither answers a scrape without the token.

// scrapeToken is the token the tests' scraper bears, and scrapeHash what the program is given.
const scrapeToken = "s3cret-scrape-token"

var scrapeHash = func() string {
	sum := sha256.Sum256([]byte(scrapeToken))
	return hex.EncodeToString(sum[:])
}()

// freeAddress is a loopback address nothing listens on, a moment ago.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// scrape reads the metrics at addr bearing token, and answers the status and the body.
func scrape(t *testing.T, addr, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// has says whether every line is in body.
func has(body string, lines ...string) bool {
	for _, l := range lines {
		if !strings.Contains(body, "\n"+l+"\n") {
			return false
		}
	}
	return true
}

// Two controllers, each answering its metrics. The leader answers every figure, read from the
// database and the bus as they stand: the task it dispatched, the queues of both pools and the label
// of one, and the runner's slots. The standby answers that it stands by and nothing of the leader's.
// Neither answers a scrape that does not bear the token.
func TestTheLeaderAnswersEveryFigureAndTheStandbyThatItStandsBy(t *testing.T) {
	super := dbtest.Migrated(t)
	pool, err := db.Open(t.Context(), dbtest.Application(super))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	seeded(t, pool, super)
	conn := dbtest.Superuser(t, super)

	// A second pool carrying a label, and a runner of the pool default that reported that it
	// runs four tasks at once, stamped an hour ahead so that it is heard from however slowly
	// the test goes.
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "gpu", Labels: []string{"accelerator=gpu"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `
		insert into runners (id, pool, cpu, memory_bytes, disk_bytes, architecture, agent_version,
		                     credential_hash, rotate_by, public_key, reported_state, concurrency, last_heartbeat_at)
		values ('runner-1', 'default', 4, 8589934592, 137438953472, 'amd64', '0.2.0',
		        repeat('a', 64), now() + interval '30 days', decode(repeat('00', 32), 'hex'), 'ready', 4, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}

	b := withInstallationBus(t)
	js := b.streams(t)
	credential := b.controlPlane(t, "agentiik-controller")
	c := instanceConfig{
		Database: dbtest.Application(super), Bus: b.url,
		JWT: credential.JWT, Seed: credential.Seed, Objects: t.TempDir(),
		MetricsTokenHash: scrapeHash,
	}
	first, second := c, c
	first.MetricsListen, second.MetricsListen = freeAddress(t), freeAddress(t)
	one, other := startController(t, first), startController(t, second)
	both := []*process{one, other}

	var leader, standby string
	eventually(t, 30*time.Second, "a controller leading", func() bool {
		switch termOf(t, conn).holder {
		case one.name:
			leader, standby = first.MetricsListen, second.MetricsListen
		case other.name:
			leader, standby = second.MetricsListen, first.MetricsListen
		}
		return leader != ""
	}, both...)

	// A run whose first step goes out on the queue of the pool default.
	started(t, pool, goodCommit)
	eventually(t, 30*time.Second, "the leader publishing the ready task", func() bool {
		return held(t, js, "AGENTIIK_TASKS") == 1
	}, both...)

	var body string
	eventually(t, 30*time.Second, "the leader answering every figure", func() bool {
		var status int
		status, body = scrape(t, leader, scrapeToken)
		return status == http.StatusOK && has(body,
			`agentiik_controller_leading 1`,
			`agentiik_tasks_dispatched_total{pool="default"} 1`,
			`agentiik_queue_depth{pool="default"} 1`,
			`agentiik_queue_depth{pool="gpu"} 0`,
			`agentiik_runner_pool_label{pool="gpu",label="accelerator=gpu"} 1`,
			`agentiik_runner_slots{pool="default",runner="runner-1"} 4`,
			`agentiik_runner_tasks{pool="default",runner="runner-1"} 0`,
			`agentiik_runner_ready{pool="default",runner="runner-1"} 1`,
		)
	}, both...)

	status, body := scrape(t, standby, scrapeToken)
	if status != http.StatusOK || !has(body, `agentiik_controller_leading 0`) {
		t.Fatalf("the standby answered %d:\n%s", status, body)
	}
	for _, figure := range []string{"agentiik_queue_depth{", "agentiik_runner_slots{", "agentiik_tasks_dispatched_total{"} {
		if strings.Contains(body, figure) {
			t.Errorf("the standby reported %s, which is the leader's to report:\n%s", figure, body)
		}
	}

	for _, addr := range []string{leader, standby} {
		for _, token := range []string{"", "wrong"} {
			if status, body := scrape(t, addr, token); status != http.StatusUnauthorized || strings.Contains(body, "agentiik_") {
				t.Errorf("a scrape bearing %q was answered %d:\n%s", token, status, body)
			}
		}
	}
}

// What the core tells lands in the family the documentation names, under the labels it names: a
// scripted history of two dispatches on two pools, a loss, a failure retried then a success, and a
// run that ended, read back as a scraper reads it.
func TestWhatTheCoreTellsIsCountedUnderItsLabels(t *testing.T) {
	c := newCounted(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Dispatched("default")
	c.Dispatched("gpu")
	c.Dispatched("default")
	c.Lost("default")
	c.Ended("invoice", "1.0.0", agk.TaskFailed, 30*time.Second)
	c.Retried("invoice", "1.0.0")
	c.Ended("invoice", "1.0.0", agk.TaskSucceeded, 90*time.Second)
	c.Ended("", "", agk.TaskSucceeded, 2*time.Second)
	c.RunEnded("finance", "monthly-invoicing", agk.Succeeded, 17*time.Minute)

	var out bytes.Buffer
	if err := c.registry.WriteTo(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	body := "\n" + out.String()
	for _, line := range []string{
		`agentiik_controller_leading 0`,
		`agentiik_tasks_dispatched_total{pool="default"} 2`,
		`agentiik_tasks_dispatched_total{pool="gpu"} 1`,
		`agentiik_tasks_lost_total{pool="default"} 1`,
		`agentiik_task_retries_total{brick="invoice",version="1.0.0"} 1`,
		`agentiik_task_duration_seconds_bucket{brick="invoice",version="1.0.0",state="failed",le="30"} 1`,
		`agentiik_task_duration_seconds_bucket{brick="invoice",version="1.0.0",state="failed",le="10"} 0`,
		`agentiik_task_duration_seconds_sum{brick="invoice",version="1.0.0",state="succeeded"} 90`,
		`agentiik_task_duration_seconds_count{brick="invoice",version="1.0.0",state="succeeded"} 1`,
		`agentiik_task_duration_seconds_count{brick="",version="",state="succeeded"} 1`,
		`agentiik_run_duration_seconds_bucket{namespace="finance",workflow="monthly-invoicing",state="succeeded",le="600"} 0`,
		`agentiik_run_duration_seconds_bucket{namespace="finance",workflow="monthly-invoicing",state="succeeded",le="1800"} 1`,
		`agentiik_run_duration_seconds_sum{namespace="finance",workflow="monthly-invoicing",state="succeeded"} 1020`,
	} {
		if !has(body, line) {
			t.Errorf("no line %s in\n%s", line, body)
		}
	}
}

// An address the metrics cannot be answered on refuses the start, naming the variable, rather than
// leaving the monitoring to find out that nothing answers.
func TestAMetricsAddressTakenRefusesTheStart(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	c := newCounted(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	stop, err := c.serveMetrics(t.Context(), config.Metrics{Listen: taken.Addr().String(), TokenHash: scrapeHash}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		stop()
		t.Fatal("the metrics were answered on an address something else listens on")
	}
	if !strings.Contains(err.Error(), config.MetricsListen) {
		t.Errorf("the refusal does not name %s: %s", config.MetricsListen, err)
	}
}
