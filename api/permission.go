package api

import (
	"fmt"

	"github.com/agentiik/agentiik/access"
)

// Permission is one atom of what a principal may do. It is package access's, which resolves what a
// principal holds from its grants with no HTTP behind it, so that the controller asks the same
// question with the same words when a run is created. The names are kept here so that every route
// reads as it was written: one vocabulary, spelled from either package.
type Permission = access.Permission

// The nine, as package access names them.
const (
	WorkflowRead   = access.WorkflowRead
	WorkflowRun    = access.WorkflowRun
	WorkflowWrite  = access.WorkflowWrite
	WorkflowDelete = access.WorkflowDelete
	RunRead        = access.RunRead
	RunReadData    = access.RunReadData
	SecretUse      = access.SecretUse
	SecretWrite    = access.SecretWrite
	GrantManage    = access.GrantManage
)

// Permissions are the nine, in the order the page lists them: package access's list.
var Permissions = access.Permissions

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
