package secret

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The secret of a TOTP generator, "sealed under the master key, since a code is checked against the
// secret itself": a code cannot be checked against a hash, so the secret is kept as a builtin value
// is, under a data key of its own wrapped by the master key, and a dump of the credentials table
// alone opens nothing.
//
// A sealed secret is bound to its user and its credential, as a value is to its namespace and its
// name: under totpNamespace and the login, and the credential's identifier, so that a ciphertext
// copied into another user's row, or another generator's, does not open, and none can be taken for a
// secret value, whose namespace never holds the colon. A generator is written once, when it is
// enrolled, and replaced by enrolling another, which is another row, so its version is always the
// first.
//
// The column holds the sealed value as JSON, every field of which is safe to store.

// totpNamespace begins the namespace a TOTP secret is bound under, followed by its user's login.
// The colon is in no namespace's name, so no secret value is ever bound where a TOTP secret is.
const totpNamespace = "totp:"

// totpVersion is the one version of a generator's secret.
const totpVersion = 1

// TOTP seals and opens the secrets of TOTP generators under an installation's keyring. It is
// api.TOTPSecrets, and seals what enrolling a generator writes.
type TOTP struct {
	keys *Keyring
}

// NewTOTP seals under the current key of the ring and opens under whichever key of it sealed.
func NewTOTP(keys *Keyring) (*TOTP, error) {
	if keys == nil {
		return nil, errors.New("secret: no keyring, and a TOTP generator's secret is sealed under a master key held outside the database")
	}
	return &TOTP{keys: keys}, nil
}

// SealTOTP seals the secret of login's generator id, for its row of credentials.
func (t *TOTP) SealTOTP(login, id string, secret []byte) ([]byte, error) {
	if login == "" || id == "" {
		return nil, errors.New("secret: a TOTP generator's secret is bound to its user and its identifier, and one of them is missing")
	}
	s, err := t.keys.Seal(totpNamespace+login, id, totpVersion, secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// OpenTOTP opens the secret of login's generator id, as its row of credentials holds it.
func (t *TOTP) OpenTOTP(login, id string, sealed []byte) ([]byte, error) {
	if login == "" || id == "" {
		return nil, errors.New("secret: a TOTP generator's secret is bound to its user and its identifier, and one of them is missing")
	}
	var s Sealed
	if err := json.Unmarshal(sealed, &s); err != nil {
		return nil, fmt.Errorf("secret: the TOTP generator %s of %s is not a sealed value: %w", id, login, err)
	}
	return t.keys.Open(totpNamespace+login, id, totpVersion, s)
}
