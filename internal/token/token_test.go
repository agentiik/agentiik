package token

import (
	"regexp"
	"strings"
	"testing"
)

// The three patterns the wire holds a credential to, copied here so that a credential minted by
// this package and a credential the schema accepts cannot drift apart without a test saying so.
var (
	grantForm  = regexp.MustCompile(`^agkgrant_[0-9A-HJKMNP-TV-Z]+_[A-Za-z0-9_-]{16,}$`)
	joinForm   = regexp.MustCompile(`^agkjoin_[A-Za-z0-9_-]{43,}$`)
	runnerForm = regexp.MustCompile(`^agkrunner_[A-Za-z0-9_-]{43,}$`)
)

const aTask = "01M2AAZ9G62NQXFAFCXKRPJEH5"

// What is minted is what the wire accepts, which is the one thing this package could get wrong in
// a way nothing else would notice until a runner was refused at the door.
func TestWhatIsMintedIsWhatTheWireAccepts(t *testing.T) {
	for _, c := range []struct {
		kind Kind
		id   string
		form *regexp.Regexp
	}{
		{Grant, aTask, grantForm},
		{Join, "", joinForm},
		{Runner, "", runnerForm},
	} {
		clear, hashed, err := New(c.kind, c.id)
		if err != nil {
			t.Fatalf("%s: %s", c.kind, err)
		}
		if !c.form.MatchString(clear) {
			t.Errorf("a %s is written %q, which the wire refuses", c.kind, clear)
		}
		if !Same(clear, hashed) {
			t.Errorf("a %s does not match the hash it was minted with", c.kind)
		}
		if strings.Contains(hashed, clear) {
			t.Errorf("the stored form of a %s holds the clear value", c.kind)
		}
		if len(hashed) != 64 {
			t.Errorf("the stored form of a %s is %d characters", c.kind, len(hashed))
		}
	}
}

// Two credentials are never the same one, which is the whole of what the entropy is for.
func TestNoTwoCredentialsAreTheSame(t *testing.T) {
	seen := map[string]bool{}
	for range 512 {
		clear, _, err := New(Join, "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[clear] {
			t.Fatalf("two credentials came out the same: %s", clear)
		}
		seen[clear] = true
	}

	// And the secret half is the full width the wire's minimum is written for, rather than
	// the minimum itself: forty-three base64url characters is 256 bits.
	clear, _, err := New(Join, "")
	if err != nil {
		t.Fatal(err)
	}
	if secret := strings.TrimPrefix(clear, "agkjoin_"); len(secret) != secretLength {
		t.Errorf("the secret half is %d characters and 256 bits needs %d", len(secret), secretLength)
	}
}

// A grant names its task inside its own text, which is what lets the API refuse a redemption
// whose body names a different one.
func TestAGrantNamesItsTask(t *testing.T) {
	clear, _, err := New(Grant, aTask)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := TaskOf(clear)
	if !ok || got != aTask {
		t.Fatalf("the grant %q names task %q, %v", clear, got, ok)
	}

	kind, ok := KindOf(clear)
	if !ok || kind != Grant {
		t.Errorf("the grant reads as %q, %v", kind, ok)
	}

	// A credential of another kind is not a grant, and is refused for being the wrong sort of
	// thing rather than after a lookup the wrong door should not have made.
	join, _, err := New(Join, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := TaskOf(join); ok {
		t.Error("a join token was read as a grant")
	}
	if kind, _ := KindOf(join); kind != Join {
		t.Errorf("a join token reads as %q", kind)
	}
}

// What cannot be minted, and what is not a credential.
func TestWhatIsNotACredential(t *testing.T) {
	if _, _, err := New(Grant, ""); err == nil {
		t.Error("a grant was minted naming no task")
	}
	if _, _, err := New(Join, aTask); err == nil {
		t.Error("a join token was minted carrying an identifier, and what it is bound to is a row")
	}
	if _, _, err := New("agkwhat", ""); err == nil {
		t.Error("a credential of an unknown kind was minted")
	}
	if _, _, err := New(Grant, "has_underscore"); err == nil {
		t.Error("an identifier carrying the separator was accepted, and it would split the credential somewhere else")
	}

	for _, c := range []string{"", "agkgrant", "agkgrant_", "agkgrant_x", "nonsense", "agkjoin_short"} {
		if _, ok := KindOf(c); ok {
			t.Errorf("%q was read as a credential", c)
		}
	}
}

// A hash answers for one credential and not for a near miss.
func TestAHashAnswersForOneCredential(t *testing.T) {
	clear, hashed, err := New(Runner, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, near := range []string{
		clear[:len(clear)-1],
		clear + "x",
		strings.Replace(clear, "agkrunner_", "agkjoin_", 1),
		"",
	} {
		if Same(near, hashed) {
			t.Errorf("%q was accepted for a credential it is not", near)
		}
	}
}
