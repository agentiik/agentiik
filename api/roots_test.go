package api

import (
	"slices"
	"testing"

	"github.com/agentiik/agentiik/agk"
)

// Every first segment of an address the API answers at the root is a word no namespace is created
// under, so that the web console, which answers every other address and reads its first segment as
// a namespace, opens every new namespace's pages: auth reserved from the start, for the routes under
// /api/v1/auth/, and api, hooks, mcp and objects late, for the console's sake. No word is reserved
// for the console's sake but a root.
func TestEveryRootOfTheAPIIsAWordNoNamespaceTakes(t *testing.T) {
	for _, root := range apiRoots {
		r, late := agk.ReservedLate(root)
		switch {
		case !agk.IsReservedNamespace(root):
			t.Errorf("/%s is an address the API answers, and %s a name a namespace may still be given, whose console pages the API would answer", root, root)
		case root == "auth" && late:
			t.Errorf("auth, reserved before any namespace could take it, reads as reserved late: %+v", r)
		case root != "auth" && !(late && r.Console):
			t.Errorf("%s, a root of the API, is not reserved for the console's sake: %+v", root, r)
		}
	}
	for _, r := range agk.LateReservations {
		if r.Console && !slices.Contains(apiRoots, r.Word) {
			t.Errorf("%s is reserved for the console's sake, and the API answers no address under it", r.Word)
		}
	}
}
