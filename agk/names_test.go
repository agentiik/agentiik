package agk_test

import (
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// TestTheIdentifierGrammar is the grammar the schema writes as
// ^[A-Za-z0-9][A-Za-z0-9_-]*$, and the reason it is that one: the name travels as a
// directory under /agk/in/, as a file under /agk/out/ports/ and as one entry of the
// comma separated AGK_OUT_PORTS.
func TestTheIdentifierGrammar(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"normalize", true},
		{"out", true},
		{"ok", true},
		{"error", true},
		{"fetch-invoices", true},
		{"step_2", true},
		{"9lives", true},
		{"", false},
		{"out,error", false},
		{"two words", false},
		{"in/out", false},
		{"-leading", false},
		{"_leading", false},
		{"café", false},
		{"out.json", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, err := range []error{agk.Step(c.name).Validate(), agk.Port(c.name).Validate()} {
				if c.valid && err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !c.valid && err == nil {
					t.Fatal("accepted")
				}
			}
		})
	}
}

func TestAPortNameSaysWhatTheRuleIs(t *testing.T) {
	err := agk.Port("out,error").Validate()
	if err == nil {
		t.Fatal("accepted a port name carrying a comma")
	}
	if !strings.Contains(err.Error(), `^[A-Za-z0-9][A-Za-z0-9_-]*$`) {
		t.Fatalf("the refusal does not print the grammar: %v", err)
	}
}

// TestARunIdentifier imposes no length. The documentation says a run carries a ULID and
// then prints run identifiers shorter than twenty-six characters, no two of them the
// same length; the schema records that reading and asks only that the value be there.
func TestARunIdentifier(t *testing.T) {
	cases := []struct {
		id    agk.RunID
		valid bool
	}{
		{"01JMZ8W4K2R7Q0E3N5T9ZQ4XKB", true},
		{"01JMZ8W4K2R7Q0E3N5T9", true},
		{"run-12", true},
		{"", false},
		{"01JMZ8/W4K2R7", false},
		{"01JMZ8 W4K2R7", false},
		{"..", false},
	}
	for _, c := range cases {
		err := c.id.Validate()
		if c.valid && err != nil {
			t.Errorf("%q was refused: %v", c.id, err)
		}
		if !c.valid && err == nil {
			t.Errorf("%q was accepted", c.id)
		}
	}
}

func TestNewRunIDMintsAULID(t *testing.T) {
	id := agk.NewRunID()
	if len(id) != 26 {
		t.Fatalf("NewRunID minted %q, which is not a twenty-six character identifier", id)
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("NewRunID minted an identifier it then refuses: %v", err)
	}
	if agk.NewRunID() == id {
		t.Fatal("NewRunID minted the same identifier twice")
	}
}
