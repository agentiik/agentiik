package secret_test

import (
	"errors"
	"testing"

	"github.com/agentiik/agentiik/secret"
)

// Retiring a master key, which is the half of rotation the page does not describe and without
// which a rotation is something an installation starts and never finishes.

// The whole path: seal under the old key, rotate, keep reading, reseal, and only then is the old
// key destroyable.
func TestAKeyIsRetirableOnlyWhenNothingIsSealedUnderIt(t *testing.T) {
	was, now := master(t, "2026-01"), master(t, "2026-09")

	old, err := was.Seal("finance", "billing", 3, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}

	ring, err := secret.NewKeyring(now, was)
	if err != nil {
		t.Fatal(err)
	}

	// While it waits its turn it still opens, which is what makes a rotation something an
	// installation can do without stopping.
	if got, err := ring.Open("finance", "billing", 3, old); err != nil || string(got) != "the value" {
		t.Fatalf("a value waiting to be resealed does not open: %q, %v", got, err)
	}
	if ok, left := ring.Retirable([]string{"2026-01", "2026-09"}); ok {
		t.Error("the old key was called retirable while something was still sealed under it")
	} else if len(left) != 1 || left[0] != "2026-01" {
		t.Errorf("what is left reads %v", left)
	}

	// Resealed, and the version does not move: this is the same write under another key, and
	// bumping it would make maintenance look like a rotation in every audit that counts.
	next, moved, err := ring.Reseal("finance", "billing", 3, old)
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("a value under the old key was not moved")
	}
	if next.Master != "2026-09" || next.Version != 3 {
		t.Errorf("the resealed value reads master %q version %d", next.Master, next.Version)
	}
	if got, err := ring.Open("finance", "billing", 3, next); err != nil || string(got) != "the value" {
		t.Errorf("the resealed value does not open: %q, %v", got, err)
	}

	// Running the sweep again does nothing, which is what lets it be run until it says there
	// is nothing left.
	if _, moved, err := ring.Reseal("finance", "billing", 3, next); err != nil || moved {
		t.Errorf("resealing a value already under the current key answered %v, %v", moved, err)
	}
	if ok, left := ring.Retirable([]string{"2026-09"}); !ok {
		t.Errorf("with everything moved the key is still not retirable: %v", left)
	}
}

// The old key can be destroyed once the sweep is done, and then the new key alone reads
// everything. That is the whole point of the exercise.
func TestOnceEverythingHasMovedTheOldKeyIsNotNeeded(t *testing.T) {
	was, now := master(t, "2026-01"), master(t, "2026-09")
	old, err := was.Seal("finance", "billing", 1, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secret.NewKeyring(now, was)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := ring.Reseal("finance", "billing", 1, old)
	if err != nil {
		t.Fatal(err)
	}

	// The old key is gone from the ring, and from the host.
	alone, err := secret.NewKeyring(now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := alone.Open("finance", "billing", 1, next); err != nil || string(got) != "the value" {
		t.Errorf("the resealed value does not open under the current key alone: %q, %v", got, err)
	}
	// And what was never moved says so, rather than failing as corruption.
	if _, err := alone.Open("finance", "billing", 1, old); !errors.Is(err, secret.ErrNotMine) {
		t.Errorf("a value left behind answered %v", err)
	}
}

// A resealed value is bound exactly as a freshly sealed one is, so none of the four attacks that
// worked against the first version work against the way out of a rotation either.
func TestAResealedValueIsBoundLikeAnyOther(t *testing.T) {
	was, now := master(t, "2026-01"), master(t, "2026-09")
	ring, err := secret.NewKeyring(now, was)
	if err != nil {
		t.Fatal(err)
	}
	old, err := was.Seal("finance", "billing", 2, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := ring.Reseal("finance", "billing", 2, old)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name      string
		namespace string
		secret    string
		version   int
	}{
		{"another namespace", "team-ops", "billing", 2},
		{"another secret", "finance", "shipping", 2},
		{"another version", "finance", "billing", 3},
	} {
		if _, err := ring.Open(c.namespace, c.secret, c.version, next); err == nil {
			t.Errorf("a resealed value opened under %s", c.name)
		}
	}
}

// A ring that cannot tell two of its keys apart is refused, because a value naming one of them
// would be opened with whichever happened to be found.
func TestAKeyringRefusesTwoKeysOfOneName(t *testing.T) {
	if _, err := secret.NewKeyring(nil); err == nil {
		t.Error("a keyring with nothing to seal under was built")
	}
	a, b := master(t, "2026-09"), master(t, "2026-09")
	if _, err := secret.NewKeyring(a, b); err == nil {
		t.Error("a key was put on the ring beside another of the same name")
	}
	c, d := master(t, "2026-01"), master(t, "2026-01")
	if _, err := secret.NewKeyring(master(t, "2026-09"), c, d); err == nil {
		t.Error("two past keys of one name were put on the ring")
	}
}
