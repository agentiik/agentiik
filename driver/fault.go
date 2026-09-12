package driver

import (
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
)

// The errors of this package name the step, the port and the rule, in the words the
// documentation uses for them. A message that says only what went wrong leaves the
// reader to find out which step it was about and which rule was breached, and both are
// known here.

// Charge says whose failure this was. It exists because the exit code table already
// draws the line and a driver reporting its own trouble as a brick failure fails
// somebody else's step: "125 and above is read as an infrastructure failure, charged to
// the runner and not to the brick".
type Charge int

const (
	// ChargeBrick is the image's own failure: a manifest that breaks the contract,
	// an output that could not be read back.
	ChargeBrick Charge = iota

	// ChargePlatform is this side's failure: a daemon that could not be reached, a
	// pull that died, a working directory that could not be prepared. None of it is
	// the brick's, and none of it is retried on the step's policy.
	ChargePlatform
)

// String names the charge as a log line carries it.
func (c Charge) String() string {
	if c == ChargePlatform {
		return "the runtime"
	}
	return "the brick"
}

// Fault is one refusal, naming what it was about.
//
// Step is always set, because every one of these happens inside a step. Port is set
// where the rule is about one port and empty where it is about the task. Rule is the
// documentation's own sentence, so that a reader who wants the page can search for the
// words rather than guess which section they came from. Detail is what was actually
// found.
type Fault struct {
	Step   agk.Step
	Port   agk.Port
	Rule   string
	Charge Charge
	Detail string

	// err is the sentinel this fault is an instance of, so that a caller tests with
	// errors.Is rather than by reading the prose.
	err error
}

// Error writes the step, the port, what was found and the rule, in that order, which is
// the order the documentation asks a failure to be reported in: "naming the step, the
// exit code and what was refused, in that order".
func (f *Fault) Error() string {
	s := "driver: step " + string(f.Step)
	if f.Port != "" {
		s += ": port " + string(f.Port)
	}
	if f.Detail != "" {
		s += ": " + f.Detail
	}
	if f.Rule != "" {
		s += ": " + f.Rule
	}
	return s
}

// Unwrap gives up the sentinel, which is how ErrRootUser and the rest are tested for.
func (f *Fault) Unwrap() error { return f.err }

// fault composes one, with the sentinel it is an instance of.
func fault(step agk.Step, err error, charge Charge, format string, args ...any) *Fault {
	return &Fault{
		Step:   step,
		Rule:   ruleOf(err),
		Charge: charge,
		Detail: fmt.Sprintf(format, args...),
		err:    err,
	}
}

// ruleOf is the sentinel's own sentence, which is the rule in the documentation's words.
func ruleOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ErrDaemonUnreachable is the daemon not being there at all. It is not a failed step: a
// task whose container never existed has no exit code, and inventing one for it would
// charge this side's trouble to somebody's brick.
var ErrDaemonUnreachable = errors.New("the Docker daemon could not be reached, so no container was created and no exit code exists")

// ErrContractBroken is an image that does not honour the brick contract: a manifest that
// does not read, an output that is not an envelope, a port file the collection cannot
// take. The exit code table already has a band for it, 121 to 124, "reserved for the
// runner. A brick that exits with one is treated as having failed the contract, whatever
// its manifest says".
var ErrContractBroken = errors.New("the image does not honour the brick contract")

// Charged says whose failure an error was, and whether anybody decided. A plain error
// from somewhere else is nobody's until this package says so.
func Charged(err error) (Charge, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f.Charge, true
	}
	return ChargeBrick, false
}
