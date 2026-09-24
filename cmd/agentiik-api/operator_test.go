package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// theToken is an operator's token, and theHash what the file AGK_OPERATOR_TOKEN_FILE names holds.
const theToken = "agk_op_3q2Z7x9Kf1LmQ8vR4tYw6pBn0sDhJc5A"

var theHash = func() string {
	sum := sha256.Sum256([]byte(theToken))
	return hex.EncodeToString(sum[:])
}()

func anOperator(t *testing.T) *operator {
	t.Helper()
	o, err := newOperator(theHash)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// The token identifies the operator, and nothing else identifies anybody: not a token that is
// wrong, not the hash itself presented as a token, and not a credential presented any other way.
func TestTheTokenIdentifiesTheOperatorAndNothingElseIdentifiesAnybody(t *testing.T) {
	o := anOperator(t)
	for header, want := range map[string]api.Principal{
		"Bearer " + theToken:        theOperator,
		"":                          "",
		"Bearer ":                   "",
		"Bearer " + theToken + "x":  "",
		"Bearer " + theToken[:20]:   "",
		"Bearer " + theHash:         "",
		"bearer " + theToken:        "",
		"Basic " + theToken:         "",
		theToken:                    "",
		"Bearer  " + theToken:       "",
		"Bearer " + theToken + "\n": "",
	} {
		r := httptest.NewRequest("GET", "/api/v1/finance/runs", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		who, err := o.identify(r)
		if err != nil {
			t.Errorf("identifying %q failed: %s", header, err)
		}
		if who != want {
			t.Errorf("%q is identified as %q, want %q", header, who, want)
		}
	}
}

// The operator is allowed every permission at every scope, and nobody else anything: not the
// empty principal, which is nobody, and not a principal the operator's token did not identify.
func TestTheOperatorIsAllowedEverythingAndEveryoneElseNothing(t *testing.T) {
	o := anOperator(t)
	targets := []api.Target{{}, {Namespace: "finance"}, {Namespace: "finance", Workflow: "monthly-invoicing"}}
	for _, p := range api.Permissions {
		for _, over := range targets {
			if ok, err := o.Allow(t.Context(), theOperator, p, over); !ok || err != nil {
				t.Errorf("the operator was refused %s over %+v: %v", p, over, err)
			}
			for _, who := range []api.Principal{"", "alice", "Operator", "operator "} {
				if ok, err := o.Allow(t.Context(), who, p, over); ok || err != nil {
					t.Errorf("%q was allowed %s over %+v: %v", who, p, over, err)
				}
			}
		}
	}
	if ok, _ := o.Allow(t.Context(), theOperator, "workflow:everything", api.Target{}); ok {
		t.Error("the operator was allowed a permission the documentation does not name")
	}
}

func TestAnOperatorIsBuiltFromAHashAndNothingElse(t *testing.T) {
	for _, hash := range []string{"", theToken, theHash[:62], theHash + "00", "zz" + theHash[2:]} {
		if _, err := newOperator(hash); err == nil {
			t.Errorf("an operator was built from %q", hash)
		}
	}
}
