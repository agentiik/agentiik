package api

import (
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	"github.com/agentiik/agentiik/internal/fixtures"
)

// The grammars a join is checked against are the ones the wire writes for
// $defs/runnerRegistration/request, pattern for pattern, for the reason a pool's are: a looser copy
// would take in a machine the wire refuses, and a narrower one would refuse a machine the
// documentation prints.
func TestTheJoinGrammarsAreTheWiresOwn(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	type pattern struct {
		Pattern    string             `json:"pattern"`
		Items      *pattern           `json:"items"`
		Ref        string             `json:"$ref"`
		Properties map[string]pattern `json:"properties"`
	}
	var schema struct {
		Defs map[string]pattern `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	request := schema.Defs["runnerRegistration"].Properties["request"].Properties
	capacity := request["capacity"].Properties
	containment := request["containment"].Properties

	// The namespaces and labels a join names are the wire's shared definitions, which the pool
	// test already holds these to; what is checked here is that the join refers to them.
	if ref := request["namespaces"].Items; ref == nil || ref.Ref != "#/$defs/namespace" {
		t.Errorf("a join's namespaces are no longer the wire's namespace, so givenName may not be their grammar")
	}
	if ref := request["labels"].Items; ref == nil || ref.Ref != "#/$defs/label" {
		t.Errorf("a join's labels are no longer the wire's label, so labelForm may not be their grammar")
	}

	for _, c := range []struct {
		what string
		ours *regexp.Regexp
		wire string
	}{
		{"a host's public key", publicKeyForm, request["public_key"].Pattern},
		{"a host's architecture", architectureForm, request["architecture"].Pattern},
		{"an agent's version", versionForm, request["agent_version"].Pattern},
		{"a host's memory", memoryForm, capacity["memory"].Pattern},
		{"a host's disk", memoryForm, capacity["disk"].Pattern},
		{"a container runtime", runtimeForm, containment["runtime"].Pattern},
	} {
		if c.wire == "" {
			t.Errorf("the wire writes no pattern for %s where this test looks for one", c.what)
			continue
		}
		if c.ours.String() != c.wire {
			t.Errorf("%s is checked against %s, and the wire writes %s", c.what, c.ours, c.wire)
		}
	}
}

// The grammars a heartbeat is checked against are the wire's own too, for the same reason, and so
// is the list of states a runner may report.
func TestTheHeartbeatGrammarsAreTheWiresOwn(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	type property struct {
		Pattern string    `json:"pattern"`
		Enum    []string  `json:"enum"`
		Ref     string    `json:"$ref"`
		Items   *property `json:"items"`
	}
	var schema struct {
		Defs struct {
			IdempotencyKey  property `json:"idempotencyKey"`
			RunnerHeartbeat struct {
				Properties struct {
					Request struct {
						Properties map[string]property `json:"properties"`
					} `json:"request"`
				} `json:"properties"`
			} `json:"runnerHeartbeat"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	request := schema.Defs.RunnerHeartbeat.Properties.Request.Properties

	if ref := request["tasks"].Items; ref == nil || ref.Ref != "#/$defs/idempotencyKey" {
		t.Errorf("a heartbeat's tasks are no longer the wire's idempotency key, so keyForm may not be their grammar")
	}
	if ref := request["sent_at"].Ref; ref != "#/$defs/timestamp" {
		t.Errorf("a heartbeat's sent_at is no longer the wire's timestamp, so instantForm may not be its grammar")
	}
	if !slices.Equal(request["state"].Enum, runnerStates) {
		t.Errorf("a runner reports itself %v, and the wire lists %v", runnerStates, request["state"].Enum)
	}
	for _, c := range []struct {
		what string
		ours *regexp.Regexp
		wire string
	}{
		{"a heartbeat's runner", runnerForm, request["runner"].Pattern},
		{"a heartbeat's agent version", versionForm, request["agent_version"].Pattern},
		{"an idempotency key", keyForm, schema.Defs.IdempotencyKey.Pattern},
	} {
		if c.wire == "" {
			t.Errorf("the wire writes no pattern for %s where this test looks for one", c.what)
			continue
		}
		if c.ours.String() != c.wire {
			t.Errorf("%s is checked against %s, and the wire writes %s", c.what, c.ours, c.wire)
		}
	}
}

// So are the grammars a rotation is checked against.
func TestTheRotationGrammarsAreTheWiresOwn(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	type property struct {
		Pattern string `json:"pattern"`
		Ref     string `json:"$ref"`
	}
	var schema struct {
		Defs struct {
			RunnerRotation struct {
				Properties struct {
					Request struct {
						Properties map[string]property `json:"properties"`
					} `json:"request"`
				} `json:"properties"`
			} `json:"runnerRotation"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	request := schema.Defs.RunnerRotation.Properties.Request.Properties

	if ref := request["at"].Ref; ref != "#/$defs/timestamp" {
		t.Errorf("a rotation's at is no longer the wire's timestamp, so instantForm may not be its grammar")
	}
	for _, c := range []struct {
		what string
		ours *regexp.Regexp
		wire string
	}{
		{"a rotation's runner", runnerForm, request["runner"].Pattern},
		{"a rotation's signature", signatureForm, request["signature"].Pattern},
	} {
		if c.wire == "" {
			t.Errorf("the wire writes no pattern for %s where this test looks for one", c.what)
			continue
		}
		if c.ours.String() != c.wire {
			t.Errorf("%s is checked against %s, and the wire writes %s", c.what, c.ours, c.wire)
		}
	}
}
