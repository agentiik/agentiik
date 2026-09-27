package main

import (
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// What agk whoami says of each thing the installation tells: a grant an administrator wrote, a deny
// they took from their own access, told after it was written, and a passkey refused.
func TestWhoamiSaysWhatItIsTold(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	ends := at.Add(24 * time.Hour)
	finance := access.Scope{Namespace: "finance"}
	for _, c := range []struct {
		told api.Notification
		want string
	}{
		{api.Notification{ID: "01A", Kind: "admin_access_widened", At: at, Namespace: "finance",
			Grant: &access.Grant{Principal: "carol", Scope: finance, Role: access.Editor, ExpiresAt: &ends, GrantedBy: "carol"}},
			"told 01A at 2026-09-27T14:00:00Z: carol, an administrator, granted editor on finance to carol until 2026-09-28T14:00:00Z"},
		{api.Notification{ID: "01B", Kind: "admin_access_widened", At: at, Namespace: "finance",
			Grant: &access.Grant{Principal: "group:admins", Scope: finance, Deny: access.RunReadData, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01B at 2026-09-27T14:00:00Z: an administrator lifted the deny of run:read_data on finance for group:admins, widening their own access"},
		{api.Notification{ID: "01D", Kind: "admin_access_widened", At: at, Namespace: "finance",
			Grant: &access.Grant{Principal: "alice", Scope: finance, Deny: access.RunReadData, GrantedBy: "carol", GrantedAt: at}},
			"told 01D at 2026-09-27T14:00:00Z: carol, an administrator, granted a deny of run:read_data on finance to alice"},
		{api.Notification{ID: "01C", Kind: "passkey_counter_refused", At: at, Credential: "aVBob25l"},
			"told 01C at 2026-09-27T14:00:00Z: a sign-in with your passkey aVBob25l was refused because its signature counter did not move forward, as a copy of it would; remove it if the other copy is not yours"},
	} {
		if got := noticeLine(c.told); got != c.want {
			t.Errorf("agk whoami says\n%s\nwant\n%s", got, c.want)
		}
	}
}
