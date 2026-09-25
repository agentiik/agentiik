// Package dbname names the database, and the role that goes with it, that a test creates on the
// shared PostgreSQL AGENTIIK_TEST_DATABASE_URL names.
//
// It is a package of its own, with nothing of the application behind it, because package db
// tests itself against a real PostgreSQL too and cannot import dbtest, which imports it.
package dbname

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Max is the longest name Of answers. PostgreSQL keeps 63 bytes of an identifier, and a test
// derives roles of its own from the name by adding a suffix of up to eight, such as "_granter":
// a suffix that cut into the digest below would give two tests one role again.
const Max = 50

// digest is how many hexadecimal digits of the hash a name ends with: forty bits, which two
// tests of one checkout share by chance once in about a trillion pairs.
const digest = 10

// Of answers the name of the database this test creates, which is also its role's.
//
// A test starts by dropping whatever is left under its name, WITH (FORCE), which terminates
// every connection to it, so two tests given one name break each other at whatever the other
// was doing: a migration ended by "terminating connection due to administrator command", or
// a database that already exists. The name was once the test's name alone, and go test runs
// packages at once, so a test of api and a test of db that shared a name did exactly that,
// and so did one package tested at once from two checkouts on one machine.
//
// The name therefore ends with a digest of the test's name and of the directory it runs in,
// which go test sets to the package's own. The digest does not change from one run to the
// next, so a database a killed run left behind is the one the next run drops, rather than one
// more that nobody tidies up. The readable part in front is the test's name, cut to fit, so
// that a database found on the server says which test it was.
func Of(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("the directory the test runs in could not be read: %s", err)
	}
	return name(dir, t.Name())
}

func name(dir, test string) string {
	sum := sha256.Sum256([]byte(dir + "\x00" + test))
	suffix := "_" + hex.EncodeToString(sum[:])[:digest]

	// The name is written into statements unquoted, so only what an unquoted identifier may
	// hold is kept of the test's name, and the digest is what keeps apart the names this
	// folds together.
	readable := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return '_'
	}, test)
	readable = "agk_" + readable
	if keep := Max - len(suffix); len(readable) > keep {
		readable = readable[:keep]
	}
	return readable + suffix
}
