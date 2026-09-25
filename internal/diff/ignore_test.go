package diff

import (
	"strings"
	"testing"
)

func TestTheFlagIsReadBackAsWhatItNames(t *testing.T) {
	for _, c := range []struct {
		written string
		want    Ignore
	}{
		{"", 0},
		{"nothing", 0},
		{"meta.run_id", IgnoreRunID},
		{"meta.run_id,meta.produced_at,files.uri", Default},
		{" items.id , meta.run_id ", IgnoreItemIDs | IgnoreRunID},
		{"meta.run_id,meta.run_id", IgnoreRunID},
	} {
		got, err := ParseIgnore(c.written)
		if err != nil {
			t.Fatalf("%q: %v", c.written, err)
		}
		if got != c.want {
			t.Errorf("%q reads as %s and it reads as %s", c.written, got, c.want)
		}
	}
}

// TestWhatIsHeldAsideIsPrintedAsItIsRead is what lets a command print the default of its
// own flag out of the value rather than out of a second string beside it.
func TestWhatIsHeldAsideIsPrintedAsItIsRead(t *testing.T) {
	if got, want := Default.String(), "meta.run_id,meta.produced_at,files.uri"; got != want {
		t.Errorf("the default prints as %q and it prints as %q", got, want)
	}
	if got, want := (Default | IgnoreItemIDs).String(), "meta.run_id,meta.produced_at,files.uri,items.id"; got != want {
		t.Errorf("it prints as %q and it prints as %q", got, want)
	}
	if got, want := Ignore(0).String(), "nothing"; got != want {
		t.Errorf("holding nothing aside prints as %q", got)
	}
	back, err := ParseIgnore((Default | IgnoreItemIDs).String())
	if err != nil || back != Default|IgnoreItemIDs {
		t.Errorf("what it printed does not read back: %s, %v", back, err)
	}
}

// TestAMemberTheEnvelopeDoesNotCarryIsRefusedNamingWhatThereIs, because an ignore flag
// that was ignored would make a test pass for a reason nobody asked for.
func TestAMemberTheEnvelopeDoesNotCarryIsRefusedNamingWhatThereIs(t *testing.T) {
	_, err := ParseIgnore("meta.run_id,item.id")
	if err == nil {
		t.Fatal("item.id was accepted, and the member is items.id")
	}
	for _, want := range []string{"item.id", "meta.run_id", "meta.produced_at", "files.uri", "items.id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}
