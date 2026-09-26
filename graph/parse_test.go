package graph

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// minimal is the smallest workflow the language accepts, which every test below writes
// its own one line into.
const minimal = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: nightly-reconciliation
steps:
  reconcile:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [out]
`

// parsed reads a document that has to be readable, and fails the test where it is not.
func parsed(t *testing.T, doc string) *Workflow {
	t.Helper()
	wf, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("this document was refused: %v", err)
	}
	return wf
}

// refused reads a document that has to be refused, and returns what it was refused with,
// so that a test can say what the message has to name.
func refused(t *testing.T, doc string) error {
	t.Helper()
	if _, err := Parse([]byte(doc)); err != nil {
		return err
	}
	t.Fatal("this document was read without complaint")
	return nil
}

// TestOnIsAKeyAndNotTheBooleanTrue is the rule that decides which YAML the file is read
// with. "The trigger block is spelled on:, as the documentation spells it, and a YAML 1.1
// parser reads that bare key as the boolean true. A loader that does so will fail every
// fixture carrying a trigger, and will do the same to the workflows people write."
func TestOnIsAKeyAndNotTheBooleanTrue(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: monthly-invoicing
on:
  schedule:
    - cron: "0 6 1 * *"
      timezone: Europe/Paris
steps:
  reconcile:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [out]
`)
	if len(wf.On.Schedule) != 1 {
		t.Fatalf("the trigger block was read as %+v", wf.On)
	}
	if wf.On.Schedule[0].Cron != "0 6 1 * *" || wf.On.Schedule[0].Timezone != "Europe/Paris" {
		t.Fatalf("the schedule was read as %+v", wf.On.Schedule[0])
	}
}

// TestTheFileIsClosed holds the decision: "every block the language owns refuses a key it
// does not recognise ... timout: 10m is a rejected push, not a line that quietly does
// nothing".
func TestTheFileIsClosed(t *testing.T) {
	for what, doc := range map[string]string{
		"a key at the root":     strings.Replace(minimal, "steps:", "env:\n  TZ: Europe/Paris\nsteps:", 1),
		"a step keyword":        strings.Replace(minimal, "    outputs: [out]", "    parallelism: 4\n    outputs: [out]", 1),
		"a misspelled timeout":  strings.Replace(minimal, "    outputs: [out]", "    timout: 10m\n    outputs: [out]", 1),
		"a key under metadata":  strings.Replace(minimal, "  name: nightly-reconciliation", "  name: nightly-reconciliation\n  owner: finance", 1),
		"a key under a trigger": strings.Replace(minimal, "steps:", "on:\n  schedule:\n    - cron: \"0 6 1 * *\"\n      every: day\nsteps:", 1),
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
}

// TestDefaultsCarriesExecutionSettingsAndOnlyThose holds the cut the documentation draws:
// "anything deciding what a step runs, or where it sits in the graph, belongs to the
// step: the cut is what keeps the graph readable from the steps block alone".
func TestDefaultsCarriesExecutionSettingsAndOnlyThose(t *testing.T) {
	for _, keyword := range []string{"image", "script", "needs", "inputs", "outputs", "params", "if", "merge", "strategy", "extends", "workflow"} {
		doc := strings.Replace(minimal, "steps:", "defaults:\n  "+keyword+": x\nsteps:", 1)
		err := refused(t, doc)
		if !strings.Contains(err.Error(), keyword) {
			t.Errorf("defaults carrying %s was refused without naming it: %v", keyword, err)
		}
	}

	wf := parsed(t, strings.Replace(minimal, "steps:", "defaults:\n  timeout: 10m\n  network: egress\n  idempotent: false\nsteps:", 1))
	st := wf.Steps["reconcile"]
	if time.Duration(st.Timeout) != 10*time.Minute {
		t.Errorf("the step took %s from defaults", st.Timeout)
	}
	if st.Network != NetworkEgress {
		t.Errorf("the step took the network %s from defaults", st.Network)
	}
	if st.Idempotent {
		t.Error("the step was read as idempotent, and defaults says otherwise")
	}
}

// TestAStepIsIdempotentUntilItSaysOtherwise holds the default the language states:
// idempotent is "a boolean, true by default", and it decides whether a lost task is
// requeued and whether the step can be cached.
func TestAStepIsIdempotentUntilItSaysOtherwise(t *testing.T) {
	if !parsed(t, minimal).Steps["reconcile"].Idempotent {
		t.Fatal("a step that says nothing was read as not idempotent")
	}
}

// TestTheShortFormOfAnEdgeIsWrittenOut holds the table: "short form step-name equivalent
// to { step: name, port: out, as: in }".
func TestTheShortFormOfAnEdgeIsWrittenOut(t *testing.T) {
	wf := parsed(t, minimal+`
  archive:
    image: ghcr.io/acme/agk-archive@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    needs: [reconcile]
    outputs: [out]
`)
	edges := wf.Steps["archive"].Needs
	if len(edges) != 1 {
		t.Fatalf("the step needs %v", edges)
	}
	if edges[0] != (Edge{Step: "reconcile", Port: "out", As: "in"}) {
		t.Fatalf("the short form was read as %+v", edges[0])
	}
}

// TestScriptIsAListOfCommands holds the rule and the reason: "one command per entry, and
// a single multi-line block scalar is refused. The list is what makes that exit
// attributable to one command rather than to a script."
func TestScriptIsAListOfCommands(t *testing.T) {
	err := refused(t, strings.Replace(minimal, "    outputs: [out]", "    script: |\n      echo one\n      echo two\n    outputs: [out]", 1))
	if !strings.Contains(err.Error(), "one command per entry") {
		t.Fatalf("a block scalar was refused without saying why: %v", err)
	}

	wf := parsed(t, strings.Replace(minimal, "    outputs: [out]", "    script:\n      - echo one\n      - echo two\n    outputs: [out]", 1))
	if got := wf.Steps["reconcile"].Script; len(got) != 2 {
		t.Fatalf("the script was read as %v", got)
	}
}

// TestAScriptStepGetsTheShellTheLanguageNames holds the default beside the keyword:
// shell "defaults to ["/bin/sh", "-e"]", and a task carries what a driver needs rather
// than what it has to look up.
func TestAScriptStepGetsTheShellTheLanguageNames(t *testing.T) {
	wf := parsed(t, strings.Replace(minimal, "    outputs: [out]", "    script: [echo one]\n    outputs: [out]", 1))
	shell := wf.Steps["reconcile"].Shell
	if len(shell) != 2 || shell[0] != "/bin/sh" || shell[1] != "-e" {
		t.Fatalf("the shell was read as %v", shell)
	}
	if got := parsed(t, minimal).Steps["reconcile"].Shell; got != nil {
		t.Fatalf("a step that runs no command was given the shell %v", got)
	}
}

// TestAStepRunsAnImageOrCallsAWorkflow holds the sentence about the two keywords: "a step
// carries image or workflow and never both, and script, before_script, after_script and
// shell are refused beside it: a sub-workflow call runs no container of its own".
func TestAStepRunsAnImageOrCallsAWorkflow(t *testing.T) {
	refused(t, strings.Replace(minimal, "    outputs: [out]", "    workflow: finance/common@v2.1.0\n    outputs: [out]", 1))

	for _, keyword := range []string{"script: [echo one]", "before_script: [echo one]", "after_script: [echo one]", "shell: [/bin/sh]"} {
		doc := `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: dunning
steps:
  remind:
    workflow: finance/common@v2.1.0
    ` + keyword + `
`
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("a sub-workflow call carrying %s was accepted", keyword)
		}
	}

	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: dunning
steps:
  remind:
    workflow: finance/common@v2.1.0
  archive-cycle:
    workflow:
      workflow: finance/archive
      ref: a3f9c1e
    needs: [remind]
`)
	if call := wf.Steps["remind"].Call; call == nil || call.Workflow != "finance/common" || call.Ref != "v2.1.0" {
		t.Fatalf("the short form of a call was read as %+v", call)
	}
	if call := wf.Steps["archive-cycle"].Call; call == nil || call.Workflow != "finance/archive" || call.Ref != "a3f9c1e" {
		t.Fatalf("the long form of a call was read as %+v", call)
	}
}

// TestOnlyAWorkflowOutputCanBeOneShot holds the rule about fetches: "only a workflow
// output can be one-shot: an artifact travelling between two steps is read once per shard
// and again by a replay".
func TestOnlyAWorkflowOutputCanBeOneShot(t *testing.T) {
	refused(t, strings.Replace(minimal, "steps:", "defaults:\n  retain: { for: 7d, fetches: 1 }\nsteps:", 1))

	wf := parsed(t, strings.Replace(minimal, "steps:", "outputs:\n  payslips:\n    from: { step: reconcile, port: out }\n    retain: { for: 24h, fetches: 1 }\nsteps:", 1))
	retain := wf.Outputs["payslips"].Retain
	if time.Duration(retain.For) != 24*time.Hour || retain.Fetches != 1 {
		t.Fatalf("the retention was read as %+v", retain)
	}
}

// TestANumberReachesJSONAsANumber holds what a parameter is for: it is "validated against
// the manifest schema" by a JSON Schema validator and written to /agk/params.json, so a
// number the file wrote has to arrive as a number and not as whatever a YAML loader
// happens to answer.
func TestANumberReachesJSONAsANumber(t *testing.T) {
	wf := parsed(t, strings.Replace(minimal, "    outputs: [out]", "    params:\n      retries: 3\n      ratio: 0.5\n      label: \"1\"\n    outputs: [out]", 1))
	params := wf.Steps["reconcile"].Params
	doc, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"retries":3`, `"ratio":0.5`, `"label":"1"`} {
		if !strings.Contains(string(doc), want) {
			t.Fatalf("the parameters reached JSON as %s, and %s is not in it", doc, want)
		}
	}
}

// TestADuplicateKeyIsRefused is the other half of the file being closed: a key written
// twice is a file whose second line silently wins, and the reader refuses it.
func TestADuplicateKeyIsRefused(t *testing.T) {
	refused(t, minimal+`
  reconcile:
    image: ghcr.io/acme/agk-archive@sha256:44de908840a673cc25120ecd3e506292d691f72937ce1da04a6ee4252ff4c115
    outputs: [out]
`)
}

// TestTheEveryKeywordOfAStepIsRead reads the documentation's own step, keyword by
// keyword, so that a keyword quietly dropped by the reader is a failing test rather than
// a setting nobody applied.
func TestTheEveryKeywordOfAStepIsRead(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: reconciliation
  namespace: finance
secrets: [billing]
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d073f187ad520aaf67af255db9208210cfac76f1f2426ef8d938079b7e0
    outputs: [ok]
  invoice:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    needs:
      - { step: normalize, port: ok, as: in }
    inputs:
      reference: ${{ workflow.inputs.reference }}
    outputs: [out, error]
    params:
      currency: EUR
    if: ${{ inputs.in.count > 0 }}
    when: [succeeded, skipped]
    merge: { join: { on: "$.data.customer_id" } }
    strategy:
      fan_out: batch(50)
      max_parallel: 8
      fail_fast: true
    retry:
      max: 4
      on: [transient, lost]
      backoff: { type: exponential, base: 2s, max: 60s }
    timeout: 30m
    continue_on_error: true
    resources: { cpu: "0.5", memory: 256Mi, pids: 128 }
    network: egress
    egress:
      allow: ["api.billing.example.com:443"]
    runs_on: [zone=dmz, arch=arm64]
    secrets: [billing]
    cache: true
    idempotent: false
    files:
      - ./sql/**
      - { from: ./certs/ca.pem, to: /etc/ssl/certs/ca.pem, mode: "0444" }
`)
	st := wf.Steps["invoice"]
	switch {
	case st.Image == "":
		t.Error("image")
	case len(st.Needs) != 1 || st.Needs[0].As != "in":
		t.Error("needs")
	case len(st.Inputs) != 1:
		t.Error("inputs")
	case len(st.Outputs) != 2:
		t.Error("outputs")
	case st.Params["currency"] != "EUR":
		t.Error("params")
	case st.If == "":
		t.Error("if")
	case len(st.When) != 2 || st.When[1] != WhenSkipped:
		t.Error("when")
	case st.Merge != MergeJoin || st.Join.On != "$.data.customer_id":
		t.Error("merge")
	case st.Strategy.FanOut != FanOutBatch || st.Strategy.Batch != 50 || st.Strategy.MaxParallel != 8 || !st.Strategy.FailFast:
		t.Error("strategy")
	case st.Retry.Max != 4 || len(st.Retry.On) != 2 || time.Duration(st.Retry.Backoff.Base) != 2*time.Second:
		t.Error("retry")
	case time.Duration(st.Timeout) != 30*time.Minute:
		t.Error("timeout")
	case !st.ContinueOnError:
		t.Error("continue_on_error")
	case st.Resources.CPU != "0.5" || st.Resources.Memory != "256Mi" || st.Resources.PIDs != 128:
		t.Error("resources")
	case st.Network != NetworkEgress:
		t.Error("network")
	case len(st.EgressAllow) != 1:
		t.Error("egress")
	case len(st.RunsOn) != 2:
		t.Error("runs_on")
	case len(st.Secrets) != 1:
		t.Error("secrets")
	case !st.Cache:
		t.Error("cache")
	case st.Idempotent:
		t.Error("idempotent")
	case len(st.Files) != 2 || st.Files[1].To != "/etc/ssl/certs/ca.pem" || st.Files[1].Mode != "0444":
		t.Error("files")
	}
}

// TestAnEmptyToolListIsNotNoBlockAtAll holds the distinction the documentation draws:
// "an empty list publishes a server with nothing on it, which is not the same as
// declaring no mcp block at all: the first serves an empty tool list, the second serves
// nothing and returns 404".
func TestAnEmptyToolListIsNotNoBlockAtAll(t *testing.T) {
	if parsed(t, minimal).MCP != nil {
		t.Fatal("a workflow declaring no mcp block was read as publishing one")
	}
	wf := parsed(t, strings.Replace(minimal, "steps:", "mcp:\n  name: nothing\n  tools: []\nsteps:", 1))
	if wf.MCP == nil {
		t.Fatal("a workflow declaring an mcp block was read as publishing nothing")
	}
	if wf.MCP.Tools == nil || len(wf.MCP.Tools) != 0 {
		t.Fatalf("the empty tool list was read as %v", wf.MCP.Tools)
	}
}

// TestAWorkflowNamesItsSecretsAndNothingMore holds the line the namespace draws: the file
// names the secrets it uses, and where each value lives, its provider and its path, is
// declared on the namespace by a principal holding secret:write. A path in the file would let
// anybody able to push the workflow aim it at whatever the store holds.
func TestAWorkflowNamesItsSecretsAndNothingMore(t *testing.T) {
	wf := parsed(t, strings.Replace(minimal, "steps:", "secrets: [billing, ledger]\nsteps:", 1))
	if len(wf.Secrets) != 2 || wf.Secrets[0] != "billing" || wf.Secrets[1] != "ledger" {
		t.Fatalf("the secrets were read as %v", wf.Secrets)
	}

	err := refused(t, strings.Replace(minimal, "steps:", "secrets:\n  billing:\n    provider: vault\n    path: kv/data/agentiik/billing\nsteps:", 1))
	for _, want := range []string{"list of names", "declared on the namespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a secret written with its provider and path was refused without saying %q: %v", want, err)
		}
	}
	for what, block := range map[string]string{
		"an empty list":        "secrets: []",
		"a name written twice": "secrets: [billing, billing]",
		"a path for a name":    "secrets: [kv/data/agentiik/billing]",
	} {
		if _, err := Parse([]byte(strings.Replace(minimal, "steps:", block+"\nsteps:", 1))); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
}

// TestCPUIsWrittenAsAString holds the sentence about the quotation marks: they are "load
// bearing rather than house style: YAML reads an unquoted 1 as a number, and a number is
// refused".
func TestCPUIsWrittenAsAString(t *testing.T) {
	refused(t, strings.Replace(minimal, "    outputs: [out]", "    resources: { cpu: 1 }\n    outputs: [out]", 1))
	refused(t, strings.Replace(minimal, "    outputs: [out]", "    resources: { memory: 512M }\n    outputs: [out]", 1))
	parsed(t, strings.Replace(minimal, "    outputs: [out]", "    resources: { cpu: \"1\", memory: 512Mi }\n    outputs: [out]", 1))
}

// TestRetainIsNotAStepKeyword holds the one execution setting a step may not write for
// itself. defaults carries it, "and any output not saying otherwise" takes it from there,
// and a workflow output writes its own; the step table does not carry it at all.
func TestRetainIsNotAStepKeyword(t *testing.T) {
	refused(t, strings.Replace(minimal, "    outputs: [out]", "    retain: 7d\n    outputs: [out]", 1))

	wf := parsed(t, strings.Replace(minimal, "steps:", "defaults:\n  retain: 7d\nsteps:", 1))
	if time.Duration(wf.Steps["reconcile"].Retain.For) != 7*24*time.Hour {
		t.Fatalf("the retention of defaults reached the step as %s", wf.Steps["reconcile"].Retain.For)
	}
}

// TestTheDocumentKeepsWhereAValueWasWritten holds the seam a refusal about an expression
// needs: the file is kept beside what was read out of it, so that a rule about an
// expression can point at the line and column the author wrote it on rather than at a key
// somewhere in the file.
func TestTheDocumentKeepsWhereAValueWasWritten(t *testing.T) {
	wf := parsed(t, `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: monthly-invoicing
steps:
  invoice:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc
    if: ${{ inputs.in.count > 0 }}
    outputs: [out]
`)
	line, column := position(wf.doc, "$.steps.invoice.if")
	if line != 9 {
		t.Fatalf("the condition was written on line 9 and is reported on line %d", line)
	}
	if column == 0 {
		t.Fatal("the condition is reported at no column")
	}
	if line, _ := position(wf.doc, "$.steps.invoice.nowhere"); line != 0 {
		t.Fatal("a key the file never wrote was given a position")
	}
}

// A number keeps the way it was written, which is what an expression reads: one written without
// a fraction or an exponent is an int and any other a double, so 2.0 among the vars stays 2.0
// rather than the 2 a float64 would print. A count written 3.0 is still the whole number the
// schema's integer takes it for.
func TestANumberKeepsTheWayItWasWritten(t *testing.T) {
	wf := parsed(t, strings.Replace(minimal, "steps:\n", "vars: { n: 3, x: 2.0, big: 1.0e21 }\nsteps:\n", 1)+"    retry: { max: 3.0 }\n")
	for name, want := range map[string]json.Number{"n": "3", "x": "2.0", "big": "1000000000000000000000.0"} {
		if got := wf.Vars[name]; got != want {
			t.Errorf("vars.%s is read as %#v, want %#v", name, got, want)
		}
	}
	if got := wf.Steps["reconcile"].Retry.Max; got != 3 {
		t.Errorf("retry.max: 3.0 is read as %d, want 3", got)
	}
	if err := refused(t, strings.Replace(minimal, "    outputs: [out]\n", "    outputs: [out]\n    retry: { max: 2.5 }\n", 1)); !strings.Contains(err.Error(), "retry.max is 2.5, and it is a whole number") {
		t.Errorf("retry.max: 2.5 is refused with %q", err)
	}
}
