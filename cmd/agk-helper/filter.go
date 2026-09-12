package main

import (
	"fmt"
	"strings"
)

// supported is the whole of what --filter takes, written out so that the refusal prints
// the two forms rather than a paraphrase of them.
const supported = "--filter takes a field path, as .valid, or a field path piped into not, as .valid | not, and nothing else: for anything more, jq and a redirect do the same job"

// filter keeps the items a field is true of, or false of.
//
// Two forms and no more. The documentation's example writes --filter '.valid' and --filter
// '.valid | not', does not say the expression is a jq program, and says in the same breath
// that this helper "is a convenience, never a requirement; jq and a redirect do the same
// job". So those two are what is accepted, and anything else is refused naming them.
//
// It is never called jq, in the help or in a refusal. A subset parser answering to that
// name would be the worst of the available answers: it would promise a language and
// deliver two expressions of it, and a script whose --filter '.items | length > 0' was
// refused would have been lied to rather than told. The alternative was taking a pure-Go
// jq as a dependency, which would put a full expression evaluator inside every container
// that runs a script step for a convenience the page says is never required; that is a
// decision worth raising rather than taking quietly.
type filter struct {
	// path is the field path, outermost first. Empty is no filter, which keeps
	// everything.
	path []string
	// negate is the | not, which keeps what the path is false of.
	negate bool
}

// parseFilter reads one of the two forms.
func parseFilter(expr string) (filter, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return filter{}, nil
	}

	var f filter
	path := expr
	if before, after, piped := strings.Cut(expr, "|"); piped {
		if strings.TrimSpace(after) != "not" {
			return filter{}, fmt.Errorf("%q pipes into %q: %s", expr, strings.TrimSpace(after), supported)
		}
		path, f.negate = strings.TrimSpace(before), true
	}

	rest, ok := strings.CutPrefix(path, ".")
	if !ok {
		return filter{}, fmt.Errorf("%q does not begin with a dot: %s", expr, supported)
	}
	if rest == "" {
		return filter{}, fmt.Errorf("%q names no field: %s", expr, supported)
	}
	for _, name := range strings.Split(rest, ".") {
		if err := fieldName(name, expr); err != nil {
			return filter{}, err
		}
		f.path = append(f.path, name)
	}
	return f, nil
}

// fieldName holds one segment of a field path to being one.
//
// The grammar is the one a field of an item's data is written in where a path can reach
// it. A bracket, a quote or a space is where the other language begins, and this is the
// line at which the refusal is more use than an attempt: a filter that accepted
// .items[0].name and then ignored the index would be worse than one that refused it.
func fieldName(name, expr string) error {
	if name == "" {
		return fmt.Errorf("%q has an empty field between two dots: %s", expr, supported)
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return fmt.Errorf("%q carries %q in a field name: %s", expr, string(c), supported)
		}
	}
	return nil
}

// keeps says whether one item's data passes the filter.
//
// A field is false when it is absent, null or false, and true otherwise. That is the only
// reading that lets --filter '.valid' and --filter '.valid | not' partition one payload
// into two ports, which is exactly what the documented example does with them: a field
// missing from half the responses would otherwise be kept by neither.
//
// Zero and the empty string are true, because they are values the field carries. A step
// that means "zero" has a condition and not a field.
func (f filter) keeps(data map[string]any) bool {
	if len(f.path) == 0 {
		return true
	}
	return truthy(walk(data, f.path)) != f.negate
}

// walk follows a field path, answering nil where any step of it is absent or where the
// document stops being an object before the path does.
func walk(data map[string]any, path []string) any {
	var v any = data
	for _, name := range path {
		object, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = object[name]; !ok {
			return nil
		}
	}
	return v
}

// truthy applies the rule: null and false are false, and everything else a field carries
// is true.
func truthy(v any) bool {
	if v == nil {
		return false
	}
	b, ok := v.(bool)
	return !ok || b
}
