package api

import (
	"errors"
	"testing"

	"github.com/agentiik/agentiik/db"
)

// The rule a removal is held to, case by case, the last credential among them, which no route can
// ask about once device_bound_only has ended the session a synced passkey opened: min_passkeys
// counted in the passkeys the policy accepts, the password kept below it and where no passkey signs
// anybody in, a generator removed with a code alone, and never the last credential.
func TestARemovalLeavesWhatThePolicyAsks(t *testing.T) {
	passkey := func(id string, synced bool) db.Credential {
		return db.Credential{ID: id, Type: db.CredentialPasskey, BackupEligible: synced}
	}
	password := db.Credential{ID: "pw", Type: db.CredentialPassword}
	generator := db.Credential{ID: "totp", Type: db.CredentialTOTP}
	two := accountPolicy{minPasskeys: 2}
	bound := accountPolicy{minPasskeys: 1, deviceBoundOnly: true}
	var below *errBelow
	for _, c := range []struct {
		name   string
		policy accountPolicy
		held   []db.Credential
		target db.Credential
		ip     bool
		want   func(error) bool
	}{
		{"one of three passkeys", two, []db.Credential{passkey("a", false), passkey("b", false), passkey("c", false)}, passkey("c", false), false, isNil},
		{"one of two passkeys", two, []db.Credential{passkey("a", false), passkey("b", false)}, passkey("b", false), false, func(err error) bool { return errors.As(err, &below) && below.left == 1 }},
		{"the password beside one passkey of two", two, []db.Credential{password, passkey("a", false)}, password, false, func(err error) bool { return errors.As(err, &below) && below.left == 1 }},
		{"the password beside two passkeys of two", two, []db.Credential{password, generator, passkey("a", false), passkey("b", false)}, password, false, isNil},
		{"the password where no passkey signs in", two, []db.Credential{password, passkey("a", false), passkey("b", false)}, password, true, is(errOnlyHere)},
		{"a generator", two, []db.Credential{password, generator}, generator, false, is(errWithACode)},
		{"a synced passkey beside a device-bound one", bound, []db.Credential{passkey("a", false), passkey("s", true)}, passkey("s", true), false, isNil},
		{"the one device-bound passkey", bound, []db.Credential{passkey("a", false), passkey("s", true)}, passkey("a", false), false, func(err error) bool { return errors.As(err, &below) && below.left == 0 }},
		{"the last credential, a synced passkey", bound, []db.Credential{passkey("s", true)}, passkey("s", true), false, is(errLast)},
		{"a synced passkey beside a password", bound, []db.Credential{password, passkey("s", true)}, passkey("s", true), false, isNil},
	} {
		if err := removable(c.policy, c.held, c.target, c.ip); !c.want(err) {
			t.Errorf("removing %s answered %v", c.name, err)
		}
	}
}

func isNil(err error) bool { return err == nil }

func is(want error) func(error) bool { return func(err error) bool { return errors.Is(err, want) } }
