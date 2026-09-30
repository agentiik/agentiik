package graph

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// A version stored before v0.5.0 was held to none of its rules about triggers: a sync webhook
// naming no output, a schedule stepping a value alone, a minute of 60, an event trigger naming
// nothing it listens for. It keeps being read back
// and built, since its runs and replays have to go on after the upgrade, and it is Armable's to
// say that its triggers cannot be armed.
func TestAVersionStoredBeforeTheTriggerRulesRebuildsAndIsNotArmable(t *testing.T) {
	const stored = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: price-quotes, namespace: sales }
on:
  schedule:
    - cron: "5/15 60 * * *"
      timezone: "+02:00"
  webhook:
    - path: /quotes/../admin
      response: sync
  event:
    - {}
steps:
  quote:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: [quote]
    outputs: [out]
`
	if _, err := Parse([]byte(stored)); err == nil {
		t.Fatal("a version made today was read without complaint, and it breaks five rules of v0.5.0")
	}
	wf, err := LoadStored(fstest.MapFS{"agentiik.yaml": {Data: []byte(stored)}}, "agentiik.yaml", nil)
	if err != nil {
		t.Fatalf("a version stored before v0.5.0 is no longer read back: %v", err)
	}
	if err := Check(wf); err != nil {
		t.Fatalf("a version stored before v0.5.0 is no longer built: %v", err)
	}
	err = wf.Armable()
	if err == nil || !strings.Contains(err.Error(), "5/15") {
		t.Errorf("its triggers are armable, or refused for %v", err)
	}
}

// A webhook's map is evaluated over the request that fired the run: an expression filling the
// whole value keeps its type, one embedded in text is text, a map or a list is evaluated through,
// and a literal is what the file wrote. The workflow's vars are read as a step reads them; the run's
// id is not yet, and reading it is refused.
func TestAMapFillsTheInputsFromTheRequest(t *testing.T) {
	wf, err := Parse([]byte(`
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: price-quotes, namespace: sales }
vars:
  region: eu
inputs:
  orders: {}
  label: {}
  batch: {}
  fixed: {}
on:
  webhook:
    - path: /quotes
      map:
        orders: ${{ trigger.body.orders }}
        label: "${{ vars.region }}-${{ trigger.query.cycle }}"
        batch: { size: "${{ size(trigger.body.orders) }}", by: ["${{ run.triggered_by }}"] }
        fixed: 7
steps:
  quote:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: [quote]
    outputs: [out]
`))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(wf, nil)
	if err != nil {
		t.Fatal(err)
	}
	fired := Fired{
		Commit:      strings.Repeat("a", 40),
		Trigger:     map[string]any{"body": map[string]any{"orders": []any{"A-1", "A-2"}}, "query": map[string]any{"cycle": "2026-09"}, "headers": map[string]any{}},
		TriggerKind: "webhook", TriggeredBy: "sales/agentiik",
	}
	got, err := g.Fill(wf.On.Webhook[0].Map, fired)
	if err != nil {
		t.Fatal(err)
	}
	want := `map[batch:map[by:[sales/agentiik] size:2] fixed:7 label:eu-2026-09 orders:[A-1 A-2]]`
	if s := fmt.Sprint(got); s != want {
		t.Errorf("the map filled %s, want %s", s, want)
	}
	if _, err := g.Fill(map[string]any{"orders": "${{ run.id }}"}, fired); err == nil {
		t.Error("a map read the id of a run that does not exist yet")
	}
}

// "The event root in event triggers only": a webhook's map reading it is refused where the file is
// read, as any root its position does not expose is, and an event trigger's filter reads it.
func TestAWebhooksMapReadsNoEvent(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: price-quotes, namespace: sales }
inputs:
  orders: {}
on:
  %s
steps:
  quote:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: [quote]
    outputs: [out]
`
	checked := func(on string) error {
		wf, err := Parse([]byte(fmt.Sprintf(doc, on)))
		if err != nil {
			return err
		}
		return Check(wf)
	}
	err := checked(`webhook:
    - path: /quotes
      map:
        orders: ${{ event.data.orders }}`)
	var r *Refusal
	if !errors.As(err, &r) || r.Rule != RuleExpressionUnknownRoot || !strings.Contains(r.Error(), "a webhook's map") {
		t.Errorf("a webhook's map reading the event was read with %v", err)
	}
	if err := checked(`event:
    - type: com.example.order.approved
      filter: ${{ event.data.total > 100 }}`); err != nil {
		t.Errorf("an event trigger's filter reading the event was refused: %v", err)
	}
}

// An event trigger names a type, a source or a filter: one naming none would start a run on every
// event its namespace can see, which the schema refuses by its shape and so does a version made
// today. Any one of the three is a subscription.
func TestAnEventTriggerListeningForNothingIsRefused(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: order-fulfilment, namespace: sales }
on:
  event:
    - %s
steps:
  ship:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: [ship]
    outputs: [out]
`
	_, err := Parse([]byte(fmt.Sprintf(doc, "{}")))
	if err == nil || !strings.Contains(err.Error(), "on.event[0] names no type, no source and no filter") {
		t.Errorf("an event trigger listening for nothing was read, or refused for %v", err)
	}
	for _, entry := range []string{"{ type: com.example.order.approved }", "{ source: /erp/orders }", `{ filter: "${{ event.data.amount > 0 }}" }`} {
		if _, err := Parse([]byte(fmt.Sprintf(doc, entry))); err != nil {
			t.Errorf("the event trigger %s was refused: %v", entry, err)
		}
	}
}
