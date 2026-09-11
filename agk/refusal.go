package agk

import (
	"errors"
	"fmt"
	"strings"
)

// The names the four size rules are written under, in the documentation's own spelling,
// so that a Refusal read from a log names the setting an operator would go and look at.
const (
	RuleInlineMaxBytes   = "inline_max_bytes"
	RuleEnvelopeMaxBytes = "envelope_max_bytes"
	RuleMaxItems         = "max_items"
	RuleArtifactMaxBytes = "artifact_max_bytes"
)

// Outcome says what a refusal does to the run. The documentation distinguishes two, and
// the difference is not cosmetic: one discards a document, the other fails a step and
// enters the retry policy.
type Outcome int

const (
	// Reject refuses the envelope whole and publishes nothing. It is what the
	// runner does with a document that breaks the contract, an oversized value left
	// inline being the case the documentation names.
	Reject Outcome = iota + 1

	// Fail is an application failure of the emitting step, the exit code table's
	// band 1 to 99: the step produced more than may travel, and what it must do
	// differently is paginate, spill or write less.
	Fail
)

// String names the outcome as a log line ends on it.
func (o Outcome) String() string {
	switch o {
	case Reject:
		return "envelope rejected"
	case Fail:
		return "step failed"
	default:
		return "outcome unknown"
	}
}

// The two ways a refusal lands, and the way a run is refused before it starts. They are
// what a caller tests with errors.Is; the detail of what was refused is in the error's
// own text.
var (
	// ErrEnvelopeRejected is returned when an envelope is refused whole: nothing on
	// that port is published and the step is not at fault.
	ErrEnvelopeRejected = errors.New("envelope rejected")

	// ErrStepFailed is returned when the refusal is an application failure of the
	// step that emitted the envelope.
	ErrStepFailed = errors.New("step failed")

	// ErrRunRefused is returned when a run is refused before it starts, which at
	// v0.1.0 is what happens to inputs that do not validate.
	ErrRunRefused = errors.New("run refused")
)

// Refusal is one size rule refusing one thing, naming the step and the port it refused
// it on. It is an error, and the error it unwraps to is the outcome: a rejected
// envelope and a failed step are told apart with errors.Is and not by reading the text.
type Refusal struct {
	Step    Step
	Port    Port
	Rule    string
	Outcome Outcome
	Limit   int64
	Got     int64
	Detail  string
}

// Error names the step, then the port, then the rule, then what was refused, and ends
// on the outcome. That order is the one a person scanning a log reads in: whose step,
// which port, which setting, and what it does to the run.
func (r *Refusal) Error() string {
	var b strings.Builder
	if r.Step != "" {
		fmt.Fprintf(&b, "step %s: ", r.Step)
	}
	if r.Port != "" {
		fmt.Fprintf(&b, "port %s: ", r.Port)
	}
	b.WriteString(r.Rule)
	b.WriteString(": ")
	if r.Detail != "" {
		b.WriteString(r.Detail)
	} else {
		fmt.Fprintf(&b, "%d %s, above the %d the rule allows", r.Got, unitOf(r.Rule), r.Limit)
	}
	b.WriteString("; ")
	b.WriteString(r.Outcome.String())
	return b.String()
}

// Unwrap turns the outcome into the sentinel a caller tests for.
func (r *Refusal) Unwrap() error {
	switch r.Outcome {
	case Reject:
		return ErrEnvelopeRejected
	case Fail:
		return ErrStepFailed
	default:
		return nil
	}
}

// unitOf gives the rule its unit, so that a Refusal built anywhere in the module reads
// in whole words without carrying a field for it.
func unitOf(rule string) string {
	if rule == RuleMaxItems {
		return "items"
	}
	return "bytes"
}

// refuse builds the refusal of one size rule. Every one of them is worded the same way
// on purpose: what was refused, how big it is, and what the rule allows. Only the verb
// moves, because an envelope is so many bytes and holds so many items.
func refuse(rule string, o Outcome, step Step, port Port, what string, got, limit int64) *Refusal {
	verb := "is"
	if rule == RuleMaxItems {
		verb = "holds"
	}
	return &Refusal{
		Step:    step,
		Port:    port,
		Rule:    rule,
		Outcome: o,
		Limit:   limit,
		Got:     got,
		Detail:  fmt.Sprintf("%s %s %d %s, above the %d the rule allows", what, verb, got, unitOf(rule), limit),
	}
}

// rejection is a document refused for its shape rather than its size. The envelope is a
// closed document and a departure from it refuses the whole, so there is one outcome
// here and no field for it.
//
// It stays unexported: what a caller needs is errors.Is against ErrEnvelopeRejected and
// a sentence naming the member, and a second exported error type would be a second
// vocabulary for the same refusal.
type rejection struct {
	step Step
	port Port
	what string // where in the document, as items[1].files[0].sha256
	why  string // the rule, in the documentation's words
}

func (r *rejection) Error() string {
	var b strings.Builder
	if r.step != "" {
		fmt.Fprintf(&b, "step %s: ", r.step)
	}
	if r.port != "" {
		fmt.Fprintf(&b, "port %s: ", r.port)
	}
	if r.what != "" {
		b.WriteString(r.what)
		b.WriteString(": ")
	}
	b.WriteString(r.why)
	b.WriteString("; ")
	b.WriteString(Reject.String())
	return b.String()
}

func (r *rejection) Unwrap() error { return ErrEnvelopeRejected }

// reject builds a rejection naming where in the document the refusal is.
func reject(what, why string, a ...any) *rejection {
	return &rejection{what: what, why: fmt.Sprintf(why, a...)}
}

// located attributes a rejection to the step and the port once the metadata that names
// them has itself been read. A refusal found while parsing items cannot know them
// before meta is parsed, and a log line that names neither is a log line nobody can act
// on.
func located(err error, step Step, port Port) error {
	var r *rejection
	if errors.As(err, &r) && r.step == "" {
		r.step, r.port = step, port
	}
	return err
}
