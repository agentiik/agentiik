package api_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A run's steps carry what the version it pinned declares of them, the image each runs by digest
// and the ports it reads and publishes, and its tasks the parameters they were dispatched with,
// which a principal without run:read_data is not answered, as a run's inputs are not.
func TestARunSaysWhatItsStepsDeclareAndWhatItsTasksWereGiven(t *testing.T) {
	o := withOneRun(t)
	conn := dbtest.Superuser(t, o.super)
	const row = "01M2T9AAAAAAAAAAAAAAAAAAAA"
	if _, err := conn.Exec(t.Context(), `insert into steps (namespace, run_id, step) values ('finance', $1, 'normalize'), ('finance', $1, 'archive') on conflict do nothing`, o.run); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `insert into tasks (namespace, id, run_id, step, attempt, state) values ('finance', $1, $2, 'normalize', 1, 'dispatched')`, row, o.run); err != nil {
		t.Fatal(err)
	}
	if err := o.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		_, err := w.IssueGrant(ctx, "finance", agk.NewTaskID(agk.RunID(o.run), "normalize", 1, agk.Shard{}), row,
			db.GrantScope{Run: agk.RunID(o.run), Step: "normalize", Params: map[string]any{"currency": "EUR", "key": map[string]any{"secret": "vat-api"}}},
			time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	read := func(h http.Handler, who string) map[string]any {
		t.Helper()
		w, detail := call(t, h, "GET", "/api/v1/runs/"+o.run, who, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("the run answered %d: %s", w.Code, w.Body)
		}
		return detail
	}
	detail := read(o.servedTo(t, everything{who: "admin"}), "admin")
	steps := map[string]map[string]any{}
	for _, s := range detail["steps"].([]any) {
		step := s.(map[string]any)
		steps[step["step"].(string)] = step
	}
	for name, want := range map[string]struct{ in, out []any }{
		"normalize": {[]any{"orders"}, []any{"ok", "rejected"}},
		"archive":   {[]any{"orders"}, []any{"ok"}},
	} {
		s := steps[name]
		if s == nil || s["image"] != image || !reflect.DeepEqual(s["input_ports"], want.in) || !reflect.DeepEqual(s["output_ports"], want.out) {
			t.Errorf("step %s reads %v", name, s)
		}
	}
	tasks := detail["tasks"].([]any)
	if len(tasks) != 1 || !reflect.DeepEqual(tasks[0].(map[string]any)["params"], map[string]any{"currency": "EUR", "key": map[string]any{"secret": "vat-api"}}) {
		t.Errorf("the tasks read %v", tasks)
	}

	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	detail = read(o.servedTo(t, granted{"bruno": {{api.RunRead, invoicing}}}), "bruno")
	if params, there := detail["tasks"].([]any)[0].(map[string]any)["params"]; there {
		t.Errorf("a principal without run:read_data is answered the parameters %v", params)
	}
	if detail["steps"].([]any)[0].(map[string]any)["image"] != image {
		t.Errorf("a principal holding run:read is not answered the image a step runs: %v", detail["steps"])
	}
}
