package graph

import (
	"strings"
	"testing"
	"testing/fstest"
)

// A version stored before v0.5.0 was held to none of its rules about triggers: a sync webhook
// naming no output, a schedule stepping a value alone, a minute of 60. It keeps being read back
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
steps:
  quote:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    script: [quote]
    outputs: [out]
`
	if _, err := Parse([]byte(stored)); err == nil {
		t.Fatal("a version made today was read without complaint, and it breaks four rules of v0.5.0")
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
