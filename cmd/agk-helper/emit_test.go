package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestEmitReadsTheThreeShapesAScriptProduces covers the array, the JSON Lines stream and
// the single object, which are the three things a command in a script actually writes.
func TestEmitReadsTheThreeShapesAScriptProduces(t *testing.T) {
	for _, c := range []struct {
		what    string
		payload string
		items   int
	}{
		{"a JSON array", `[{"status":200},{"status":404}]`, 2},
		{"JSON Lines", "{\"status\":200}\n{\"status\":404}\n", 2},
		{"one object", `{"status":200}`, 1},
		{"an empty array", `[]`, 0},
		{"an empty payload", "", 0},
		{"an array on several lines", "[\n  {\"status\":200},\n  {\"status\":404}\n]\n", 2},
	} {
		t.Run(c.what, func(t *testing.T) {
			h := newHarness(t)
			h.ok("emit", "out", "--from", h.write("result.json", c.payload))

			envelope := h.port("out")
			if len(envelope.Items) != c.items {
				t.Fatalf("%s became %d items, want %d", c.what, len(envelope.Items), c.items)
			}
			if envelope.Meta.Count != c.items {
				t.Errorf("meta.count says %d and the envelope holds %d items", envelope.Meta.Count, c.items)
			}
		})
	}
}

// TestEmitStampsTheMetadataTheContainerWasGiven is what keeps the collection from refusing
// what this wrote: run_id, step and attempt are the values the container reads, and the
// port is the file the envelope was written in.
func TestEmitStampsTheMetadataTheContainerWasGiven(t *testing.T) {
	h := newHarness(t)
	h.ok("emit", "error", "--from", h.write("r.json", `{"message":"refused"}`))

	meta := h.port("error").Meta
	if string(meta.RunID) != fixtureRun {
		t.Errorf("meta.run_id is %q, and %s carries %q", meta.RunID, EnvRunID, fixtureRun)
	}
	if string(meta.Step) != fixtureStep {
		t.Errorf("meta.step is %q, and %s carries %q", meta.Step, EnvStep, fixtureStep)
	}
	if meta.Port != "error" {
		t.Errorf("meta.port is %q, and the envelope was written in the file of port error", meta.Port)
	}
	if meta.Attempt != 1 {
		t.Errorf("meta.attempt is %d, and %s carries 1", meta.Attempt, EnvAttempt)
	}
	if meta.ProducedAt.IsZero() {
		t.Error("meta.produced_at is not set, and an envelope says when it was published")
	}
}

// TestAScalarIsRefusedBecauseDataIsAnObject holds the one shape that cannot be read: there
// is no key to put 42 under that a workflow expression could have known to read.
func TestAScalarIsRefusedBecauseDataIsAnObject(t *testing.T) {
	for _, payload := range []string{`42`, `"FR40000"`, `true`, `null`, `[1,2]`, `[[{"a":1}]]`} {
		h := newHarness(t)
		message := h.refused("emit", "out", "--from", h.write("r.json", payload))
		says(t, message, "an item's data is an object, never a scalar and never an array")
	}
}

// TestAPayloadThatIsNotJSONNamesWhatWasRead keeps a curl that returned an HTML error page
// from reading as a refusal about something else.
func TestAPayloadThatIsNotJSONNamesWhatWasRead(t *testing.T) {
	h := newHarness(t)
	says(t, h.refused("emit", "out", "--from", h.write("r.json", "<html>502</html>")), "not JSON")
}

// TestEmitReadsStandardInputByDefault is the ordinary pipe, and the one accident it closes:
// the contract puts the input envelope on the container's standard input, so a bare agk
// emit with nothing piped in would otherwise publish one item whose data is that envelope.
func TestEmitReadsStandardInputByDefault(t *testing.T) {
	h := newHarness(t)
	h.stdin(`{"status":200}`)
	h.ok("emit", "out")
	if n := len(h.port("out").Items); n != 1 {
		t.Errorf("standard input became %d items", n)
	}

	h = newHarness(t)
	var b []byte
	envelope := batch("ok", 2)
	b, _ = envelope.MarshalJSON()
	h.stdin(string(b))
	says(t, h.refused("emit", "out"), "standard input carries an envelope", "--from", "agk items")
}

// TestASecondEmitAppends is the loop case. A script that emits what it found per page would
// otherwise keep only the last page, in silence.
func TestASecondEmitAppends(t *testing.T) {
	h := newHarness(t)
	h.ok("emit", "out", "--from", h.write("a.json", `[{"page":1},{"page":2}]`))
	h.ok("emit", "out", "--from", h.write("b.json", `{"page":3}`))

	envelope := h.port("out")
	if len(envelope.Items) != 3 {
		t.Fatalf("two emits left %d items, and they carried 3", len(envelope.Items))
	}
	if envelope.Meta.Count != 3 {
		t.Errorf("meta.count says %d after the second emit", envelope.Meta.Count)
	}
	for i, want := range []string{"1", "2", "3"} {
		if got := fmt.Sprint(envelope.Items[i].Data["page"]); got != want {
			t.Errorf("item %d carries page %v, want %s: the order of the items is part of the contract", i+1, got, want)
		}
	}
}

// TestTheFilterPartitionsOnePayloadIntoTwoPorts is the documented example, which is the
// whole reason --filter exists: one result file, the valid on out and the rest on error.
func TestTheFilterPartitionsOnePayloadIntoTwoPorts(t *testing.T) {
	h := newHarness(t)
	result := h.write("result.json", `[{"vat":"FR1","valid":true},{"vat":"FR2","valid":false},{"vat":"FR3","valid":true}]`)

	h.ok("emit", "out", "--from", result, "--filter", ".valid")
	h.ok("emit", "error", "--from", result, "--filter", ".valid | not")

	out, bad := h.port("out"), h.port("error")
	if len(out.Items) != 2 {
		t.Errorf("out carries %d items, and 2 of the 3 are valid", len(out.Items))
	}
	if len(bad.Items) != 1 {
		t.Errorf("error carries %d items, and 1 of the 3 is not valid", len(bad.Items))
	}
	// Every item of the payload landed on exactly one of the two ports, which is what
	// the pair of filters is for.
	if len(out.Items)+len(bad.Items) != 3 {
		t.Error("the two filters do not partition the payload")
	}
}

// TestTheFilterIsTheTwoFormsAndNothingElse holds --filter to what it supports, and to
// saying so rather than pretending.
func TestTheFilterIsTheTwoFormsAndNothingElse(t *testing.T) {
	h := newHarness(t)
	payload := h.write("r.json", `[{"a":{"b":true}}]`)

	h.ok("emit", "out", "--from", payload, "--filter", ".a.b")
	if n := len(h.port("out").Items); n != 1 {
		t.Errorf("a field path of two segments kept %d of 1 item", n)
	}

	for _, expr := range []string{
		"valid",
		".items | length",
		".items[0].name",
		`.["valid"]`,
		".valid | not | not",
		".a..b",
		".valid and .checked",
		"",
	} {
		if expr == "" {
			continue
		}
		h := newHarness(t)
		message := h.refused("emit", "out", "--from", payload, "--filter", expr)
		says(t, message, "--filter takes a field path", "jq")
		if strings.Contains(strings.ToLower(message), "jq program") {
			t.Errorf("the refusal of %q calls the filter a jq program: %s", expr, message)
		}
	}
}

// TestAnAbsentFieldIsFalseAndSoIsNull is the truthiness rule, written down where it can be
// argued with: a field missing from half the responses has to land on the error port rather
// than on neither.
func TestAnAbsentFieldIsFalseAndSoIsNull(t *testing.T) {
	h := newHarness(t)
	payload := h.write("r.json", `[{"valid":true},{"valid":false},{"valid":null},{},{"valid":0},{"valid":""},{"valid":[]}]`)

	h.ok("emit", "out", "--from", payload, "--filter", ".valid")
	h.ok("emit", "error", "--from", payload, "--filter", ".valid | not")

	// true, 0, "" and [] are values the field carries; false, null and absent are not.
	if n := len(h.port("out").Items); n != 4 {
		t.Errorf("the filter kept %d items, and 4 of the 7 carry a value", n)
	}
	if n := len(h.port("error").Items); n != 3 {
		t.Errorf("the negated filter kept %d items, and 3 of the 7 are false, null or absent", n)
	}
}

// TestTheIdentityIsDerivedFromAFieldOrMinted is the reproducibility rule: a ULID carries
// the moment it was minted at, so a script that wants two identical runs to produce two
// identical envelopes names a field.
func TestTheIdentityIsDerivedFromAFieldOrMinted(t *testing.T) {
	payload := `[{"invoice":"INV-1042"},{"invoice":"INV-1043"}]`

	first := newHarness(t)
	first.ok("emit", "out", "--from", first.write("r.json", payload), "--id", "invoice")
	second := newHarness(t)
	second.ok("emit", "out", "--from", second.write("r.json", payload), "--id", "invoice")

	for i, want := range []string{"INV-1042", "INV-1043"} {
		if got := first.port("out").Items[i].ID; got != want {
			t.Errorf("item %d carries the identifier %q, and the field it is derived from carries %q", i+1, got, want)
		}
	}
	if a, b := first.port("out"), second.port("out"); a.Items[0].ID != b.Items[0].ID {
		t.Error("two runs over one payload derived two identities from one field")
	}

	// Without the flag the identity is minted, which is correct and is also what makes
	// a rerun differ.
	third := newHarness(t)
	third.ok("emit", "out", "--from", third.write("r.json", payload))
	if third.port("out").Items[0].ID == first.port("out").Items[0].ID {
		t.Error("an item with no --id carries the field's value as its identity")
	}
	if third.port("out").Items[0].ID == "" {
		t.Error("an item with no --id carries no identity")
	}
}

// TestTwoItemsCannotShareAnIdentity is the refusal that catches --id pointed at a field
// that does not identify anything: an identity is what a fan-out shards on and what a zip
// pairs by, so two items carrying one would be two things the graph treats as one.
func TestTwoItemsCannotShareAnIdentity(t *testing.T) {
	h := newHarness(t)
	message := h.refused("emit", "out", "--from", h.write("r.json", `[{"status":"ok"},{"status":"ok"}]`), "--id", "status")
	says(t, message, "ok", "status", "fan-out")

	// Across two emits too, because the second appends and the contradiction is in the
	// document that travels.
	h = newHarness(t)
	h.ok("emit", "out", "--from", h.write("a.json", `{"invoice":"INV-1"}`), "--id", "invoice")
	says(t, h.refused("emit", "out", "--from", h.write("b.json", `{"invoice":"INV-1"}`), "--id", "invoice"), "INV-1")
	// And the refusal leaves what was already there, rather than half of the second
	// batch.
	if n := len(h.port("out").Items); n != 1 {
		t.Errorf("the port carries %d items after a refused emit, and it carried 1 before it", n)
	}
}

// TestAnIdentityIsDerivedFromAScalarThatIsThere names the three ways --id has nothing to
// derive from.
func TestAnIdentityIsDerivedFromAScalarThatIsThere(t *testing.T) {
	for _, c := range []struct {
		payload string
		names   []string
	}{
		{`{"invoice":"INV-1"}`, []string{"no field", "reference"}},
		{`{"reference":null}`, []string{"reference"}},
		{`{"reference":{"id":1}}`, []string{"reference", "scalar"}},
		{`{"reference":""}`, []string{"reference", "empty"}},
	} {
		h := newHarness(t)
		message := h.refused("emit", "out", "--from", h.write("r.json", c.payload), "--id", "reference")
		says(t, message, c.names...)
	}

	// A number and a boolean are written as they stand: the field was chosen because
	// it identifies the thing.
	h := newHarness(t)
	h.ok("emit", "out", "--from", h.write("r.json", `[{"reference":1042},{"reference":true}]`), "--id", "reference")
	for i, want := range []string{"1042", "true"} {
		if got := h.port("out").Items[i].ID; got != want {
			t.Errorf("the identity is %q, want %q", got, want)
		}
	}
}

// TestAPortTheStepDoesNotDeclareIsRefusedHere is the refusal arriving from the tool that
// could still have been told: the collection would refuse the file afterwards, and the
// batch would be lost with the step.
func TestAPortTheStepDoesNotDeclareIsRefusedHere(t *testing.T) {
	h := newHarness(t)
	message := h.refused("emit", "rejected", "--from", h.write("r.json", `{"a":1}`))
	says(t, message, "rejected", "out,error", EnvOutPorts)

	// And nothing was written, because a file under an undeclared port is what the
	// collection refuses.
	if _, err := readEnvelope(portPath(h.env, "rejected")); err == nil {
		t.Error("the envelope of an undeclared port was written")
	}
}

// TestAMissingVariableIsRefusedNamingTheVariable is the rule for the environment: the
// refusal comes from the tool that could still have been told, because an envelope whose
// meta disagrees with what the container was given is refused when it is collected.
func TestAMissingVariableIsRefusedNamingTheVariable(t *testing.T) {
	for _, name := range []string{EnvRunID, EnvStep, EnvAttempt, EnvOutPorts} {
		h := newHarness(t)
		delete(h.vars, name)
		says(t, h.refused("emit", "out", "--from", h.write("r.json", `{"a":1}`)), name)
	}

	for _, c := range []struct{ name, value string }{
		{EnvRunID, "a run/with/slashes"},
		{EnvStep, "not a step"},
		{EnvAttempt, "one"},
		{EnvAttempt, "0"},
		{EnvOutPorts, "out,not a port"},
	} {
		h := newHarness(t)
		h.vars[c.name] = c.value
		says(t, h.refused("emit", "out", "--from", h.write("r.json", `{"a":1}`)), c.name)
	}
}

// TestAnEnvelopeOnThePortFromAnotherAttemptIsRefused keeps an append from landing in
// somebody else's document, which the collection would then refuse whole.
func TestAnEnvelopeOnThePortFromAnotherAttemptIsRefused(t *testing.T) {
	h := newHarness(t)
	h.ok("emit", "out", "--from", h.write("a.json", `{"a":1}`))

	h.vars[EnvAttempt] = "2"
	says(t, h.refused("emit", "out", "--from", h.write("b.json", `{"a":2}`)), EnvAttempt, "attempt 2")
}

// TestEmitRefusesAPayloadLongerThanAnEnvelopeMayBe applies the size rule where the line
// that wrote the batch is, rather than at the end of the step.
func TestEmitRefusesAPayloadLongerThanAnEnvelopeMayBe(t *testing.T) {
	h := newHarness(t)
	big := `[{"blob":"` + strings.Repeat("x", int(agk.DefaultEnvelopeMaxBytes)) + `"}]`
	says(t, h.refused("emit", "out", "--from", h.write("r.json", big)), "envelope")
}

// TestAValueTooHeavyToTravelInlineIsRefusedByTheRule holds the other size rule, and names
// the value rather than the item.
func TestAValueTooHeavyToTravelInlineIsRefusedByTheRule(t *testing.T) {
	h := newHarness(t)
	payload := `{"report":"` + strings.Repeat("x", int(agk.DefaultInlineMaxBytes)+1) + `"}`
	message := h.refused("emit", "out", "--from", h.write("r.json", payload), "--id", "report")
	says(t, message, "report", "artifact")
}

// TestEmitNamesThePortAndTakesOneArgument keeps the argument before the flags, which is how
// the documented example writes it.
func TestEmitNamesThePortAndTakesOneArgument(t *testing.T) {
	h := newHarness(t)
	says(t, h.refused("emit"), "no port named", EnvOutPorts)
	says(t, h.refused("emit", "out", "--from", h.write("r.json", `{"a":1}`), "extra"), "extra")
	says(t, h.refused("emit", "not a port"), "not a port")
}

// TestTheArgumentIsTakenFromEitherSideOfTheFlags accepts what a script meant. The documented
// order writes the port first, and a script that wrote the flags first meant the same thing.
func TestTheArgumentIsTakenFromEitherSideOfTheFlags(t *testing.T) {
	h := newHarness(t)
	h.ok("emit", "--from", h.write("a.json", `{"a":1}`), "out")
	if n := len(h.port("out").Items); n != 1 {
		t.Errorf("the port written after the flags carries %d items", n)
	}

	// A second argument is refused rather than dropped, whichever side it is on.
	says(t, h.refused("emit", "out", "error", "--from", h.write("b.json", `{"a":1}`)), "error")
	says(t, h.refused("emit", "--from", h.write("b.json", `{"a":1}`), "out", "error"), "error")
}
