package main

import (
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// What agk whoami says of each thing the installation tells: each act by which an administrator
// widened access, with who did it, a grant written, a deny lifted from their own access, somebody or
// themselves put in a group holding a role, and themselves taken out of a group or removing one whose
// deny applied; a passkey refused; the break-glass path used; and a recovery code issued the caller
// and spent, a notice missing what it names said by its kind alone.
func TestWhoamiSaysWhatItIsTold(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	ends := at.Add(24 * time.Hour)
	finance := access.Scope{Namespace: "finance"}
	payroll := access.Scope{Namespace: "finance", Workflow: "payroll"}
	for _, c := range []struct {
		told api.Notification
		want string
	}{
		{api.Notification{ID: "01A", Kind: "admin_access_widened", At: at, Act: "granted", By: "carol", Namespace: "finance",
			Grant: &access.Grant{Principal: "carol", Scope: finance, Role: access.Editor, ExpiresAt: &ends, GrantedBy: "carol"}},
			"told 01A at 2026-09-27T14:00:00Z: carol, an administrator, granted editor on finance to carol until 2026-09-28T14:00:00Z"},
		{api.Notification{ID: "01B", Kind: "admin_access_widened", At: at, Act: "deny_lifted", By: "carol", Namespace: "finance",
			Grant: &access.Grant{Principal: "carol", Scope: finance, Deny: access.RunReadData, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01B at 2026-09-27T14:00:00Z: carol, an administrator, lifted the deny of run:read_data on finance for carol, widening their own access"},
		{api.Notification{ID: "01D", Kind: "admin_access_widened", At: at, Act: "granted", By: "carol", Namespace: "finance",
			Grant: &access.Grant{Principal: "alice", Scope: finance, Deny: access.RunReadData, GrantedBy: "carol", GrantedAt: at}},
			"told 01D at 2026-09-27T14:00:00Z: carol, an administrator, granted a deny of run:read_data on finance to alice"},
		{api.Notification{ID: "01F", Kind: "admin_access_widened", At: at, Act: "joined_group", By: "carol", Login: "alice", Namespace: "finance",
			Grant: &access.Grant{Principal: "group:auditors", Scope: payroll, Role: access.Editor, ExpiresAt: &ends, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01F at 2026-09-27T14:00:00Z: carol, an administrator, put alice in group:auditors, which holds editor on finance/payroll until 2026-09-28T14:00:00Z"},
		{api.Notification{ID: "01G", Kind: "admin_access_widened", At: at, Act: "joined_group", By: "carol", Login: "carol", Namespace: "finance",
			Grant: &access.Grant{Principal: "group:auditors", Scope: payroll, Role: access.Editor, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01G at 2026-09-27T14:00:00Z: carol, an administrator, put themselves in group:auditors, which holds editor on finance/payroll"},
		{api.Notification{ID: "01H", Kind: "admin_access_widened", At: at, Act: "left_group", By: "carol", Namespace: "hr",
			Grant: &access.Grant{Principal: "group:auditors", Scope: access.Scope{Namespace: "hr"}, Deny: access.RunReadData, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01H at 2026-09-27T14:00:00Z: carol, an administrator, took themselves out of group:auditors, which holds a deny of run:read_data on hr, widening their own access"},
		{api.Notification{ID: "01J", Kind: "admin_access_widened", At: at, Act: "group_removed", By: "carol", Namespace: "hr",
			Grant: &access.Grant{Principal: "group:auditors", Scope: access.Scope{Namespace: "hr"}, Deny: access.RunReadData, GrantedBy: "frank", GrantedAt: at.Add(-time.Hour)}},
			"told 01J at 2026-09-27T14:00:00Z: carol, an administrator, removed group:auditors, which they were in and which held a deny of run:read_data on hr, widening their own access"},
		{api.Notification{ID: "01K", Kind: "admin_access_widened", At: at, Act: "renamed", By: "carol", Namespace: "hr",
			Grant: &access.Grant{Principal: "carol", Scope: access.Scope{Namespace: "hr"}, Role: access.Viewer}},
			"told 01K at 2026-09-27T14:00:00Z: admin_access_widened"},
		{api.Notification{ID: "01C", Kind: "passkey_counter_refused", At: at, Credential: "aVBob25l"},
			"told 01C at 2026-09-27T14:00:00Z: a sign-in with your passkey aVBob25l was refused because its signature counter did not move forward, as a copy of it would; remove it if the other copy is not yours"},
		{api.Notification{ID: "01E", Kind: "break_glass_recovery", At: at, Login: "carol"},
			"told 01E at 2026-09-27T14:00:00Z: agentiik-api recover, run on the installation's host, issued carol, an administrator, a recovery code"},
		{api.Notification{ID: "01L", Kind: "recovery_code_issued", At: at, By: "carol"},
			"told 01L at 2026-09-27T14:00:00Z: carol issued you a recovery code, which enrols a passkey or sets a password on your account for whoever holds it, within the hour; if you did not ask for one, tell another administrator"},
		{api.Notification{ID: "01M", Kind: "recovery_code_issued", At: at, By: "operator"},
			"told 01M at 2026-09-27T14:00:00Z: operator issued you a recovery code, which enrols a passkey or sets a password on your account for whoever holds it, within the hour; if you did not ask for one, tell another administrator"},
		{api.Notification{ID: "01N", Kind: "recovery_code_used", At: at, By: "carol", Credential: "bmV3UGFzc2tleQ"},
			"told 01N at 2026-09-27T14:00:00Z: a recovery code carol issued you enrolled bmV3UGFzc2tleQ on your account; if that was not you, remove it from your sign-in methods and tell another administrator"},
		{api.Notification{ID: "01P", Kind: "recovery_code_used", At: at, By: "carol"},
			"told 01P at 2026-09-27T14:00:00Z: recovery_code_used"},
	} {
		if got := noticeLine(c.told); got != c.want {
			t.Errorf("agk whoami says\n%s\nwant\n%s", got, c.want)
		}
	}
}
