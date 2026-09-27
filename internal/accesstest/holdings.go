package accesstest

import (
	"slices"
	"strings"
)

// What each asker holds, written here by hand from the documentation's rules rather than computed by
// package access, so that what the installation resolves is held to the page and never to itself.

// The four roles, as the page's table of roles writes them: a role holds every permission of each
// column it says yes to.
var (
	viewer   = []string{"workflow:read", "run:read"}
	operator = []string{"workflow:run", "run:read"}
	editor   = []string{"workflow:read", "workflow:run", "workflow:write", "run:read", "run:read_data", "secret:use", "secret:write"}
	owner    = []string{"workflow:read", "workflow:run", "workflow:write", "workflow:delete", "run:read", "run:read_data", "secret:use", "secret:write", "grant:manage"}
)

// Holding is what an asker holds, as GET /api/v1/me answers it: whether it administers the
// installation through the credential it presents, its groups, and its permissions at each scope
// where it holds any, a workflow's only where they are other than its namespace's.
type Holding struct {
	Admin       bool
	Groups      []string
	Permissions map[string][]string

	// Owns are the namespaces it owns through its credential, which the routes about service
	// accounts and their tokens answer it about: an unexpired owner grant on the namespace, and a
	// credential narrowing nothing, since a narrowed token owns none.
	Owns []string

	// Opens is whether its credential opens anything: nobody's does not, and neither does a token
	// past its expiry.
	Opens bool
}

// Holds is what an asker holds before the lapse, or from it where lapsed is set.
//
// Each row is the page's rules applied to the fixture's grants. "Effective permissions are the
// union of every applying grant: the principal's own and its groups', at both scopes." "A
// workflow-scope grant only adds. Only an explicit deny removes, and it wins over any allow at any
// scope." "secret:use and secret:write come from namespace grants alone." "A grant or a deny ends at
// the instant its expiry names." And an administrator "holds what their grants give, as anybody
// does", through a token with no scope alone.
func (f *Fixture) Holds(as Asker, lapsed bool) Holding {
	switch as.holding {
	case "carol":
		// finance's record names carol its owner, and her personal namespace is hers: nothing in
		// hr, where she holds no grant, although she administers the installation.
		return Holding{Admin: true, Opens: true, Owns: []string{"carol", Finance}, Permissions: map[string][]string{
			"carol": owner, Finance: owner,
		}}
	case "carol for finance":
		// A token narrowed to finance holds there what carol holds, and carries none of an
		// administrator's powers, which pass through a token only where its scope does not
		// narrow them away.
		return Holding{Opens: true, Permissions: map[string][]string{Finance: owner}}
	case "alice", "alice until the lapse":
		if as.holding == "alice until the lapse" && lapsed {
			return Holding{}
		}
		// The page's figure on monthly-invoicing: team-finance's editor, inherited by every
		// workflow of finance, less run:read_data, which the deny takes from alice alone. On
		// onboarding, her own editor on the workflow, less secret:use and secret:write, which a
		// workflow's grant never gives, less workflow:run, which team-finance's deny on hr takes
		// at namespace scope from every member, and less workflow:read until the lapse. hr itself
		// she holds nothing on: a deny alone gives nothing.
		return Holding{Opens: true, Groups: []string{"group:" + TeamFinance}, Owns: []string{"alice"}, Permissions: map[string][]string{
			"alice":                   owner,
			Finance:                   editor,
			Finance + "/" + Invoicing: without(editor, "run:read_data"),
			HR + "/" + Onboarding:     onboarding(lapsed),
		}}
	case "alice for monthly-invoicing":
		// Narrowed to two permissions on one workflow: what alice holds there, kept to those.
		return Holding{Opens: true, Groups: []string{"group:" + TeamFinance}, Permissions: map[string][]string{
			Finance + "/" + Invoicing: {"workflow:read", "run:read"},
		}}
	case "alice for hr":
		return Holding{Opens: true, Groups: []string{"group:" + TeamFinance}, Permissions: map[string][]string{
			HR + "/" + Onboarding: onboarding(lapsed),
		}}
	case "bob":
		return Holding{Opens: true, Owns: []string{"bob", HR}, Permissions: map[string][]string{"bob": owner, HR: owner}}
	case NightlySync:
		// An operator of payroll, which follows the runs of what it may run and reads none of
		// the workflows, and a viewer of finance until the lapse, whose grant adds workflow:read
		// on payroll while it lasts. A service account belongs to no group.
		if lapsed {
			return Holding{Opens: true, Permissions: map[string][]string{Finance + "/" + Payroll: operator}}
		}
		return Holding{Opens: true, Permissions: map[string][]string{
			Finance: viewer, Finance + "/" + Payroll: union(viewer, operator),
		}}
	}
	return Holding{}
}

// onboarding is what alice holds on hr/onboarding.
func onboarding(lapsed bool) []string {
	held := without(editor, "secret:use", "secret:write", "workflow:run")
	if !lapsed {
		held = without(held, "workflow:read")
	}
	return held
}

// Hold says whether h holds what at a namespace, or at a workflow of it where workflow is not empty:
// a workflow's own permissions where h names them, and its namespace's otherwise.
func (h Holding) Hold(what, namespace, workflow string) bool {
	held, ok := h.Permissions[namespace+"/"+workflow]
	if workflow == "" || !ok {
		held = h.Permissions[namespace]
	}
	return slices.Contains(held, what)
}

// Sees says whether h is shown a namespace's record: everyone of them to an administrator, and
// otherwise one where h holds a grant carrying a role, which in the fixture is one where it holds
// anything, since no deny there takes every permission a role gives.
func (h Holding) Sees(namespace string) bool {
	if h.Admin {
		return true
	}
	for at := range h.Permissions {
		if ns, _, _ := strings.Cut(at, "/"); ns == namespace {
			return true
		}
	}
	return false
}

// without is set less the permissions named.
func without(set []string, less ...string) []string {
	return slices.DeleteFunc(slices.Clone(set), func(p string) bool { return slices.Contains(less, p) })
}

// union is every permission of either set, in the order the page lists the nine.
func union(a, b []string) []string {
	var out []string
	for _, p := range owner {
		if slices.Contains(a, p) || slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return out
}
