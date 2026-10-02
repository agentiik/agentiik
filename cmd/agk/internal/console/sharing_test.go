package console

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

var (
	three = tea.KeyPressMsg{Code: '3', Text: "3"}
	sKey  = tea.KeyPressMsg{Code: 's', Text: "s"}
)

// chloe owns finance, which is what lets her list its grants and its workflows'.
var chloe = &principal{Principal: "chloe", Permissions: map[string][]string{"finance": {"workflow:read", "workflow:run", "workflow:write", "workflow:delete", "run:read", "run:read_data", "secret:use", "secret:write", "grant:manage"}}}

const (
	invoicingGrants = "/api/v1/finance/workflows/monthly-invoicing/grants"
	financeGrants   = "/api/v1/finance/grants"
)

// sharedFinance is the documentation's example: three grants on finance, which monthly-invoicing
// inherits, and four on the workflow, a deny of run:read_data to alice among them.
func sharedFinance(me *principal) *installation {
	grant := func(id, principal, scope string, role access.Role, deny access.Permission) access.Grant {
		at, _ := access.ParseScope(scope)
		return access.Grant{ID: id, Principal: principal, Scope: at, Role: role, Deny: deny, GrantedBy: "chloe", GrantedAt: now.Add(-48 * time.Hour)}
	}
	onFinance := []access.Grant{
		grant("01GRANTTEAMFINANCE00000000", "group:team-finance", "finance", access.Viewer, ""),
		grant("01GRANTDEPLOYBOT0000000000", "finance/deploy-bot", "finance", access.Operator, ""),
		grant("01GRANTCHLOE0000000000000A", "chloe", "finance", access.Owner, ""),
	}
	until := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	alice := grant("01GRANTALICE0000000000000A", "alice", "finance/monthly-invoicing", access.Editor, "")
	alice.ExpiresAt = &until
	onInvoicing := append(slices.Clone(onFinance),
		alice,
		grant("01GRANTALICEDENY000000000A", "alice", "finance/monthly-invoicing", "", access.RunReadData),
		grant("01GRANTBRUNO0000000000000A", "bruno", "finance/monthly-invoicing", access.Editor, ""),
		grant("01GRANTTEAMOPS00000000000A", "group:team-ops", "finance/monthly-invoicing", access.Viewer, ""),
	)
	return &installation{
		runs:   someRuns(),
		me:     me,
		grants: map[string][]access.Grant{financeGrants: onFinance, invoicingGrants: onInvoicing},
		groups: []api.Group{{Name: "team-finance", Members: []string{"alice", "bruno"}}, {Name: "team-ops", Members: []string{"dana"}}},
	}
}

// 3 opens the sharing of the workflow in front of the principal, the run's selected, as written:
// each grant with its principal, what it gives, where it was written, those the workflow inherits
// said to be, and its expiry; then what each role carries; and resolved for the caller, every
// permission an owner's namespace grant gives, secret:use and secret:write among them.
func TestTheSharingViewListsTheGrantsAsWrittenAndResolvesThem(t *testing.T) {
	in := sharedFinance(chloe)
	m := press(t, opened(t, in, Options{}, 160, 44), three)
	if m.view != sharingView || m.scope != "finance/monthly-invoicing" || !slices.Contains(in.asked, invoicingGrants) {
		t.Fatalf("3 leaves view %d on %q, having read %v", m.view, m.scope, in.asked)
	}
	s := screen(m)
	lines := strings.Split(s, "\n")
	if !strings.Contains(lines[0], "1 Runs   2 Workflows   3 Sharing") {
		t.Errorf("the top line does not name the sharing: %s", lines[0])
	}
	for _, want := range []string{
		"Grants as written on finance/monthly-invoicing", "7 grants · 3 from the namespace · 1 deny",
		"PRINCIPAL", "ROLE OR DENY", "SCOPE", "EXPIRES",
		"group:team-finance     viewer              finance inherited",
		"finance/deploy-bot     operator            finance inherited",
		"alice                  editor              finance/monthly-invoicing",
		"2026-12-31",
		"deny run:read_data",
		"What each role carries",
		"owner     ●       ●       ●       ●       ●       ●       ●",
		"operator  ·       ●       ·       ·       ·       ·       ·",
		"Never shown, to anyone",
		"read only: agk share and the web console change grants",
		"Resolved for chloe (you)", "user",
		"+ workflow:read     finance: owner",
		"+ secret:write      finance: owner",
		"+ grant:manage      finance: owner",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the sharing view does not show %q:\n%s", want, s)
		}
	}
	if last := strings.TrimRight(lines[43], " "); last != "↑↓ Grant   enter Resolve for…   s Scope   / Filter   : Commands   esc Runs   q Quit   ? Every key" {
		t.Errorf("the key line is %q", last)
	}
	for i, l := range lines {
		if w := len([]rune(l)); w != 160 {
			t.Errorf("line %d is %d columns wide in a window of 160: %q", i, w, l)
		}
	}
}

// enter resolves the grants for the principal of the grant chosen: alice holds what her editor
// grant on the workflow gives, run:read_data taken away by the deny, and neither secret
// permission, which a grant on one workflow never gives. Whether she is in team-finance is an
// administrator's to read, so its viewer grant is said to give to a member.
func TestTheGrantsAreResolvedForOnePrincipalADenyTheOnlySubtraction(t *testing.T) {
	in := sharedFinance(chloe)
	m := press(t, opened(t, in, Options{}, 160, 44), three, down, down, down, enter)
	if m.palette == nil || m.palette.prompt != "resolve for: " {
		t.Fatalf("enter does not open the chooser of whom to resolve for")
	}
	if l := m.listed(); m.palette.chosen >= len(l) || l[m.palette.chosen].label != "alice" {
		t.Errorf("the chooser does not open on the principal of the grant chosen: %d of %v", m.palette.chosen, l)
	}
	m = press(t, m, enter)
	if m.whomOf() != "alice" {
		t.Fatalf("the grants are resolved for %q", m.whomOf())
	}
	s := screen(m)
	for _, want := range []string{
		"Resolved for alice", "user · groups an administrator's to read",
		"+ workflow:read     finance/monthly-invoicing: editor",
		"+ workflow:run      finance/monthly-invoicing: editor",
		"− run:read_data     denied at finance/monthly-invoicing",
		"editor holds it; the deny wins",
		"  secret:use        from namespace grants alone",
		"  workflow:delete   the owner's",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the grants resolved for alice do not show %q:\n%s", want, s)
		}
	}
	if len(in.sent) != 0 {
		t.Errorf("resolving sent %v: the sharing view changes nothing", in.sent)
	}
}

// Where the principal's groups are an administrator's to read, a group's grants are said to give
// what they give to a member, and a deny with nothing to take away is said still.
func TestAGroupsGrantsAreSaidToGiveToAMember(t *testing.T) {
	in := sharedFinance(chloe)
	erin, _ := access.ParseScope("finance/monthly-invoicing")
	in.grants[invoicingGrants] = append(in.grants[invoicingGrants], access.Grant{ID: "01GRANTERINDENY0000000000A", Principal: "erin", Scope: erin, Deny: access.WorkflowRun, GrantedBy: "chloe", GrantedAt: now})
	m := press(t, opened(t, in, Options{}, 160, 44), three)
	m.whom = "erin"
	s := screen(m)
	for _, want := range []string{
		"Resolved for erin",
		"  workflow:read     to a member: finance: group:team-finance, viewer",
		"− workflow:run      denied at finance/monthly-invoicing",
		"Who is in a group is an administrator's to read",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the grants resolved for erin do not show %q:\n%s", want, s)
		}
	}
}

// An administrator reads who is in each group, so a group's grants are said to be the principal's
// own where it is a member.
func TestAnAdministratorSeesTheGroupsGrantsAsAMembers(t *testing.T) {
	admin := &principal{Principal: "dana", Admin: true, Permissions: chloe.Permissions}
	in := sharedFinance(admin)
	m := press(t, opened(t, in, Options{}, 160, 44), three)
	m.whom = "alice"
	s := screen(m)
	for _, want := range []string{
		"Resolved for alice", "user · member of group:team-finance",
		"+ workflow:read     finance: group:team-finance, viewer",
		"                    finance/monthly-invoicing: editor",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the grants resolved by an administrator do not show %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "to a member") || !slices.Contains(in.asked, "/api/v1/groups") {
		t.Errorf("an administrator is not told which groups alice is in, having read %v:\n%s", in.asked, s)
	}
}

// What the view says each permission comes to is what access.Resolve answers, for every principal
// the grants name, at both scopes.
func TestTheResolutionIsAccessResolves(t *testing.T) {
	in := sharedFinance(&principal{Principal: "dana", Admin: true, Permissions: chloe.Permissions})
	m := press(t, opened(t, in, Options{}, 160, 44), three)
	for _, scope := range []string{"finance", "finance/monthly-invoicing"} {
		m.scope, m.grants = scope, in.grants[grantsPath(scope)]
		for _, whom := range m.principalsOf() {
			groups, _ := m.groupsOf(whom)
			want, err := access.Resolve(access.Principal{Ref: whom, Groups: groups}, m.grants, scopeOf(scope), now)
			if err != nil {
				t.Fatal(err)
			}
			lines, err := m.resolved(whom, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range lines {
				if l.held != want.Has(l.permission) || l.held != (len(l.gives) > 0 && len(l.takes) == 0) {
					t.Errorf("%s on %s: %s is said held %t, with %d giving and %d taking, where access.Resolve answers %t", whom, scope, l.permission, l.held, len(l.gives), len(l.takes), want.Has(l.permission))
				}
			}
		}
	}
}

// s chooses among the scopes the principal may list the grants of, and the namespace's are listed
// with nothing inherited.
func TestSChoosesTheScope(t *testing.T) {
	in := sharedFinance(chloe)
	m := press(t, opened(t, in, Options{}, 120, 40), three, sKey)
	if m.palette == nil || m.palette.prompt != "scope: " {
		t.Fatal("s does not open the chooser of scopes")
	}
	var labels []string
	for _, c := range m.listed() {
		labels = append(labels, c.label+" "+c.kind)
	}
	if want := []string{"finance namespace", "finance/monthly-invoicing workflow", "finance/nightly-export workflow"}; !slices.Equal(labels, want) {
		t.Errorf("the scopes offered are %v, not %v", labels, want)
	}
	m = press(t, m, up, enter)
	if m.scope != "finance" || !slices.Contains(in.asked, financeGrants) {
		t.Fatalf("choosing finance shows %q, having read %v", m.scope, in.asked)
	}
	s := screen(m)
	for _, want := range []string{"Grants as written on finance", "3 grants", "Resolved for chloe (you)"} {
		if !strings.Contains(s, want) {
			t.Errorf("the namespace's sharing does not show %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "inherited") || strings.Contains(s, "from the namespace") {
		t.Errorf("a namespace's grants are said to be inherited:\n%s", s)
	}
	for i, l := range strings.Split(s, "\n") {
		if w := len([]rune(l)); w != 120 {
			t.Errorf("line %d is %d columns wide in a window of 120: %q", i, w, l)
		}
	}
}

// A principal holding grant:manage nowhere is told why nothing is listed, and no grants route is
// asked: it would be refused.
func TestSharingSaysWhyNothingIsListedWithoutGrantManage(t *testing.T) {
	in := sharedFinance(&principal{Principal: "alice", Permissions: map[string][]string{"finance": {"run:read"}}})
	m := press(t, opened(t, in, Options{}, 100, 30), three)
	if s := screen(m); m.view != sharingView || !strings.Contains(s, "you hold it nowhere") {
		t.Errorf("a principal managing nothing sees:\n%s", s)
	}
	for _, path := range in.asked {
		if strings.HasSuffix(path, "/grants") {
			t.Errorf("the console asked %s of a principal holding grant:manage nowhere", path)
		}
	}
}

// The grants are listed one above the other in an 80 by 24 window, every line within it, the
// roles' table left out for the room.
func TestTheSharingViewFitsTheLeastWindow(t *testing.T) {
	m := press(t, opened(t, sharedFinance(chloe), Options{}, 80, 24), three)
	s := screen(m)
	lines := strings.Split(s, "\n")
	if len(lines) != 24 {
		t.Fatalf("the screen is %d lines high in a window of 24:\n%s", len(lines), s)
	}
	for i, l := range lines {
		if w := len([]rune(l)); w != 80 {
			t.Errorf("line %d is %d columns wide in a window of 80: %q", i, w, l)
		}
	}
	if !strings.Contains(s, "Grants as written on finance/monthly-invoicing") {
		t.Errorf("the least window does not show the grants:\n%s", s)
	}
}

// / narrows the grants listed as you type, and leaves the resolution reading them all; esc clears
// the filter before it leaves the view.
func TestSlashFiltersTheGrantsAndNotTheResolution(t *testing.T) {
	in := sharedFinance(chloe)
	m := press(t, opened(t, in, Options{}, 160, 44), three, tea.KeyPressMsg{Code: '/', Text: "/"})
	for _, r := range "alice" {
		m = press(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := len(m.shownGrants()); got != 2 {
		t.Errorf("alice leaves %d grants, not her 2", got)
	}
	s := screen(m)
	for _, want := range []string{"/ alice▏", "2 of 7 grants", "deny run:read_data", "+ grant:manage      finance: owner"} {
		if !strings.Contains(s, want) {
			t.Errorf("the grants filtered for alice do not show %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "bruno") || strings.Contains(s, "group:team-ops") {
		t.Errorf("the filter leaves grants it does not match:\n%s", s)
	}
	m = press(t, m, enter, down, enter)
	if l := m.listed(); m.palette == nil || l[m.palette.chosen].label != "alice" {
		t.Fatalf("enter on the grants filtered does not offer alice first")
	}
	m = press(t, m, esc, esc)
	if m.view != sharingView || m.grantsFilter != "" {
		t.Errorf("esc leaves view %d with the filter %q, where it clears the filter first", m.view, m.grantsFilter)
	}
	if m = press(t, m, esc); m.view != runsView {
		t.Errorf("esc with no filter leaves view %d", m.view)
	}
}
