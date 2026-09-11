package expr

import (
	"encoding/json"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// The interpolation syntax, as the documentation writes it: ${{ ... }} inside a YAML
// string.
const (
	openBraces  = "${{"
	closeBraces = "}}"
)

// Template is one string value of the workflow file, read for the expressions in it. A
// string carrying no expression is a template too, with nothing to evaluate, so a caller
// reads every value the same way and asks afterwards whether anything was found.
type Template struct {
	scope    Scope
	source   string
	parts    []part
	programs []*Program
	whole    bool
}

// part is either literal text or one expression. The two rules of the language are the
// two shapes this can take: one part that is an expression fills the whole value and
// keeps its type, and anything else is text with expressions converted into it.
type part struct {
	text string
	prog *Program
}

// Interpolate reads one string value for the position it was written in. It refuses what
// it cannot read: an expression that never closes, an expression that does not compile,
// and a secret embedded in text, which is refused here rather than at evaluation because
// the checker already knows the value is a secret and a workflow that cannot be evaluated
// should not be accepted.
func Interpolate(sc Scope, s string) (*Template, error) {
	t := &Template{scope: sc, source: s}
	for i := 0; i < len(s); {
		at := strings.Index(s[i:], openBraces)
		if at < 0 {
			if i < len(s) {
				t.parts = append(t.parts, part{text: s[i:]})
			}
			break
		}
		at += i
		if at > i {
			t.parts = append(t.parts, part{text: s[i:at]})
		}
		start := at + len(openBraces)
		end, next, err := scanInterpolation(s, start)
		if err != nil {
			return nil, err
		}
		prog, err := Compile(sc, s[start:end])
		if err != nil {
			return nil, err
		}
		t.parts = append(t.parts, part{prog: prog})
		t.programs = append(t.programs, prog)
		i = next
	}
	t.whole = len(t.parts) == 1 && t.parts[0].prog != nil
	if !t.whole {
		for _, p := range t.programs {
			if isSecret(p.outputType()) {
				return nil, fmt.Errorf("%s: %w: it is embedded in a string, and an embedded expression is converted to text", p.source, ErrSecretOpaque)
			}
		}
	}
	return t, nil
}

// Whole says whether one expression fills the whole value. That is the rule the language
// states in one sentence: such an expression keeps its type, and an embedded one is
// converted to text.
func (t *Template) Whole() bool { return t.whole }

// Programs gives the expressions the value carries, in the order they were written. A
// value carrying none gives none, which is how a caller tells a plain string apart
// without scanning it a second time.
func (t *Template) Programs() []*Program {
	out := make([]*Program, len(t.programs))
	copy(out, t.programs)
	return out
}

// Source gives the value as the author wrote it, braces and all.
func (t *Template) Source() string { return t.source }

// Evaluate resolves a template against a context. A template that one expression fills
// gives that expression's value, with its type: a number stays a number and a list stays
// a list. Any other template gives a string, with each expression converted to text where
// it was written.
func Evaluate(t *Template, c Context) (any, error) {
	if t == nil {
		return nil, fmt.Errorf("there is no template to evaluate")
	}
	if t.whole {
		return t.parts[0].prog.Eval(c)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.prog == nil {
			b.WriteString(p.text)
			continue
		}
		v, err := p.prog.eval(c)
		if err != nil {
			return nil, err
		}
		text, err := asText(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.prog.source, err)
		}
		b.WriteString(text)
	}
	return b.String(), nil
}

// asText converts an evaluated value for a position inside a string.
//
// The documentation says an embedded expression is converted to text and does not say
// what the text of a list is, so two readings are taken. Where CEL defines a conversion
// to string, that conversion is used, because the language an author writes in is the
// language that should say what its values look like: a timestamp is RFC 3339, a duration
// is 3600s, a double is not padded. Where CEL defines none, which is the aggregates, the
// value is written in its document form, because JSON is the form every other value in
// this engine travels in.
//
// A secret is refused. It is the one value that has no text at all.
func asText(v ref.Val) (string, error) {
	if _, ok := v.(Secret); ok {
		return "", fmt.Errorf("%w: it is converted to no text", ErrSecretOpaque)
	}
	if types.IsError(v) {
		if err, ok := v.Value().(error); ok {
			return "", err
		}
	}
	if s, ok := v.ConvertToType(types.StringType).(types.String); ok {
		return string(s), nil
	}
	value, err := native(v)
	if err != nil {
		return "", err
	}
	doc, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("a %s fills part of a string and has no text form: %w", v.Type().TypeName(), err)
	}
	return string(doc), nil
}

// isSecret says whether the checker decided an expression evaluates to a secret.
func isSecret(t *cel.Type) bool {
	return t != nil && t.TypeName() == secretTypeName
}

// scanInterpolation finds where an expression opened at i ends. It reads the same
// lexical shapes the rewrite reads, so that a }} inside a string literal closes nothing
// and a map literal inside an expression closes only itself.
func scanInterpolation(s string, i int) (end, next int, err error) {
	opened := i - len(openBraces)
	depth := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			stop, err := scanString(s, i, false)
			if err != nil {
				return 0, 0, err
			}
			i = stop

		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				if raw, ok := stringPrefix(s[i:j]); ok {
					stop, err := scanString(s, j, raw)
					if err != nil {
						return 0, 0, err
					}
					i = stop
					continue
				}
			}
			i = j

		case c == '`':
			at := strings.IndexByte(s[i+1:], '`')
			if at < 0 {
				return 0, 0, fmt.Errorf("an escaped identifier opened with a backtick at offset %d is never closed", i)
			}
			i += at + 2

		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return 0, 0, fmt.Errorf("an expression opened with %s at offset %d is never closed: a comment runs to the end of the text", openBraces, opened)
			}
			i += nl + 1

		case c == '{':
			depth++
			i++

		case c == '}':
			if depth > 0 {
				depth--
				i++
				continue
			}
			if strings.HasPrefix(s[i:], closeBraces) {
				return i, i + len(closeBraces), nil
			}
			return 0, 0, fmt.Errorf("a } at offset %d closes nothing: an expression ends with %s", i, closeBraces)

		default:
			i++
		}
	}
	return 0, 0, fmt.Errorf("an expression opened with %s at offset %d is never closed", openBraces, opened)
}
