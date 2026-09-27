package access_test

import (
	"testing"

	"github.com/agentiik/agentiik/access"
)

// The page's own example, "a token that only starts one workflow": { "scope": { "permissions":
// ["workflow:run"], "within": ["finance/monthly-invoicing"] } }. It keeps workflow:run on that
// workflow and nothing else: not another permission there, not the same permission on another
// workflow or at the namespace, and not the installation.
func TestATokenThatOnlyStartsOneWorkflowKeepsThatAndNothingElse(t *testing.T) {
	s, err := access.ParseTokenScope([]string{"workflow:run"}, []string{"finance/monthly-invoicing"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Keeps(access.WorkflowRun, invoicing) {
		t.Error("the token does not keep workflow:run on the one workflow it names")
	}
	for _, c := range []struct {
		what access.Permission
		at   access.Scope
	}{
		{access.RunRead, invoicing},
		{access.WorkflowRun, payroll},
		{access.WorkflowRun, finance},
		{access.WorkflowRun, hrInvoicing},
		{access.GrantManage, installation},
		{access.WorkflowRun, installation},
	} {
		if s.Keeps(c.what, c.at) {
			t.Errorf("the token keeps %s at %q", c.what, c.at)
		}
	}
}

// Each half narrows alone where the other is absent: permissions everywhere its principal reaches,
// and every permission within what it reaches, a namespace reaching each of its workflows as a
// grant on it does.
func TestEachHalfOfAScopeNarrowsAlone(t *testing.T) {
	reading, err := access.ParseTokenScope([]string{"workflow:read", "run:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []access.Scope{finance, invoicing, hrInvoicing} {
		if !reading.Keeps(access.RunRead, at) || reading.Keeps(access.RunReadData, at) {
			t.Errorf("a token keeping two permissions answers otherwise at %q", at)
		}
	}
	// "A permissions list keeps only the permissions it names, and administering is none of the
	// nine": a list naming grant:manage administers nothing either.
	if manages, _ := access.ParseTokenScope([]string{"grant:manage"}, nil); manages.Keeps(access.GrantManage, installation) || reading.Keeps(access.GrantManage, installation) {
		t.Error("a token keeping a list of permissions keeps something at the installation")
	}

	inFinance, err := access.ParseTokenScope(nil, []string{"finance"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range access.Permissions {
		if !inFinance.Keeps(p, finance) || !inFinance.Keeps(p, payroll) {
			t.Errorf("a token within finance does not keep %s in it", p)
		}
		if inFinance.Keeps(p, hrInvoicing) || inFinance.Keeps(p, installation) {
			t.Errorf("a token within finance keeps %s outside it", p)
		}
	}
}

// No scope narrows nothing, which is what a token minted without one is.
func TestNoScopeNarrowsNothing(t *testing.T) {
	var none access.TokenScope
	for _, p := range access.Permissions {
		for _, at := range []access.Scope{installation, finance, invoicing} {
			if !none.Keeps(p, at) {
				t.Errorf("no scope does not keep %s at %q", p, at)
			}
		}
	}
	if parsed, err := access.ParseTokenScope(nil, nil); err != nil || parsed.Permissions != nil || parsed.Within != nil {
		t.Errorf("no halves read as %+v: %v", parsed, err)
	}
}

// A half present and empty keeps nothing, rather than reading as absent: the store refuses one,
// and a scope that could be read either way is read the way that narrows.
func TestAnEmptyHalfKeepsNothing(t *testing.T) {
	for _, s := range []struct {
		permissions, within []string
	}{{[]string{}, nil}, {nil, []string{}}} {
		parsed, err := access.ParseTokenScope(s.permissions, s.within)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Keeps(access.RunRead, finance) {
			t.Errorf("a scope of %q and %q keeps run:read in finance", s.permissions, s.within)
		}
	}
}

// A narrowing that does not read is refused rather than passed over, since passing over it would
// widen the token.
func TestAScopeThatDoesNotReadIsRefused(t *testing.T) {
	for _, s := range []struct {
		permissions, within []string
	}{
		{[]string{"workflow:everything"}, nil},
		{[]string{"owner"}, nil},
		{nil, []string{"Finance"}},
		{nil, []string{"runs"}},
		{nil, []string{"finance/"}},
		{nil, []string{""}},
	} {
		if parsed, err := access.ParseTokenScope(s.permissions, s.within); err == nil {
			t.Errorf("a scope of %q and %q read as %+v", s.permissions, s.within, parsed)
		}
	}
}

// A token reaches a namespace's record through the namespace or any workflow of it, whatever
// permissions it keeps, since reading the record is none of them; and no token reaches the record
// of no namespace.
func TestATokenReachesTheNamespacesItsWithinNames(t *testing.T) {
	for _, c := range []struct {
		permissions, within []string
		finance, hr         bool
	}{
		{nil, nil, true, true},
		{[]string{"workflow:run"}, nil, true, true},
		{nil, []string{"finance"}, true, false},
		{[]string{"workflow:run"}, []string{"finance/monthly-invoicing"}, true, false},
		{nil, []string{"hr/onboarding", "finance/payroll"}, true, true},
	} {
		s, err := access.ParseTokenScope(c.permissions, c.within)
		if err != nil {
			t.Fatal(err)
		}
		if s.Reaches("finance") != c.finance || s.Reaches("hr") != c.hr {
			t.Errorf("a token keeping %v within %v reaches finance %v and hr %v, want %v and %v",
				c.permissions, c.within, s.Reaches("finance"), s.Reaches("hr"), c.finance, c.hr)
		}
		if s.Reaches("") {
			t.Errorf("a token keeping %v within %v reaches the record of no namespace", c.permissions, c.within)
		}
	}
}
