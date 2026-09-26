package access

// Role is a fixed set of permissions a grant binds a principal to.
//
// "viewer, operator and editor are the read, run and edit roles; owner exists because someone has
// to be able to share." Four and no more, and fixed rather than configured, since a role invented
// per installation is one the clients cannot explain. A principal needing more than one holds more
// than one: "someone who needs both holds both roles", and Resolve adds them up.
type Role string

const (
	// Viewer reads: the workflow and its runs, without their data.
	Viewer Role = "viewer"

	// Operator runs, and reads nothing. It "deliberately lacks workflow:read, so a colleague can
	// launch a job without seeing the queries, endpoints and business rules inside it".
	Operator Role = "operator"

	// Editor reads, runs, writes, reads data and uses and writes the namespace's secrets.
	// "editor holds it by default, and a deny takes it away where only owners should say where
	// a value lives": the it is secret:write.
	Editor Role = "editor"

	// Owner is editor and grant:manage, "because someone has to be able to share".
	Owner Role = "owner"
)

// Roles are the four, in the order the page's table lists them. A test holds this list to the
// wire's enumeration.
var Roles = []Role{Viewer, Operator, Editor, Owner}

// The page's table has six columns for nine atoms, and these are the columns. read is what the
// page's own example expands a viewer grant to, "workflow:read, run:read", and run what it expands
// an operator grant to, "workflow:run". data is run:read_data, and secret is secret:write, as the
// page says of that column. The table names no column for workflow:delete or secret:use, so they
// sit with write: deleting a workflow and letting its steps reference a namespace secret are both
// part of deciding what the workflow is, and the roles holding write are the ones that decide it.
// No role holds write without secret, so where the two sit changes nothing a role holds.
var (
	readColumn   = SetOf(WorkflowRead, RunRead)
	runColumn    = SetOf(WorkflowRun)
	writeColumn  = SetOf(WorkflowWrite, WorkflowDelete, SecretUse)
	dataColumn   = SetOf(RunReadData)
	secretColumn = SetOf(SecretWrite)
	grantColumn  = SetOf(GrantManage)
)

// Valid says whether this is one of the four. It asks the constants rather than Roles, which an
// importer could append to.
func (r Role) Valid() bool {
	switch r {
	case Viewer, Operator, Editor, Owner:
		return true
	}
	return false
}

// Permissions is the set the role stands for, the page's row for it, and the empty set for a
// string that is not one of the four.
func (r Role) Permissions() Set {
	switch r {
	case Viewer:
		return readColumn
	case Operator:
		return runColumn
	case Editor:
		return readColumn.union(runColumn).union(writeColumn).union(dataColumn).union(secretColumn)
	case Owner:
		return Editor.Permissions().union(grantColumn)
	}
	return Set{}
}
