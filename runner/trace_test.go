package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// "Context propagated into the container environment for bricks that use it", from the message on
// the pool's queue to the container, through the real API's redemption: the container is told the
// run's trace and the span of the dispatch it runs for, the one the controller exports under the
// dispatch's task_id. Nothing on the wire carries it; the runner derives it from the run and the
// task_id the message already names, as the controller does.
func TestTheContainerIsToldTheTraceOfItsRunAndTheSpanOfItsDispatch(t *testing.T) {
	in := anInstallationServing(t)
	c := carrier(t, func(dockertest.Container) (int, error) { return 0, nil })
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Agent{
			Config: Config{
				API: in.url, Runner: in.runner, Pool: in.poolName, Concurrency: 1,
				WorkDir: c.root, Credential: in.credential, Labels: []string{"zone=dmz"},
			},
			Driver: c.carrier.Driver.(*driver.Docker), Client: client, Endings: c.carrier.Endings,
			Log: func(s string) { t.Log(s) },
		})
	}()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
	}()

	m := in.dispatch(t, "invoice")
	eventually(t, "the task's container", func() bool {
		for _, made := range c.daemon.Created() {
			if made.Labels[driver.LabelTask] == m.IdempotencyKey {
				return true
			}
		}
		return false
	})

	var told []string
	for _, made := range c.daemon.Created() {
		if made.Labels[driver.LabelTask] != m.IdempotencyKey {
			continue
		}
		for _, v := range made.Config.Env {
			if name, value, _ := strings.Cut(v, "="); name == driver.EnvTraceParent {
				told = append(told, value)
			}
		}
	}
	trace, _ := agk.RunID(m.RunID).Trace()
	want := "00-" + trace.String() + "-" + agk.TaskSpan(m.TaskID).String() + "-01"
	if len(told) != 1 || told[0] != want {
		t.Fatalf("the container was told %s=%q, want %q: the run's trace and the span of dispatch %s", driver.EnvTraceParent, told, want, m.TaskID)
	}
}
