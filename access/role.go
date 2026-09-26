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

	// Operator runs, and follows the runs it starts: it "deliberately lacks workflow:read, so a
	// colleague can launch a job without seeing the queries, endpoints and business rules inside
	// it", and holds run:read "to follow the runs it starts, their state and log lines, and never
	// run:read_data".
	Operator Role = "operator"

	// Editor reads, runs, writes, reads data and uses and writes the namespace's secrets.
	// "editor holds it by default, and a deny takes it away where only owners should say where
	// a value lives": the it is secret:write. It does not delete: "every change an editor makes
	// is a new version the history keeps; a deletion takes the versions with it".
	Editor Role = "editor"

	// Owner is editor, workflow:delete and grant:manage, "because someone has to be able to
	// share", and "workflow:delete is the owner's alone".
	Owner Role = "owner"
)

// Roles are the four, in the order the page's table lists them. A test holds this list to the
// wire's enumeration.
var Roles = []Role{Viewer, Operator, Editor, Owner}

// The page's table has seven columns for nine atoms, and a second table names the atoms of each,
// which these are: "A role holds every permission of each column it says yes to, so each of the
// nine sits in one column, run:read in two." run:read is in read and in run, since reading a
// workflow and running one both come with following its runs; secret:use is in write, "because it
// is checked at the push, which is writing"; and delete is the owner's column alone.
var (
	readColumn   = SetOf(WorkflowRead, RunRead)
	runColumn    = SetOf(WorkflowRun, RunRead)
	writeColumn  = SetOf(WorkflowWrite, SecretUse)
	dataColumn   = SetOf(RunReadData)
	secretColumn = SetOf(SecretWrite)
	deleteColumn = SetOf(WorkflowDelete)
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
		return Editor.Permissions().union(deleteColumn).union(grantColumn)
	}
	return Set{}
}
