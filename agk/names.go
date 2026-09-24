package agk

import (
	"fmt"
	"strings"

	"github.com/agentiik/agentiik/internal/ulid"
)

// identifierPattern is the grammar a step and a port are written in, quoted here as the
// schema writes it so that an error prints the rule rather than a paraphrase of it.
const identifierPattern = `^[A-Za-z0-9][A-Za-z0-9_-]*$`

// IdentifierMaxBytes is the longest an identifier may be, which is NAME_MAX: 255 bytes
// is what the filesystems a runner lays a task out on hold a name to, and the grammar
// is one "so that one name survives a URL, a directory and a tool list unchanged". A
// step is a directory of its task's work directory, a port a directory under /agk/in/
// and a file under /agk/out/ports/, a secret a file under /agk/secrets/, so a longer
// name is one no runner could lay out, and a version holding one is a version every run
// of which fails. It is refused where the name is written instead, and the database's
// identifier domain holds the same bound. The grammar is ASCII, so this is characters
// as well as bytes.
const IdentifierMaxBytes = 255

// ReservedNamespaces are the words the API routes on as the first segment after /api/v1/,
// which is why they cannot name a namespace: GET /api/v1/runs/{id} and GET
// /api/v1/{ns}/runs would both claim /api/v1/runs/runs, and the router answers such a
// path by the word. The list is fixed rather than read off the routes, so that a route
// added later under a new word cannot make an existing namespace unreachable. The
// schemas refuse the same words, and namespace and login creation refuse them from
// v0.3.0, since each user gets a namespace named after their login.
var ReservedNamespaces = []string{"auth", "me", "users", "groups", "service-accounts", "namespaces", "runners", "runner-pools", "bus", "tasks", "bricks", "runs", "artifacts"}

// IsReservedNamespace reports whether name is one of ReservedNamespaces.
func IsReservedNamespace(name string) bool {
	for _, w := range ReservedNamespaces {
		if name == w {
			return true
		}
	}
	return false
}

// RunID is the identifier of a run, carried as the run's ULID. It is the value the
// container reads as AGK_RUN_ID and the first path segment of every artifact URI.
type RunID string

// NewRunID mints one. It is the one door a run identifier is minted through, so that
// what identity is made of stays a decision of this module and not of its callers.
func NewRunID() RunID { return RunID(ulid.New()) }

// Validate refuses a run identifier that could not have been minted here.
//
// No length is imposed. The documentation says a run carries a ULID, which is
// twenty-six characters, and then prints run identifiers shorter than that, no two of
// them the same length; the schema records that reading and asks only that the value be
// present. Imposing twenty-six here would refuse the documentation's own examples and
// the released fixtures with them.
func (r RunID) Validate() error {
	if r == "" {
		return fmt.Errorf("a run identifier is never empty")
	}
	return segment(string(r))
}

// Step is the name a workflow gave a step.
type Step string

// Validate refuses a step name that is not an identifier.
func (s Step) Validate() error { return identifier(string(s), "a step name") }

// Port is the name of one output port of a step.
//
// It travels as a directory under /agk/in/, as a file name under /agk/out/ports/ and as
// one entry of the comma separated AGK_OUT_PORTS, which is why the grammar is the one
// it is: a name carrying a separator, a comma or a space would not survive any of the
// three.
type Port string

// Validate refuses a port name that is not an identifier.
func (p Port) Validate() error { return identifier(string(p), "a port name") }

// identifier applies the grammar a step and a port share.
func identifier(v, what string) error {
	if v == "" {
		return fmt.Errorf("%s is never empty: it matches %s", what, identifierPattern)
	}
	if len(v) > IdentifierMaxBytes {
		return fmt.Errorf("%.64s... is %d characters long, and %s is at most %d: it becomes a file or a directory name, and no filesystem holds a longer one", v, len(v), what, IdentifierMaxBytes)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if i > 0 {
			ok = ok || c == '_' || c == '-'
		}
		if !ok {
			return fmt.Errorf("%q is not an identifier: %s matches %s", v, what, identifierPattern)
		}
	}
	return nil
}

// segment refuses what cannot travel as one path segment.
//
// A run identifier and an artifact name are both segments of the agk:// URI, and the
// name is also the file a consuming container finds under its input mount, so a value
// carrying a separator would address one thing and mount another. The two names that
// mean a directory rather than a file are refused for the same reason.
func segment(v string) error {
	if v == "" {
		return fmt.Errorf("a name is never empty")
	}
	if v == "." || v == ".." {
		return fmt.Errorf("%q names a directory and not a file: a name is one segment of the agk:// URI and one file under the input mount", v)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c <= ' ' || c == 0x7f || strings.IndexByte(`/\?#[]`, c) >= 0 {
			return fmt.Errorf("%q carries %q: a name is one segment of the agk:// URI and one file under the input mount, so it carries no whitespace, no path separator and no URI delimiter", v, string(c))
		}
	}
	return nil
}
