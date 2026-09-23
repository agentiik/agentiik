package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/secret"
)

// The API with its secret stores attached the way the server's main package attaches them: a
// value written on a declaration's PUT, or kept in the API's environment, is what a redemption
// answers, from whichever store the namespace declared it in.

// TestARedemptionReadsSecretsFromTwoProviders writes one value into the built-in store as text and
// one as bytes that are not, declares a third in the environment, and redeems a grant naming all
// three. Each arrives as it was written, the bytes as base64, and nothing but the redemption ever
// answers one.
func TestARedemptionReadsSecretsFromTwoProviders(t *testing.T) {
	g := withGrants(t, api.NoSecrets{})
	m, err := secret.NewMaster("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secret.NewKeyring(m)
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"AGENTIIK_SECRET_FINANCE_LEDGER": "gl_live_notreal"}

	declarations := api.DeclarationOptions{Pool: g.pool}
	runners := api.RunnerOptions{Pool: g.pool, Objects: g.objects, URLs: g.signed}
	if err := secret.Attach(secret.Options{
		Pool: g.pool, Keys: keys,
		Environment: api.Environment{"finance": "AGENTIIK_SECRET_FINANCE_"},
		Lookup: func(name string) (string, bool) {
			value, set := environment[name]
			return value, set
		},
	}, &declarations, &runners); err != nil {
		t.Fatal(err)
	}
	rt := router(t, everything{who: "admin"})
	if _, err := api.NewRunners(rt, runners); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, g.signed); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewDeclarations(rt, declarations); err != nil {
		t.Fatal(err)
	}
	g.handler = rt

	for name, body := range map[string]string{
		"billing":  `{"provider":"builtin","value":"bk_live_notreal"}`,
		"keystore": `{"provider":"builtin","value":"//4A","encoding":"base64"}`,
		"ledger":   `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_LEDGER"}`,
	} {
		w := sent(t, rt, "PUT", "/api/v1/finance/secrets/"+name, "admin", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("declaring %s answered %d: %s", name, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "bk_live_notreal") || strings.Contains(w.Body.String(), `"value"`) {
			t.Errorf("declaring %s was answered with a value: %s", name, w.Body)
		}
	}

	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"billing", "keystore", "ledger"})
	w, _ := call(t, rt, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
	if w.Code != http.StatusOK {
		t.Fatalf("redeeming answered %d: %s", w.Code, w.Body)
	}
	var answer api.Grant
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	want := []api.Secret{
		{Name: "billing", Mount: "/agk/secrets/billing", Encoding: api.EncodingUTF8, Value: "bk_live_notreal"},
		{Name: "keystore", Mount: "/agk/secrets/keystore", Encoding: api.EncodingBase64, Value: "//4A"},
		{Name: "ledger", Mount: "/agk/secrets/ledger", Encoding: api.EncodingUTF8, Value: "gl_live_notreal"},
	}
	if !slices.Equal(answer.Secrets, want) {
		t.Errorf("the redemption answered the secrets %+v, want %+v", answer.Secrets, want)
	}

	// Removed, a value goes with its declaration: the name declared in the store again, with no
	// value this time, holds nothing a later task could be given.
	if w := sent(t, rt, "DELETE", "/api/v1/finance/secrets/billing", "admin", ""); w.Code != http.StatusNoContent {
		t.Fatalf("removing billing answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, rt, "PUT", "/api/v1/finance/secrets/billing", "admin", `{"provider":"builtin"}`); w.Code != http.StatusCreated {
		t.Fatalf("declaring billing again answered %d: %s", w.Code, w.Body)
	}
	if got, err := runners.Secrets.Value(t.Context(), "finance", "billing"); !errors.Is(err, api.ErrNoSecret) {
		t.Errorf("billing, removed and declared again with no value, reads %q, %v", got, err)
	}
}
