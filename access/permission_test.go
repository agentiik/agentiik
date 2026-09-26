package access_test

import (
	"slices"
	"testing"

	"github.com/agentiik/agentiik/access"
)

// The nine atoms, held to the page. A permission invented here is one nothing documents and one
// no role includes.
func TestThePermissionsAreThePageOwn(t *testing.T) {
	want := []access.Permission{
		"workflow:read", "workflow:run", "workflow:write", "workflow:delete",
		"run:read", "run:read_data", "secret:use", "secret:write", "grant:manage",
	}
	if !slices.Equal(access.Permissions, want) {
		t.Fatalf("this package has %v and the page names %v", access.Permissions, want)
	}
	for _, p := range want {
		if !p.Valid() {
			t.Errorf("%s is not valid", p)
		}
	}
	for _, p := range []access.Permission{"", "run:everything", "run:read-data", "*", "Workflow:Read", "viewer"} {
		if p.Valid() {
			t.Errorf("%q is valid", p)
		}
	}
}

// A Set numbers the nine by an order of its own, which nothing an importer does to the exported
// list can change: a principal holding workflow:read keeps holding workflow:read and nothing else.
func TestEditingThePermissionListChangesNoSet(t *testing.T) {
	held := access.SetOf(access.WorkflowRead)
	was := access.Permissions[0]
	access.Permissions[0] = access.GrantManage
	defer func() { access.Permissions[0] = was }()

	if !held.Has(access.WorkflowRead) || held.Has(access.GrantManage) || !access.WorkflowRead.Valid() {
		t.Errorf("a set holding workflow:read reads as %s once the list is edited", held)
	}
	if got := access.SetOf(access.GrantManage).String(); got != "grant:manage" {
		t.Errorf("grant:manage reads as %q once the list is edited", got)
	}
}

// A set writes itself as the page writes permissions, in the page's order whatever order it was
// given, and a string that is not a permission is not one it can hold.
func TestASetHoldsTheNineAndNothingElse(t *testing.T) {
	s := access.SetOf(access.GrantManage, access.WorkflowRead, "run:everything", access.WorkflowRead)
	if got := s.String(); got != "workflow:read, grant:manage" {
		t.Errorf("the set reads %q", got)
	}
	if s.Has("run:everything") || s.Has("") {
		t.Error("the set holds a string that is not a permission")
	}
	if s != access.SetOf(access.WorkflowRead, access.GrantManage) {
		t.Error("two sets holding the same permissions are not equal")
	}
	if got := (access.Set{}).Permissions(); len(got) != 0 {
		t.Errorf("the empty set holds %v", got)
	}
}
