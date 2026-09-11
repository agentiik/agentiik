package agk

import (
	"fmt"
	"strings"

	"github.com/agentiik/agentiik/internal/ulid"
)

// identifierPattern is the grammar a step and a port are written in, quoted here as the
// schema writes it so that an error prints the rule rather than a paraphrase of it.
const identifierPattern = `^[A-Za-z0-9][A-Za-z0-9_-]*$`

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
