package local

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/numbertest"
)

// A number is the same kind in a local run as on a server, wherever it came from: vars, inputs,
// their defaults and a matrix. The inputs are the ones a server run holds, read as agk reads them,
// and the params of every task are what numbertest says the controller hands the same tasks.
func TestANumberIsTheSameKindAsInAServerRun(t *testing.T) {
	var mu sync.Mutex
	var handed []graph.Task
	f := newFake(func(task graph.Task) (graph.Result, error) {
		mu.Lock()
		handed = append(handed, task)
		mu.Unlock()
		return published(task), nil
	})
	g := built(t, numbertest.Workflow)

	d := json.NewDecoder(bytes.NewReader([]byte(numbertest.Stored)))
	d.UseNumber()
	var inputs map[string]any
	if err := d.Decode(&inputs); err != nil {
		t.Fatal(err)
	}
	out, err := session(t, f).Run(t.Context(), Request{
		Graph: g, Tree: t.TempDir(), Inputs: inputs, Vars: g.Workflow().Vars,
	})
	if err != nil {
		t.Fatalf("running the graph: %s", err)
	}
	if out.Run.State != agk.Succeeded {
		t.Fatalf("the run is %s, want succeeded, and a step whose params do not evaluate fails with 120: %v", out.Run.State, out.Failures)
	}
	if len(handed) != 6 {
		t.Errorf("%d tasks were handed out, want one for first, four for the shards of second and one for third", len(handed))
	}
	for _, task := range handed {
		if diff := numbertest.Differ(task.Step, task.Shard.Index, task.Params); diff != "" {
			t.Errorf("%s %s is handed params where %s", task.Step, task.Shard, diff)
		}
	}
}
