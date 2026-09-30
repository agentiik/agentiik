package secret

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The secret of a webhook, sealed under the installation's keyring as a built-in secret's value is:
// an hmac signature is checked against the secret itself, so it cannot be kept as a hash, and a dump
// of the credentials table alone opens nothing.
//
// A sealed secret is bound to its namespace and to the method and the path its webhook answers,
// under hookNamespace, and to the write it is, so that a ciphertext copied to another namespace's
// webhook, or to another path, or restored over a later write, does not open. Not to its workflow,
// whose name a rename changes and which answers nothing a caller signs: within a namespace a path and
// a method answer one trigger. The colon is in no namespace's name and the space in no secret's, so
// no secret value and no TOTP generator is ever bound where a webhook's secret is.

// hookNamespace begins the namespace a webhook's secret is bound under, followed by the namespace
// its webhook answers in.
const hookNamespace = "webhook:"

// Hooks seals and opens the secrets of webhooks under an installation's keyring. It is
// api.HookSecrets.
type Hooks struct {
	keys *Keyring
}

// NewHooks seals under the current key of the ring and opens under whichever key of it sealed.
func NewHooks(keys *Keyring) (*Hooks, error) {
	if keys == nil {
		return nil, errors.New("secret: no keyring, and a webhook's secret is sealed under a master key held outside the database")
	}
	return &Hooks{keys: keys}, nil
}

// SealHook seals the secret of the webhook at method and path of namespace, at its write version,
// for its row of credentials.
func (h *Hooks) SealHook(namespace, method, path string, version int, secret []byte) (json.RawMessage, error) {
	if namespace == "" || method == "" || path == "" {
		return nil, errors.New("secret: a webhook's secret is bound to its namespace, its method and its path, and one of them is missing")
	}
	s, err := h.keys.Seal(hookNamespace+namespace, method+" "+path, version, secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// OpenHook opens the secret of the webhook at method and path of namespace, as its row of
// credentials holds it at its write version.
func (h *Hooks) OpenHook(namespace, method, path string, version int, sealed json.RawMessage) ([]byte, error) {
	if namespace == "" || method == "" || path == "" {
		return nil, errors.New("secret: a webhook's secret is bound to its namespace, its method and its path, and one of them is missing")
	}
	var s Sealed
	if err := json.Unmarshal(sealed, &s); err != nil {
		return nil, fmt.Errorf("secret: the secret of the webhook %s %s of %s is not a sealed value: %w", method, path, namespace, err)
	}
	return h.keys.Open(hookNamespace+namespace, method+" "+path, version, s)
}
