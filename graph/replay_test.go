package graph

import (
	"slices"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

const diamond = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
steps:
  fetch:
    image: ` + image + `
    outputs: [ok]
  side:
    image: ` + image + `
    outputs: [ok]
  normalize:
    image: ` + image + `
    needs: [{ step: fetch, port: ok, as: orders }]
    outputs: [ok]
  invoice:
    image: ` + image + `
    needs: [{ step: normalize, port: ok, as: orders }]
    outputs: [ok]
`

// What a replay from a step reuses is what it reads, through its edges and theirs, and nothing
// beside it: side is not above invoice, so it runs again.
func TestAReplayReusesTheStepsAStepReadsFrom(t *testing.T) {
	g := built(t, diamond, evaluatorManifest)
	if got := g.Upstream("invoice"); !slices.Equal(got, []agk.Step{"fetch", "normalize"}) {
		t.Errorf("above invoice are %v", got)
	}
	if got := g.Upstream("fetch"); len(got) != 0 {
		t.Errorf("above fetch are %v", got)
	}

	published := agk.Empty("01HZXOLD", "normalize", "ok", 1, runAt)
	published.Items = []agk.Item{{ID: "INV-1", Data: map[string]any{"n": 1}, Files: []agk.File{}}}
	published.Meta.Count = 1
	reused := map[agk.Step]StepState{
		"fetch":     Reused(StepState{Verdict: agk.VerdictSucceeded, Since: runAt, Ports: map[agk.Port]agk.Envelope{"ok": agk.Empty("01HZXOLD", "fetch", "ok", 1, runAt)}}, "01HZXRUN"),
		"normalize": Reused(StepState{Verdict: agk.VerdictSucceeded, Since: runAt, Shards: []ShardState{{Attempt: 1, Task: agk.TaskSucceeded}}, Ports: map[agk.Port]agk.Envelope{"ok": published}}, "01HZXRUN"),
	}
	if reused["normalize"].Shards != nil || reused["normalize"].Ports["ok"].Meta.RunID != "01HZXRUN" || reused["normalize"].Ports["ok"].Items[0].ID != "INV-1" {
		t.Fatalf("a reused step is %+v", reused["normalize"])
	}
	e := started(t, diamond, Options{Reuse: reused})
	plan := next(t, e, runAt)
	var handed []agk.Step
	for _, task := range plan.Start {
		handed = append(handed, task.Step)
		if task.Step == "invoice" && task.Inputs["orders"].Items[0].ID != "INV-1" {
			t.Errorf("invoice is handed %+v", task.Inputs["orders"])
		}
	}
	slices.Sort(handed)
	if !slices.Equal(handed, []agk.Step{"invoice", "side"}) {
		t.Errorf("the replay hands out %v", handed)
	}

	for name, reuse := range map[string]map[agk.Step]StepState{
		"a step the graph lacks": {"nothing": {Verdict: agk.VerdictSucceeded}},
		"a step not over":        {"fetch": {Verdict: agk.VerdictRunning}},
	} {
		if _, err := Start(built(t, diamond, evaluatorManifest), agk.Run{ID: "01HZXRUN", Namespace: "finance"}, Options{Reuse: reuse}, runAt); err == nil {
			t.Errorf("a replay reusing %s started", name)
		}
	}
}
