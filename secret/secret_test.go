package secret_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/secret"
)

// The built-in store, held to the sentence it exists for: "Envelope encryption with AES-256-GCM
// data keys, wrapped by a master key held outside the database."

func master(t *testing.T, id string) *secret.Master {
	t.Helper()
	m, err := secret.NewMaster(id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A value goes in and comes back, and nothing in between is the value.
func TestAValueSurvivesBeingSealed(t *testing.T) {
	m := master(t, "2026-09")
	const value = "hunter2, and a good deal longer than that in practice"

	s, err := m.Seal("finance", "billing", 1, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if s.Master != "2026-09" || s.Version != 1 {
		t.Errorf("the sealed value reads %+v", s)
	}
	for _, part := range [][]byte{s.Value, s.Key, s.Nonce, s.WrapNonce, s.Salt} {
		if bytes.Contains(part, []byte(value)) {
			t.Error("a sealed value holds the value")
		}
		if len(part) == 0 {
			t.Error("a sealed value has an empty part")
		}
	}

	back, err := m.Open("finance", "billing", 1, s)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != value {
		t.Errorf("it came back as %q", back)
	}
}

// One data key per write, never reused. Two writes of one value are two ciphertexts and two
// wrapped keys, which is what makes a nonce collision something that cannot happen rather than
// something that is unlikely.
func TestEveryWriteGetsItsOwnDataKey(t *testing.T) {
	m := master(t, "2026-09")
	seen := map[string]bool{}
	for version := 1; version <= 8; version++ {
		s, err := m.Seal("finance", "billing", version, []byte("one value"))
		if err != nil {
			t.Fatal(err)
		}
		for what, part := range map[string][]byte{"key": s.Key, "ciphertext": s.Value, "nonce": s.Nonce, "salt": s.Salt} {
			if seen[what+string(part)] {
				t.Fatalf("two writes shared a %s", what)
			}
			seen[what+string(part)] = true
		}
	}
}

// The additional data binds a ciphertext to where it lives, so one lifted into another row fails
// to open rather than opening as somebody else's secret. That is the attack a database with a
// writable row invites, and the cipher refuses it rather than a comparison somebody could forget.
func TestACiphertextCannotBeMovedSomewhereElse(t *testing.T) {
	m := master(t, "2026-09")
	s, err := m.Seal("finance", "billing", 3, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name      string
		namespace string
		secret    string
		version   int
	}{
		{"another namespace", "team-ops", "billing", 3},
		{"another secret", "finance", "shipping", 3},
	} {
		if _, err := m.Open(c.namespace, c.secret, c.version, s); err == nil {
			t.Errorf("a ciphertext opened under %s", c.name)
		}
	}

	// And a namespace that could be confused with a name cannot be: the binding is a
	// document rather than a joined string.
	a, err := m.Seal("a", "b/c", 1, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open("a/b", "c", 1, a); err == nil {
		t.Error("a name carrying a separator was read as a different namespace and name")
	}
}

// A value sealed under another master key says so, rather than failing as corruption: an
// installation holding two generations can tell "not mine" from "broken".
func TestAValueOfAnotherGenerationSaysSo(t *testing.T) {
	was, now := master(t, "2026-01"), master(t, "2026-09")
	s, err := was.Seal("finance", "billing", 1, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := now.Open("finance", "billing", 1, s); !errors.Is(err, secret.ErrNotMine) {
		t.Fatalf("a value of another generation answered %v", err)
	}
	// And the one that sealed it still opens it, which is what makes rotation a write
	// rather than a migration everything has to wait for.
	if _, err := was.Open("finance", "billing", 1, s); err != nil {
		t.Errorf("the key that sealed it could not open it: %s", err)
	}
}

// A ciphertext anybody changed does not open. It is what GCM is for, and it is worth a test
// because a store that returned a silently altered value would be worse than one that failed.
func TestATamperedValueDoesNotOpen(t *testing.T) {
	m := master(t, "2026-09")
	s, err := m.Seal("finance", "billing", 1, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		with func(*secret.Sealed)
	}{
		{"the ciphertext", func(s *secret.Sealed) { s.Value[0] ^= 1 }},
		{"the salt", func(s *secret.Sealed) { s.Salt[0] ^= 1 }},
		{"the wrapped key", func(s *secret.Sealed) { s.Key[0] ^= 1 }},
		{"the nonce", func(s *secret.Sealed) { s.Nonce[0] ^= 1 }},
		{"the wrapping nonce", func(s *secret.Sealed) { s.WrapNonce[0] ^= 1 }},
	} {
		bad := secret.Sealed{
			Master: s.Master, Version: s.Version,
			Salt: append([]byte(nil), s.Salt...),
			Key:  append([]byte(nil), s.Key...), WrapNonce: append([]byte(nil), s.WrapNonce...),
			Value: append([]byte(nil), s.Value...), Nonce: append([]byte(nil), s.Nonce...),
		}
		c.with(&bad)
		if _, err := m.Open("finance", "billing", 1, bad); err == nil {
			t.Errorf("a value with %s changed opened anyway", c.name)
		}
	}
}

// "its master key read from a file the API user alone can open". A file anybody can read is a
// master key anybody has, so it is refused rather than warned about.
func TestAMasterKeyAnybodyCanReadIsRefused(t *testing.T) {
	dir := t.TempDir()
	m := master(t, "2026-09")

	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, m.Write(), 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := secret.LoadMaster(path)
	if err != nil {
		t.Fatalf("a key the owner alone can read was refused: %s", err)
	}
	if back.ID() != "2026-09" {
		t.Errorf("it came back as %q", back.ID())
	}

	// And what it opens is what the original sealed, which is the point of writing it down
	// at all.
	s, err := m.Seal("finance", "billing", 1, []byte("the value"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := back.Open("finance", "billing", 1, s); err != nil || string(got) != "the value" {
		t.Errorf("the key read back from the file opened it as %q, %v", got, err)
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o666, 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := secret.LoadMaster(path)
		if err == nil {
			t.Errorf("a master key at mode %#o was accepted", mode)
			continue
		}
		if !strings.Contains(err.Error(), "readable by its owner alone") {
			t.Errorf("the refusal at mode %#o reads %q", mode, err)
		}
	}
}

// What is not a master key.
func TestWhatIsNotAMasterKey(t *testing.T) {
	for _, c := range []struct{ body, why string }{
		{"", "nothing at all"},
		{"key: AAAA\n", "no identifier"},
		{"id: 2026-09\n", "no key"},
		{"id: 2026-09\nkey: not base64!\n", "not base64"},
		{"id: 2026-09\nkey: AAAA\n", "too short for AES-256"},
		{"id: 2026 09\nkey: AAAA\n", "an identifier with a space in it"},
		{"id: 2026-09\nwhat: x\n", "a field nobody knows"},
	} {
		if _, err := secret.ParseMaster([]byte(c.body)); err == nil {
			t.Errorf("%q was read as a master key, and it is %s", c.body, c.why)
		}
	}

	// A comment and a blank line are fine, because a key file is a thing a person edits.
	ok := "# the master key\n\nid: 2026-09\nkey: " +
		strings.TrimSpace(strings.SplitN(string(master(t, "x").Write()), "key: ", 2)[1])
	if _, err := secret.ParseMaster([]byte(ok)); err != nil {
		t.Errorf("a key file with a comment in it was refused: %s", err)
	}
}
