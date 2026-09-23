package api

import (
	"fmt"
	"slices"
)

// Permission is one atom of what a principal may do.
//
// "Permissions are atoms, the union is recomputed per request, a deny wins at any scope."
// Atoms rather than roles, because a role is a name for a set of these and two installations
// will disagree about what a role should contain long before they disagree about whether seeing
// an envelope's contents is the same thing as seeing that a step ran.
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

	// RunReadData: "See envelope contents and download artifacts, not only state and
	// digests." It is separate from RunRead because the two are different questions, and
	// the page is explicit that nothing grants it implicitly: "No implicit run:read_data
	// anywhere."
	RunReadData Permission = "run:read_data"

	// SecretUse: "Let a step reference a namespace secret. Never allows reading its value."
	SecretUse Permission = "secret:use"

	// SecretWrite declares, moves and removes a namespace's secrets: which store holds each
	// value and where in it. Never allows reading a value.
	//
	// An atom of its own rather than workflow:write, because a declaration is not part of a
	// workflow: it is what decides which credential a step is handed, for every workflow of the
	// namespace at once, and the one holding workflow:write on a single workflow has no business
	// pointing another's secret somewhere else. Reading the declarations needs workflow:read,
	// since a workflow names the secrets it uses and the declarations are where they live.
	SecretWrite Permission = "secret:write"

	// GrantManage: "Grant and revoke access at this scope."
	GrantManage Permission = "grant:manage"
)

// Permissions are the nine, in the order the page lists them. A test holds this list to the
// page, because a permission invented here would be one nothing documents and one no role
// includes.
var Permissions = []Permission{
	WorkflowRead, WorkflowRun, WorkflowWrite, WorkflowDelete,
	RunRead, RunReadData, SecretUse, SecretWrite, GrantManage,
}

// Valid says whether this is one of the nine.
func (p Permission) Valid() bool {
	for _, known := range Permissions {
		if p == known {
			return true
		}
	}
	return false
}

// Role is a name for a set of permissions, and the set is fixed.
//
// "viewer, operator and editor are the read, run and edit roles; owner exists because someone has
// to be able to share." A grant binds a principal to one of these at one scope, and what the
// principal may then do is the union of the sets its grants name, less what a deny takes away.
type Role string

const (
	Viewer   Role = "viewer"
	Operator Role = "operator"
	Editor   Role = "editor"
	Owner    Role = "owner"
)

// Roles are the four, in the order the page lists them.
var Roles = []Role{Viewer, Operator, Editor, Owner}

// bundles are what each role holds.
//
// The page writes a role as five columns, read, run, write, data and grant, and its own figure
// says what two of them are in atoms: a viewer grant is workflow:read and run:read, an operator
// grant is workflow:run, and operator "deliberately lacks workflow:read". editor is yes on every
// column but grant, which is every atom but grant:manage, and owner is every atom. So secret:write
// is held by owner, and by editor by default: a deny takes it from an editor as it takes any
// other atom, which is how a namespace keeps its declarations to its owners.
var bundles = map[Role][]Permission{
	Viewer:   {WorkflowRead, RunRead},
	Operator: {WorkflowRun},
	Editor: {
		WorkflowRead, WorkflowRun, WorkflowWrite, WorkflowDelete,
		RunRead, RunReadData, SecretUse, SecretWrite,
	},
	Owner: Permissions,
}

// Permissions are what the role holds, in the order the page lists them, and nothing for a role
// the page does not name.
func (r Role) Permissions() []Permission { return slices.Clone(bundles[r]) }

// Holds says whether the role carries one permission.
func (r Role) Holds(p Permission) bool { return slices.Contains(bundles[r], p) }

// Scope is what a permission is held at.
//
// "access is granted by binding a principal to a role, either on the whole namespace or on a
// single workflow." The third is the installation, which is where administration lives and
// which is not something a grant targets.
type Scope int

const (
	// Installation is the whole of it: the runner inventory, the namespaces, the users. A
	// refusal here is a 403, because the caller already knows the installation exists.
	Installation Scope = iota

	// Namespace is one namespace. A refusal is a 404: a namespace a caller cannot reach is
	// a namespace they may not learn the existence of.
	Namespace

	// Workflow is one workflow inside one namespace. A refusal is a 404 for the same
	// reason, which the page states outright: "an inaccessible workflow answering the same
	// 404 as an absent one, so that probing yields nothing".
	Workflow
)

// String names the scope.
func (s Scope) String() string {
	switch s {
	case Installation:
		return "installation"
	case Namespace:
		return "namespace"
	case Workflow:
		return "workflow"
	}
	return fmt.Sprintf("scope %d", int(s))
}

// Hides says whether a refusal at this scope is answered as absence.
func (s Scope) Hides() bool { return s == Namespace || s == Workflow }
