package access_test

import (
	"slices"
	"testing"

	"github.com/agentiik/agentiik/access"
)

// The role matrix, as the page's tables write it: seven columns, and the permissions of each.
// Each role's row is written out atom by atom, and each column is checked against it by the
// permissions the page names for that column.
func TestTheFourRolesAreThePageTable(t *testing.T) {
	for _, c := range []struct {
		role  access.Role
		holds []access.Permission

		// The page's row: read, run, write, data, secret, delete, grant.
		read, run, write, data, secret, delete, grant bool
	}{
		{
			role:  access.Viewer,
			holds: []access.Permission{access.WorkflowRead, access.RunRead},
			read:  true,
		},
		{
			role:  access.Operator,
			holds: []access.Permission{access.WorkflowRun, access.RunRead},
			run:   true,
		},
		{
			role: access.Editor,
			holds: []access.Permission{
				access.WorkflowRead, access.WorkflowRun, access.WorkflowWrite,
				access.RunRead, access.RunReadData, access.SecretUse, access.SecretWrite,
			},
			read: true, run: true, write: true, data: true, secret: true,
		},
		{
			role: access.Owner,
			holds: []access.Permission{
				access.WorkflowRead, access.WorkflowRun, access.WorkflowWrite, access.WorkflowDelete,
				access.RunRead, access.RunReadData, access.SecretUse, access.SecretWrite, access.GrantManage,
			},
			read: true, run: true, write: true, data: true, secret: true, delete: true, grant: true,
		},
	} {
		got := c.role.Permissions()
		if !slices.Equal(got.Permissions(), c.holds) {
			t.Errorf("%s holds %s, want %s", c.role, got, access.SetOf(c.holds...))
		}
		// The columns, by the permissions the page names for each. run:read is in two, so a
		// role holds it where either says yes.
		for _, col := range []struct {
			p    access.Permission
			want bool
		}{
			{access.WorkflowRead, c.read}, {access.RunRead, c.read || c.run},
			{access.WorkflowRun, c.run},
			{access.WorkflowWrite, c.write}, {access.SecretUse, c.write},
			{access.RunReadData, c.data},
			{access.SecretWrite, c.secret},
			{access.WorkflowDelete, c.delete},
			{access.GrantManage, c.grant},
		} {
			if got.Has(col.p) != col.want {
				t.Errorf("%s holding %s is %v, and the page's table says %v", c.role, col.p, got.Has(col.p), col.want)
			}
		}
		if !c.role.Valid() {
			t.Errorf("%s is not valid", c.role)
		}
	}
}

// "operator deliberately lacks workflow:read, so a colleague can launch a job without seeing the
// queries, endpoints and business rules inside it. It holds run:read, so it follows the runs of
// what it may run, the ones it starts among them: their state and log lines, and never
// run:read_data, so their payloads stay out of its reach."
func TestAnOperatorRunsWhatItCannotRead(t *testing.T) {
	op := access.Operator.Permissions()
	if !op.Has(access.WorkflowRun) {
		t.Error("an operator cannot run")
	}
	if !op.Has(access.RunRead) {
		t.Error("an operator cannot follow the runs it starts")
	}
	for _, p := range []access.Permission{access.WorkflowRead, access.RunReadData, access.WorkflowWrite, access.SecretUse} {
		if op.Has(p) {
			t.Errorf("an operator holds %s", p)
		}
	}
}

// "workflow:delete is the owner's alone": no other role holds it, and an owner does.
func TestOnlyAnOwnerDeletes(t *testing.T) {
	for _, r := range access.Roles {
		if got := r.Permissions().Has(access.WorkflowDelete); got != (r == access.Owner) {
			t.Errorf("%s holding workflow:delete is %v", r, got)
		}
	}
}

// Four roles and no more: a string that is not one of them is no role and stands for nothing.
func TestARoleIsOneOfTheFour(t *testing.T) {
	if !slices.Equal(access.Roles, []access.Role{"viewer", "operator", "editor", "owner"}) {
		t.Errorf("the roles are %v", access.Roles)
	}
	for _, r := range []access.Role{"", "admin", "Owner", "owner ", "workflow:read"} {
		if r.Valid() {
			t.Errorf("%q is a role", r)
		}
		if got := r.Permissions(); got != (access.Set{}) {
			t.Errorf("%q stands for %s", r, got)
		}
	}
}
