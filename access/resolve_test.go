package access_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
)

var (
	t0 = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)

	installation = access.Scope{}
	finance      = access.Scope{Namespace: "finance"}
	invoicing    = access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll      = access.Scope{Namespace: "finance", Workflow: "payroll"}
	hrInvoicing  = access.Scope{Namespace: "hr", Workflow: "monthly-invoicing"}

	alice = access.Principal{Ref: "alice", Groups: []string{"team-finance"}}
	carol = access.Principal{Ref: "carol", Groups: []string{"team-finance", "team-ops"}}
	bob   = access.Principal{Ref: "bob"}
)

// allow and deny write one grant row each, as the wire's accessGrant does.
func allow(id, who string, at access.Scope, r access.Role) access.Grant {
	return access.Grant{ID: id, Principal: who, Scope: at, Role: r, GrantedBy: "operator", GrantedAt: t0}
}

func deny(id, who string, at access.Scope, p access.Permission) access.Grant {
	return access.Grant{ID: id, Principal: who, Scope: at, Deny: p, GrantedBy: "operator", GrantedAt: t0}
}

func until(g access.Grant, end time.Time) access.Grant {
	g.ExpiresAt = end
	return g
}

// resolved is what who holds at one scope as of now, failing the test on an error.
func resolved(t *testing.T, who access.Principal, grants []access.Grant, at access.Scope, now time.Time) access.Set {
	t.Helper()
	held, err := access.Resolve(who, grants, at, now)
	if err != nil {
		t.Fatalf("resolving %s at %q: %v", who.Ref, at, err)
	}
	return held
}

// The documentation's own example, as its figure draws it: alice, a member of team-finance, holds
// viewer on the namespace finance through her group and operator on monthly-invoicing herself, and
// a deny on that workflow takes run:read_data from her. "Grants add up; a deny is the only thing
// that subtracts."
func TestAliceHoldsWhatTheFigureSays(t *testing.T) {
	grants := []access.Grant{
		allow("01JQ3M8T", "group:team-finance", finance, access.Viewer),
		allow("01JQ3M9A", "alice", invoicing, access.Operator),
		deny("01JQ3M9B", "alice", invoicing, access.RunReadData),
	}
	for _, c := range []struct {
		what string
		who  access.Principal
		at   access.Scope
		want []access.Permission
	}{
		{
			"alice on monthly-invoicing holds her group's viewer from the namespace and her own operator from the workflow",
			alice, invoicing,
			[]access.Permission{access.WorkflowRead, access.WorkflowRun, access.RunRead},
		},
		{
			"alice on the namespace holds only what the namespace gives, since a workflow's grant never reaches up",
			alice, finance,
			[]access.Permission{access.WorkflowRead, access.RunRead},
		},
		{
			"alice on another workflow of finance inherits the namespace's grant and not the other workflow's",
			alice, payroll,
			[]access.Permission{access.WorkflowRead, access.RunRead},
		},
		{
			"alice in another namespace holds nothing, even on a workflow of the same name",
			alice, hrInvoicing,
			nil,
		},
		{
			"carol, another member of team-finance, holds the group's grant and none of alice's own",
			carol, invoicing,
			[]access.Permission{access.WorkflowRead, access.RunRead},
		},
		{
			"bob, in no group, holds nothing",
			bob, invoicing,
			nil,
		},
		{
			"nobody holds anything at the installation, which no grant names",
			alice, installation,
			nil,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			got := resolved(t, c.who, grants, c.at, t0)
			if !slices.Equal(got.Permissions(), c.want) {
				t.Errorf("holds %s, want %s", got, access.SetOf(c.want...))
			}
		})
	}
}

// "A deny wins over any allow at any scope": a deny on the workflow takes run:read_data from an
// editor of the whole namespace there, and nowhere else.
func TestADenyWinsOverAnAllowFromAnyScope(t *testing.T) {
	grants := []access.Grant{
		allow("g1", "alice", finance, access.Editor),
		deny("d1", "alice", invoicing, access.RunReadData),
	}
	if got := resolved(t, alice, grants, invoicing, t0); got.Has(access.RunReadData) {
		t.Errorf("alice holds %s on monthly-invoicing, which denies her run:read_data", got)
	} else if !got.Has(access.WorkflowWrite) {
		t.Errorf("the deny took more than run:read_data: alice holds %s", got)
	}
	for _, at := range []access.Scope{finance, payroll} {
		if got := resolved(t, alice, grants, at, t0); !got.Has(access.RunReadData) {
			t.Errorf("a deny on monthly-invoicing took run:read_data from alice at %q", at)
		}
	}

	// A deny on the namespace wins over an allow on the workflow, which is the other way round.
	grants = []access.Grant{
		deny("d2", "alice", finance, access.WorkflowRun),
		allow("g2", "alice", invoicing, access.Owner),
	}
	if got := resolved(t, alice, grants, invoicing, t0); got.Has(access.WorkflowRun) || !got.Has(access.GrantManage) {
		t.Errorf("an owner of the workflow denied workflow:run on the namespace holds %s", got)
	}
}

// A deny written for a group reaches every member, and wins over what a member holds in their own
// name.
func TestADenyForAGroupReachesItsMembers(t *testing.T) {
	grants := []access.Grant{
		allow("g1", "alice", finance, access.Owner),
		deny("d1", "group:team-finance", finance, access.GrantManage),
	}
	if got := resolved(t, alice, grants, invoicing, t0); got.Has(access.GrantManage) {
		t.Errorf("alice holds grant:manage, which her group is denied: %s", got)
	}
	if got := resolved(t, bob, append(grants, allow("g2", "bob", finance, access.Owner)), finance, t0); !got.Has(access.GrantManage) {
		t.Errorf("a deny for team-finance reached bob, who is not in it: %s", got)
	}
}

// A principal may hold several roles at one scope, and holds their union: "someone who needs both
// holds both roles".
func TestSeveralRolesAtOneScopeAddUp(t *testing.T) {
	grants := []access.Grant{
		allow("g1", "alice", finance, access.Operator),
		allow("g2", "group:team-finance", finance, access.Viewer),
	}
	want := []access.Permission{access.WorkflowRead, access.WorkflowRun, access.RunRead}
	for _, at := range []access.Scope{finance, invoicing} {
		if got := resolved(t, alice, grants, at, t0); !slices.Equal(got.Permissions(), want) {
			t.Errorf("an operator and a viewer at %q holds %s", at, got)
		}
	}
}

// "A workflow-scope grant only adds": a narrower role on one workflow takes nothing away from what
// the namespace gives there.
func TestAWorkflowGrantOnlyAdds(t *testing.T) {
	grants := []access.Grant{
		allow("g1", "alice", finance, access.Editor),
		allow("g2", "alice", invoicing, access.Viewer),
	}
	if got := resolved(t, alice, grants, invoicing, t0); got != access.Editor.Permissions() {
		t.Errorf("an editor of the namespace and a viewer of the workflow holds %s there", got)
	}
}

// secret:use and secret:write "count at namespace scope only, since a declaration serves every
// workflow of the namespace": an owner of one workflow holds neither, there or on the namespace,
// while an editor of the namespace holds both on every workflow in it.
func TestSecretPermissionsComeFromTheNamespaceAlone(t *testing.T) {
	grants := []access.Grant{allow("g1", "alice", invoicing, access.Owner)}
	got := resolved(t, alice, grants, invoicing, t0)
	if got.Has(access.SecretUse) || got.Has(access.SecretWrite) {
		t.Errorf("an owner of one workflow holds %s there", got)
	}
	rest := access.SetOf(
		access.WorkflowRead, access.WorkflowRun, access.WorkflowWrite, access.WorkflowDelete,
		access.RunRead, access.RunReadData, access.GrantManage,
	)
	if got != rest {
		t.Errorf("an owner of one workflow holds %s there, and should hold the rest of the role, %s", got, rest)
	}
	if got := resolved(t, alice, grants, finance, t0); got != (access.Set{}) {
		t.Errorf("an owner of one workflow holds %s on the namespace", got)
	}

	grants = []access.Grant{allow("g2", "alice", finance, access.Editor)}
	for _, at := range []access.Scope{finance, invoicing} {
		if got := resolved(t, alice, grants, at, t0); !got.Has(access.SecretUse) || !got.Has(access.SecretWrite) {
			t.Errorf("an editor of the namespace holds %s at %q", got, at)
		}
	}

	// A deny of either on one workflow still wins there, and only there.
	grants = append(grants, deny("d1", "alice", invoicing, access.SecretWrite))
	if got := resolved(t, alice, grants, invoicing, t0); got.Has(access.SecretWrite) || !got.Has(access.SecretUse) {
		t.Errorf("an editor denied secret:write on the workflow holds %s there", got)
	}
	if got := resolved(t, alice, grants, finance, t0); !got.Has(access.SecretWrite) {
		t.Errorf("a deny on the workflow took secret:write from the namespace: %s", got)
	}
}

// "An optional expiry ends it without anyone remembering to revoke it", and it ends at the instant
// it names. An expiring deny stops denying the same way.
func TestAGrantLapsesAtItsExpiry(t *testing.T) {
	end := t0.Add(72 * time.Hour)
	grants := []access.Grant{
		until(allow("g1", "group:team-finance", finance, access.Viewer), end),
		allow("g2", "alice", invoicing, access.Operator),
		until(deny("d1", "alice", invoicing, access.WorkflowRun), end),
	}
	for _, c := range []struct {
		what string
		now  time.Time
		want []access.Permission
	}{
		{"a second before the expiry, the viewer grant holds and the deny denies", end.Add(-time.Second), []access.Permission{access.WorkflowRead, access.RunRead}},
		{"at the expiry, both have ended", end, []access.Permission{access.WorkflowRun}},
		{"a year after, both are still ended and the grant with no expiry still holds", end.AddDate(1, 0, 0), []access.Permission{access.WorkflowRun}},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := resolved(t, alice, grants, invoicing, c.now); !slices.Equal(got.Permissions(), c.want) {
				t.Errorf("holds %s, want %s", got, access.SetOf(c.want...))
			}
		})
	}

	g := until(allow("g3", "alice", finance, access.Viewer), end)
	if g.Expired(end.Add(-time.Nanosecond)) || !g.Expired(end) {
		t.Error("a grant ends at another instant than the one it names")
	}
	if (access.Grant{}).Expired(end.AddDate(100, 0, 0)) {
		t.Error("a grant with no expiry has ended")
	}
}

// A row that applies and cannot be read makes the question unanswerable rather than being passed
// over, since a deny nobody can read would otherwise deny nothing. A row that does not apply is not
// this question's to read.
func TestAGrantThatCannotBeReadIsAnError(t *testing.T) {
	good := allow("g1", "alice", finance, access.Owner)
	for _, c := range []struct {
		what string
		bad  access.Grant
	}{
		{"a deny naming a permission that does not exist", deny("d1", "alice", invoicing, "run:read-data")},
		{"a deny naming a role", deny("d2", "group:team-finance", finance, "viewer")},
		{"a role that does not exist", allow("g2", "alice", finance, "admin")},
		{"a row with both a role and a deny", access.Grant{ID: "g3", Principal: "alice", Scope: finance, Role: access.Viewer, Deny: access.RunReadData}},
		{"a row with neither", access.Grant{ID: "g4", Principal: "alice", Scope: finance}},
	} {
		t.Run(c.what, func(t *testing.T) {
			held, err := access.Resolve(alice, []access.Grant{good, c.bad}, invoicing, t0)
			if err == nil {
				t.Fatalf("resolved to %s", held)
			}
			if held != (access.Set{}) {
				t.Errorf("an error came with %s", held)
			}
			if !strings.Contains(err.Error(), c.bad.ID) {
				t.Errorf("the error does not name the grant: %v", err)
			}
			if ok, err := access.Holds(alice, []access.Grant{good, c.bad}, access.WorkflowRead, invoicing, t0); ok || err == nil {
				t.Errorf("Holds answered %v, %v", ok, err)
			}

			// The same row for someone else, on another workflow, or expired, is not read.
			for _, g := range []access.Grant{
				func() access.Grant { g := c.bad; g.Principal = "bob"; return g }(),
				func() access.Grant { g := c.bad; g.Scope = payroll; return g }(),
				until(c.bad, t0),
			} {
				if _, err := access.Resolve(alice, []access.Grant{good, g}, invoicing, t0); err != nil {
					t.Errorf("a row that does not apply was read: %v", err)
				}
			}
		})
	}
}

// The installation is no scope a grant names, so nothing resolves there, not even a row read with
// no scope at all.
func TestNothingIsHeldAtTheInstallation(t *testing.T) {
	grants := []access.Grant{
		allow("g1", "alice", installation, access.Owner),
		allow("g2", "alice", finance, access.Owner),
	}
	if got := resolved(t, alice, grants, installation, t0); got != (access.Set{}) {
		t.Errorf("alice holds %s at the installation", got)
	}
	if got := resolved(t, alice, grants, finance, t0); got != access.Owner.Permissions() {
		t.Errorf("alice holds %s on finance, and a row with no scope reached it", got)
	}
}

// A grant names a principal exactly: a login, or group:NAME for a group the principal is a member
// of. Nothing matches by prefix, by case, or by the empty string.
func TestAGrantNamesItsPrincipalExactly(t *testing.T) {
	for _, c := range []struct {
		who  access.Principal
		ref  string
		want bool
	}{
		{alice, "alice", true},
		{alice, "group:team-finance", true},
		{alice, "Alice", false},
		{alice, "alice ", false},
		{alice, "team-finance", false},
		{alice, "group:team-ops", false},
		{alice, "group:", false},
		{access.Principal{Ref: "finance/nightly-sync"}, "finance/nightly-sync", true},
		{access.Principal{Ref: "finance/nightly-sync"}, "hr/nightly-sync", false},
		{access.Principal{}, "", false},
		{access.Principal{Groups: []string{""}}, "group:", false},
	} {
		grants := []access.Grant{{ID: "g", Principal: c.ref, Scope: finance, Role: access.Viewer}}
		ok, err := access.Holds(c.who, grants, access.WorkflowRead, finance, t0)
		if err != nil {
			t.Fatal(err)
		}
		if ok != c.want {
			t.Errorf("a grant to %q applies to %+v: %v", c.ref, c.who, ok)
		}
	}
}

// The same grants resolve the same way whatever order they are read in, since a union and a
// difference do not care.
func TestTheOrderOfTheGrantsChangesNothing(t *testing.T) {
	grants := []access.Grant{
		deny("d1", "alice", invoicing, access.WorkflowWrite),
		allow("g1", "group:team-finance", finance, access.Editor),
		allow("g2", "alice", invoicing, access.Owner),
		deny("d2", "group:team-finance", finance, access.RunReadData),
	}
	want := resolved(t, alice, grants, invoicing, t0)
	slices.Reverse(grants)
	if got := resolved(t, alice, grants, invoicing, t0); got != want {
		t.Errorf("reversed, the grants resolve to %s rather than %s", got, want)
	}
	if want.Has(access.WorkflowWrite) || want.Has(access.RunReadData) || !want.Has(access.GrantManage) {
		t.Errorf("the grants resolve to %s", want)
	}
}
