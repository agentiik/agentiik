package schema

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// The rules an input can be refused by. They are the names the workflow language gives
// them, so that the rule an error prints is the key a person can find in the file.
const (
	// RuleRequired is required: true on the input, which "refuses a run that does not
	// supply the input, rather than letting every step downstream discover the absence
	// for itself".
	RuleRequired = "required"
	// RuleSchema is the input's own schema, which says "what the value has to look like
	// for the run to start".
	RuleSchema = "schema"
	// RuleUndeclared is the trigger naming an input the workflow does not declare.
	RuleUndeclared = "undeclared"
)

// Input is one declared workflow input: what the value has to look like, whether a run
// can start without it, and what stands in when it is absent.
//
// Schema may be nil. An input without one "is accepted as it comes", which is what a
// workflow says when the shape of a value is not its business.
type Input struct {
	Schema   *Schema
	Required bool
	Default  any
}

// Bind resolves the inputs a run was started with against the inputs the workflow
// declares, and returns the values the graph reads.
//
// It applies the two keywords that are not JSON Schema's. required refuses a run that
// supplies nothing, and default stands in for a value the run did not supply, so that
// "the graph reads one value and never tests for absence". Everything else is the
// input's own schema.
//
// An input that is neither supplied nor defaulted is absent from the result rather than
// present and nil. The language gives default as the way to make an optional input
// ordinary, so an input that declines to declare one has said that absence is a state
// the graph will see.
//
// A value is supplied when its key is present, whatever the value is. JSON null is a
// value a schema can accept or refuse like any other, and reading it as absence would
// make a caller unable to say null at all. A declared default is the other way round:
// Default carries the value on its own, so a default written as null is the same
// declaration as no default, and the input falls back to required or to absence. The
// workflow schema records the same ambiguity and nothing here can resolve it.
//
// A default is applied and then validated like any other value. A default that does not
// satisfy the input's own schema is a workflow that cannot start, and finding that at
// the trigger is better than handing the graph a value the workflow says is impossible.
// The refusal says the value came from the default, because the run supplied nothing and
// the person reading the log is looking at the wrong end otherwise.
//
// The error is an *InputRefusal wrapping agk.ErrRunRefused, and only the first refusal
// is returned: a run is refused, not annotated. Refusals are looked for in a fixed
// order, undeclared names before declared ones, so that a misspelled input reports the
// misspelling rather than the required input that misspelling left unsupplied.
func Bind(declared map[string]Input, supplied map[string]any) (map[string]any, error) {
	for _, name := range sorted(supplied) {
		if _, ok := declared[name]; !ok {
			return nil, &InputRefusal{
				Input:  name,
				Rule:   RuleUndeclared,
				Detail: "the workflow declares no input of that name",
			}
		}
	}

	bound := make(map[string]any, len(declared))
	for _, name := range sorted(declared) {
		in := declared[name]

		v, ok := supplied[name]
		fromDefault := false
		if !ok {
			if in.Default == nil {
				if in.Required {
					return nil, &InputRefusal{
						Input:  name,
						Rule:   RuleRequired,
						Detail: "no value supplied and the input declares no default",
					}
				}
				continue
			}
			// The declaration is read once and handed to every run, so a run that
			// reaches into a defaulted value would be editing what the next run starts
			// from.
			v = clone(in.Default)
			fromDefault = true
		}

		if in.Schema != nil {
			if err := in.Schema.Validate(v); err != nil {
				detail := err.Error()
				if fromDefault {
					detail = "the declared default does not satisfy it: " + detail
				}
				return nil, &InputRefusal{Input: name, Rule: RuleSchema, Detail: detail}
			}
		}
		bound[name] = v
	}
	return bound, nil
}

// InputRefusal is a workflow input that refused the run, naming the input, the rule that
// refused it and what was wrong with the value.
type InputRefusal struct {
	Input  string
	Rule   string
	Detail string
}

// Error names the input, then the rule, then what was refused, and ends on what that
// does to the run. It is the order and the shape agk.Refusal is written in, so that the
// two refusals a person meets in a log read alike.
func (r *InputRefusal) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "input %s: %s", r.Input, r.Rule)
	if r.Detail != "" {
		b.WriteString(": ")
		b.WriteString(r.Detail)
	}
	b.WriteString("; ")
	b.WriteString(agk.ErrRunRefused.Error())
	return b.String()
}

// Unwrap makes every input refusal answer to agk.ErrRunRefused, so that a caller which
// only has to decide whether the run starts does not have to know which of the three
// rules refused it.
func (r *InputRefusal) Unwrap() error { return agk.ErrRunRefused }

// sorted returns the names of m in order, so that a run refused by two inputs at once is
// refused by the same one every time. A refusal that moves between runs is a refusal
// nobody can write a test against.
func sorted[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// clone deep copies a decoded JSON value. Anything that is not a JSON container is
// already immutable as far as a caller is concerned.
func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(v))
		for k, e := range v {
			c[k] = clone(e)
		}
		return c
	case []any:
		c := make([]any, len(v))
		for i, e := range v {
			c[i] = clone(e)
		}
		return c
	default:
		return v
	}
}
