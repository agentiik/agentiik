package dbname

import (
	"regexp"
	"strings"
	"testing"
)

// The test that failed on the shared server: TestADrainingOrRevokedRunnerRedeemsNothing is a test
// of package api and one of package db, and go test runs the two packages at once.
func TestOneTestNameInTwoPackagesIsTwoDatabases(t *testing.T) {
	const test = "TestADrainingOrRevokedRunnerRedeemsNothing"
	api, db := name("/src/agentiik/api", test), name("/src/agentiik/db", test)
	if api == db {
		t.Errorf("the test of api and the test of db are both given %s", api)
	}
	if other := name("/src/other-checkout/api", test); other == api {
		t.Errorf("one package tested from two checkouts is given %s twice", api)
	}
}

// A run killed before its cleanups leaves a database behind, and the next run of the same test
// drops it only if it asks for it by the same name.
func TestATestIsGivenTheSameNameOnEveryRun(t *testing.T) {
	if a, b := name("/src/agentiik/api", "TestX/sub"), name("/src/agentiik/api", "TestX/sub"); a != b {
		t.Errorf("one test is given %s and then %s", a, b)
	}
	if Of(t) != Of(t) {
		t.Errorf("Of answers two names for one test")
	}
}

func TestANameIsAnIdentifierThatFitsWithItsRolesSuffixes(t *testing.T) {
	identifier := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	long := "TestALeaderCutOffFromItsDatabaseStops/asked_to_stop_" + strings.Repeat("x", 80)
	for _, test := range []string{"TestX", "TestX/a-b.c d'é", long} {
		got := name("/src/agentiik/cmd/agentiik-controller", test)
		if !identifier.MatchString(got) {
			t.Errorf("%q is given %q, which is not an unquoted identifier", test, got)
		}
		if len(got) > Max || len(got+"_granter") > 63 {
			t.Errorf("%q is given %q, %d bytes", test, got, len(got))
		}
	}
	// Two names that differ only past the cut, or only in what is folded to an underscore, are
	// still two databases.
	if name("/d", long+"a") == name("/d", long+"b") {
		t.Error("two long test names that differ only at the end are given one name")
	}
	if name("/d", "TestX/a-b") == name("/d", "TestX/a.b") {
		t.Error("two test names that differ only in a character folded away are given one name")
	}
}
