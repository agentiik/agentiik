package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// What the program reads reaching what it runs: the environment read by config.ReadController,
// handed to the core of a term through options, as serve hands it.

// environment is a controller's environment that config.ReadController accepts, with extra laid
// over it. The addresses are never reached: the core is given a queue of the test's own and the
// test's database, and what is under test is what the settings become.
func environment(t *testing.T, extra map[string]string) config.Lookup {
	t.Helper()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := account.Seed()
	issuer, err := bus.NewIssuer(string(seed), "tls://nats.example.com:4222")
	if err != nil {
		t.Fatal(err)
	}
	c, err := issuer.ForControlPlane("agentiik-controller", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := jwt.FormatUserConfig(c.JWT, []byte(c.Seed))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "controller.creds")
	if err := os.WriteFile(file, creds, 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		config.DatabaseURL:        "postgres://agentiik@db.example.com:5432/agentiik?sslmode=verify-full",
		config.BusURL:             "tls://nats.example.com:4222",
		config.BusCredentialsFile: file,
		config.ObjectsDir:         t.TempDir(),
	}
	for k, v := range extra {
		env[k] = v
	}
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// heard is the bus as the core sees it, keeping what it was handed.
type heard struct {
	mu   sync.Mutex
	sent []controller.Dispatch
}

func (q *heard) Publish(_ context.Context, d controller.Dispatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = append(q.sent, d)
	return nil
}

func (q *heard) Stop(context.Context, graph.Stop) error { return nil }

// taken answers what was handed out since the last call.
func (q *heard) taken() []controller.Dispatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	sent := q.sent
	q.sent = nil
	return sent
}

// "set by: The installation, as AGK_MAX_REQUEUES ... Never the workflow file." A key of a step
// that may be retried after a loss is lost on each of its dispatches, by a runner that redeems it
// and goes quiet. Where AGK_MAX_REQUEUES is 1 it goes out again once, and its second loss stands:
// the step and the run fail, on the infrastructure's account and saying why, with the attempt
// retry.max left unspent. Where it is unset, the default of three sends it out a third time.
//
// AGK_TASK_CEILING reaches the same core, as the deadline of a task no timeout bounds, and
// AGK_OBJECTS_DIR is where the input envelope the controller writes lands.
func TestAKeyLostTwiceFailsItsStepWhereAGKMaxRequeuesIsOne(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      map[string]string
		fails    bool
		deadline time.Duration
	}{
		{"one", map[string]string{config.MaxRequeues: "1", config.TaskCeiling: "90m"}, true, 90 * time.Minute},
		{"unset", nil, false, config.DefaultTaskCeiling},
	} {
		t.Run(c.name, func(t *testing.T) {
			read, err := config.ReadController(environment(t, c.set))
			if err != nil {
				t.Fatal(err)
			}

			pool, super := dbtest.Open(t)
			seeded(t, pool, super)
			versions, err := version.New(pool, version.Options{})
			if err != nil {
				t.Fatal(err)
			}
			ctl, err := controller.New(pool, "requeueing")
			if err != nil {
				t.Fatal(err)
			}
			tm, err := pool.BeginTerm(t.Context(), "requeueing")
			if err != nil {
				t.Fatal(err)
			}
			q := &heard{}
			o := options(read, q, versions)
			// The clock alone is the test's, so that three heartbeat intervals pass
			// without anybody waiting them out.
			now := time.Date(2026, 9, 24, 6, 0, 0, 0, time.UTC)
			o.Now = func() time.Time { return now }
			core, err := controller.NewCore(ctl, tm, o)
			if err != nil {
				t.Fatal(err)
			}

			run := started(t, pool, goodCommit)
			if err := core.Decide(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			sent := q.taken()
			if len(sent) != 1 {
				t.Fatalf("the first pass handed out %d tasks", len(sent))
			}
			if got, want := sent[0].Task.Deadline, now.Add(c.deadline); !got.Equal(want) {
				t.Errorf("a task no timeout bounds has the deadline %s, and the ceiling puts it at %s", got, want)
			}
			if objects, _ := os.ReadDir(filepath.Join(read.Objects, "finance")); len(objects) == 0 {
				t.Errorf("nothing was written under %s, and the input envelope is written to AGK_OBJECTS_DIR", read.Objects)
			}

			// Each dispatch is redeemed by a runner of its own, which then goes quiet
			// for longer than the heartbeat allows.
			for n, runner := range []string{"runner-1", "runner-2"} {
				if len(sent) != 1 {
					t.Fatalf("after %d losses the controller handed out %d tasks", n, len(sent))
				}
				if err := ctl.Fenced(t.Context(), tm, func(ctx context.Context, w *db.Wide) error {
					_, err := w.Redeem(ctx, sent[0].Grant, sent[0].Task.ID, runner, now)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				now = now.Add(db.LostAfter + time.Second)
				if err := core.Wake(t.Context(), controller.Wake{Swept: true}); err != nil {
					t.Fatal(err)
				}
				sent = q.taken()
			}

			conn := dbtest.Superuser(t, super)
			var state, step string
			var evaluation []byte
			if err := conn.QueryRow(t.Context(),
				`select r.state, s.state, r.evaluation::text from runs r join steps s on s.run_id = r.id
				 where r.id = $1 and s.step = 'normalize'`, string(run)).Scan(&state, &step, &evaluation); err != nil {
				t.Fatal(err)
			}
			if !c.fails {
				if len(sent) != 1 || state == agk.Failed.String() {
					t.Errorf("under the default of three, a key lost twice was handed out %d more times, and the run is %s", len(sent), state)
				}
				return
			}
			if len(sent) != 0 {
				t.Errorf("under AGK_MAX_REQUEUES=1 a key lost twice went out again as %+v", sent)
			}
			if state != agk.Failed.String() || step != "failed" {
				t.Errorf("under AGK_MAX_REQUEUES=1, a key lost twice left its run %s and its step %s", state, step)
			}
			if !bytes.Contains(evaluation, []byte("max_requeues hands one key out again after a loss at most 1 times")) ||
				!bytes.Contains(evaluation, []byte("the infrastructure's account")) {
				t.Errorf("the run's evaluation does not say the step failed on the infrastructure's account past max_requeues: %s", evaluation)
			}
		})
	}
}

// A controller refused its configuration opens nothing and names every setting it was refused
// over on the one start, the master key's file among them, since that key is the API's alone.
func TestAStartTheConfigurationRefusesNamesEverySetting(t *testing.T) {
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) {
		if name == config.MasterKeyFile {
			return "/etc/agentiik/master.key", true
		}
		return "", false
	}
	if code := run(t.Context(), nil, lookup, &stdout, &stderr); code != exitFailed {
		t.Errorf("a start with nothing configured exited %d, want %d", code, exitFailed)
	}
	for _, variable := range []string{
		config.MasterKeyFile, config.DatabaseURL, config.BusURL, config.BusCredentialsFile, config.ObjectsDir,
	} {
		if !strings.Contains(stderr.String(), variable) {
			t.Errorf("the refusal does not name %s:\n%s", variable, stderr.String())
		}
	}
}

// Everything the program reads is in its environment, so an argument is a mistake it says it
// is, apart from the two every program answers.
func TestTheProgramTakesNoArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"serve"}, nil, &stdout, &stderr); code != exitUsage {
		t.Errorf("an argument exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "takes no argument") {
		t.Errorf("an argument was refused with %q", stderr.String())
	}

	stdout.Reset()
	if code := run(t.Context(), []string{"--version"}, nil, &stdout, &stderr); code != exitStopped || !strings.HasPrefix(stdout.String(), program+" ") {
		t.Errorf("--version exited %d printing %q", code, stdout.String())
	}
}
