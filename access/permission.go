package access

import "strings"

// Permission is one atom of what a principal may do.
//
// Atoms rather than roles, because a role is a name for a set of these and two installations
// will disagree about what a role should contain long before they disagree about whether seeing
// an envelope's contents is the same thing as seeing that a step ran. A route needs one atom, a
// role expands to a set of them, and a deny names one.
type Permission string

const (
	// WorkflowRead: "See the YAML, the resolved graph, the version history and the
	// schedule."
	WorkflowRead Permission = "workflow:read"

	// WorkflowRun: "Start a manual run, cancel it, replay it, approve or reject a waiting
	// run."
	WorkflowRun Permission = "workflow:run"

	// WorkflowWrite: "Register a new version, change triggers, rename, move between
	// namespaces the principal owns on both sides."
	WorkflowWrite Permission = "workflow:write"

	// WorkflowDelete: "Delete the workflow and its versions."
	WorkflowDelete Permission = "workflow:delete"

	// RunRead: "See run state, per-step state, timings and log lines."
	RunRead Permission = "run:read"

	// RunReadData: "See a run's inputs and envelope contents and download artifacts, not only
	// state and digests." It is separate from RunRead because the two are different questions,
	// and the page is explicit that nothing grants it implicitly: "No implicit run:read_data
	// anywhere."
	RunReadData Permission = "run:read_data"

	// SecretUse: "Let a step reference a namespace secret. Never allows reading its value." It
	// counts at namespace scope only, as SecretWrite does.
	SecretUse Permission = "secret:use"

	// SecretWrite: "Declare, change and delete the namespace's secrets, and write a builtin
	// value. Never allows reading one."
	//
	// An atom of its own rather than workflow:write, because a declaration is not part of a
	// workflow: it is what decides which credential a step is handed, for every workflow of the
	// namespace at once, and the one holding workflow:write on a single workflow has no business
	// pointing another's secret somewhere else. Reading the declarations needs workflow:read,
	// since a workflow names the secrets it uses and the declarations are where they live. "Both
	// count at namespace scope only, since a declaration serves every workflow of the
	// namespace", which Resolve holds.
	SecretWrite Permission = "secret:write"

	// GrantManage: "Grant and revoke access at this scope."
	GrantManage Permission = "grant:manage"
)

// nine is the vocabulary in the order the page lists it, and the order a Set numbers it in. It is
// an array rather than the exported slice so that nothing an importer does to Permissions can
// renumber a Set.
var nine = [...]Permission{
	WorkflowRead, WorkflowRun, WorkflowWrite, WorkflowDelete,
	RunRead, RunReadData, SecretUse, SecretWrite, GrantManage,
}

// Permissions are the nine, in the order the page lists them. A test holds this list to the page
// and to the wire's enumeration, because a permission invented here would be one nothing
// documents and one no role includes.
var Permissions = append([]Permission(nil), nine[:]...)

// Valid says whether this is one of the nine.
func (p Permission) Valid() bool { return p.bit() != 0 }

// bit is where p sits in a Set, and zero for a string that is not one of the nine, so that an
// unknown permission is one no Set can hold.
func (p Permission) bit() uint16 {
	for i, known := range nine {
		if p == known {
			return 1 << i
		}
	}
	return 0
}

// namespaceOnly are the atoms that "count at namespace scope only, since a declaration serves
// every workflow of the namespace": a grant on one workflow never gives them.
var namespaceOnly = SetOf(SecretUse, SecretWrite)

// Set is a set of permissions: what a role expands to, and what a principal holds at one scope.
// The zero Set holds nothing, and two Sets holding the same permissions are equal.
type Set struct {
	bits uint16
}

// SetOf is the set holding ps. A string that is not one of the nine adds nothing.
func SetOf(ps ...Permission) Set {
	var s Set
	for _, p := range ps {
		s.bits |= p.bit()
	}
	return s
}

// Has says whether the set holds p. It never holds a string that is not one of the nine.
func (s Set) Has(p Permission) bool {
	b := p.bit()
	return b != 0 && s.bits&b != 0
}

// Permissions lists what the set holds, in the order the page lists the nine.
func (s Set) Permissions() []Permission {
	var out []Permission
	for i, p := range nine {
		if s.bits&(1<<i) != 0 {
			out = append(out, p)
		}
	}
	return out
}

// String writes the set as the page writes permissions, comma separated in the page's order.
func (s Set) String() string {
	var names []string
	for _, p := range s.Permissions() {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

func (s Set) union(t Set) Set   { return Set{bits: s.bits | t.bits} }
func (s Set) without(t Set) Set { return Set{bits: s.bits &^ t.bits} }
