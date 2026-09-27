package secret_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/secret"
)

// What the password sign-in reads a TOTP generator's secret through is this.
var _ api.TOTPSecrets = (*secret.TOTP)(nil)

// A TOTP generator's secret is sealed as a value is, opens where it was sealed and nowhere else: not
// for another user, not in another generator's row, not under another master key, and never as a
// secret value of a namespace, since no namespace's name holds the colon its binding does. Nothing
// in the sealed bytes is the secret.
func TestATOTPSecretOpensForItsOwnGeneratorAlone(t *testing.T) {
	keys, err := secret.NewKeyring(master(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := secret.NewTOTP(keys)
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("bob's twenty byte ke")
	sealed, err := s.SealTOTP("bob", "bob-totp", value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, value) {
		t.Error("the sealed secret holds the secret")
	}
	opened, err := s.OpenTOTP("bob", "bob-totp", sealed)
	if err != nil || !bytes.Equal(opened, value) {
		t.Fatalf("the secret opened as %q: %v", opened, err)
	}
	for _, where := range [][2]string{{"alice", "bob-totp"}, {"bob", "bob-totp-2"}, {"", "bob-totp"}, {"bob", ""}} {
		if _, err := s.OpenTOTP(where[0], where[1], sealed); err == nil {
			t.Errorf("bob's secret opened as %s's generator %s", where[0], where[1])
		}
	}
	var row secret.Sealed
	if err := json.Unmarshal(sealed, &row); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Open("totp", "bob-totp", 1, row); err == nil {
		t.Error("bob's secret opened as a value of a namespace")
	}

	other, err := secret.NewKeyring(master(t, "2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := secret.NewTOTP(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := elsewhere.OpenTOTP("bob", "bob-totp", sealed); !errors.Is(err, secret.ErrNotMine) {
		t.Errorf("under another master key the secret answered %v", err)
	}
	if _, err := s.OpenTOTP("bob", "bob-totp", []byte("not sealed")); err == nil {
		t.Error("bytes that were never sealed opened")
	}
	if _, err := secret.NewTOTP(nil); err == nil {
		t.Error("a TOTP with no keyring was made")
	}
}
