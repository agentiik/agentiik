package secret_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/secret"
)

// What a webhook's hmac signature is checked against is opened through this.
var _ api.HookSecrets = (*secret.Hooks)(nil)

// A webhook's secret is sealed as a value is, and opens for the webhook it was sealed for alone: not
// in another namespace, not at another method or path, not at another write of it, not under another
// master key, and never as a secret value of a namespace or a TOTP generator's secret, whose bindings
// never hold the colon and the space its does. Nothing in the sealed bytes is the secret.
func TestAWebhooksSecretOpensForItsOwnWebhookAlone(t *testing.T) {
	keys, err := secret.NewKeyring(master(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := secret.NewHooks(keys)
	if err != nil {
		t.Fatal(err)
	}
	value := bytes.Repeat([]byte("k"), 32)
	sealed, err := h.SealHook("finance", "POST", "/invoicing", 2, value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, value) {
		t.Error("the sealed secret holds the secret")
	}
	opened, err := h.OpenHook("finance", "POST", "/invoicing", 2, sealed)
	if err != nil || !bytes.Equal(opened, value) {
		t.Fatalf("the secret opened as %q: %v", opened, err)
	}
	for _, where := range []struct {
		namespace, method, path string
		version                 int
	}{
		{"payroll", "POST", "/invoicing", 2}, {"finance", "PUT", "/invoicing", 2}, {"finance", "POST", "/invoicing/preview", 2},
		{"finance", "POST", "/invoicing", 1}, {"finance", "POST", "/invoicing", 3}, {"", "POST", "/invoicing", 2},
	} {
		if _, err := h.OpenHook(where.namespace, where.method, where.path, where.version, sealed); err == nil {
			t.Errorf("the secret of POST /invoicing in finance at its second write opened as %+v", where)
		}
	}
	var row secret.Sealed
	if err := json.Unmarshal(sealed, &row); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Open("finance", "POST /invoicing", 2, row); err == nil {
		t.Error("the secret opened as a value of finance")
	}

	other, err := secret.NewKeyring(master(t, "2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := secret.NewHooks(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := elsewhere.OpenHook("finance", "POST", "/invoicing", 2, sealed); !errors.Is(err, secret.ErrNotMine) {
		t.Errorf("under another master key the secret answered %v", err)
	}
	if _, err := h.OpenHook("finance", "POST", "/invoicing", 2, json.RawMessage(`"not sealed"`)); err == nil {
		t.Error("bytes that were never sealed opened")
	}
	if _, err := secret.NewHooks(nil); err == nil {
		t.Error("hooks with no keyring were made")
	}
}
