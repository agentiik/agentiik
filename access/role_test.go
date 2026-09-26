package access_test

import (
	"slices"
	"testing"

	"github.com/agentiik/agentiik/access"
)

// The role matrix, as the page's table writes it: six columns for nine atoms. Each role's row is
// written out atom by atom, and each column the page names an atom for is checked against it.
func TestTheFourRolesAreThePageTable(t *testing.T) {
	for _, c := range []struct {
		role  access.Role
		holds []access.Permission

		// The page's row: read, run, write, data, secret, grant.
		read, run, write, data, secret, grant bool
	}{
		{
			role:  access.Viewer,
			holds: []access.Permission{access.WorkflowRead, access.RunRead},
			read:  true,
		},
		{
			role:  access.Operator,
			holds: []access.Permission{access.WorkflowRun},
			run:   true,
		},
		{
			role: access.Editor,
			holds: []access.Permission{
				access.WorkflowRead, access.WorkflowRun, access.WorkflowWrite, access.WorkflowDelete,
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
			read: true, run: true, write: true, data: true, secret: true, grant: true,
		},
	} {
		got := c.role.Permissions()
		if !slices.Equal(got.Permissions(), c.holds) {
			t.Errorf("%s holds %s, want %s", c.role, got, access.SetOf(c.holds...))
		}
		// The columns, by the atoms the page names for them: its example expands viewer to
		// workflow:read and run:read and operator to workflow:run, and it says the secret
		// column is secret:write.
		for _, col := range []struct {
			p    access.Permission
			want bool
		}{
			{access.WorkflowRead, c.read}, {access.RunRead, c.read},
			{access.WorkflowRun, c.run},
			{access.WorkflowWrite, c.write},
			{access.RunReadData, c.data},
			{access.SecretWrite, c.secret},
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
// queries, endpoints and business rules inside it."
func TestAnOperatorRunsWhatItCannotRead(t *testing.T) {
	op := access.Operator.Permissions()
	if !op.Has(access.WorkflowRun) {
		t.Error("an operator cannot run")
	}
	for _, p := range []access.Permission{access.WorkflowRead, access.RunReadData, access.WorkflowWrite, access.SecretUse} {
		if op.Has(p) {
			t.Errorf("an operator holds %s", p)
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
