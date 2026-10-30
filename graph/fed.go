package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/internal/expr"
)

// checkFedPorts holds what the inputs keyword feeds a port to what a port carries. "A port
// carries a batch of items", so the value is an object, which is one item, or a list of objects,
// one item each, and a run refuses anything else to the step before any container starts
// (envelopeOf), on every run, where the person who wrote it reads the reason of a step that
// failed rather than the refusal of a push. Where the file alone shows that the value is no
// batch, it is refused here instead.
//
// The file shows it in two ways. A value written as it is: a number, a boolean, text, or a list
// holding one of them, text with an expression embedded in it included, since "an expression
// embedded in a string is converted to text". And an expression that is a workflow input and
// nothing else, ${{ workflow.inputs.count }}, whose schema declares only types no batch has: a
// string, a number, an integer, a boolean, or a list whose items are none of them an object.
// Anything else is the run's to tell, since only the value says: an expression computing
// something, an input with no schema, one given by a $ref, with no type, or with a type that
// admits an object, a list of objects or nothing, which is a batch of nothing.
//
// A step that calls a workflow is not held to it, since its inputs keyword writes the called
// workflow's inputs, "a string or a number among them, which no port could carry"; nor is a step
// whose call could still arrive from a block an included file carries, which Load settles.
//
// A rule of v0.7.2, so it holds a version being made and never one read back: a stored version
// feeding a port a scalar keeps rebuilding, and its step keeps failing with the reason it failed
// with before.
func checkFedPorts(wf *Workflow) error {
	if !wf.made {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		if st.Call != nil || (!wf.resolved && len(st.Extends) > 0) {
			continue
		}
		for _, port := range slices.Sorted(maps.Keys(st.Inputs)) {
			v := st.Inputs[port]
			what := notABatch(wf, v)
			if what == "" {
				continue
			}
			return place(refuse(RuleStepInputNotABatch, name, port, fmt.Sprintf(
				"inputs feeds the port %s, and a port carries a batch of items: an object, which is one item, or a list of objects, one item each. Written under a key of an object, %s: { value: %s }, it is one item, and the key is yours to choose",
				what, port, written(v))), wf.StepAt(name, "inputs", string(port)))
		}
	}
	return nil
}

// notABatch says what a value fed to a port is, in the file's terms, where the file shows that
// it is no batch, and nothing where it is one or the file cannot tell. Nothing written, null, is
// a batch of nothing, which is what a port carries when no item came.
func notABatch(wf *Workflow, v any) string {
	switch value := v.(type) {
	case nil, map[string]any:
		return ""
	case []any:
		for i, entry := range value {
			if what := notAnItem(wf, entry); what != "" {
				return fmt.Sprintf("a list whose entry %d is %s", i, what)
			}
		}
		return ""
	case string:
		name, whole, ok := fedText(value)
		if !ok {
			return whatText(value)
		}
		if !whole {
			return ""
		}
		if declared, batchless := inputTypes(wf, name).batchless(); batchless {
			return "the workflow input " + name + ", which its schema declares of type " + declared
		}
		return ""
	}
	return kindOf(v)
}

// notAnItem is notABatch for one entry of a list, which is one item, so an object and nothing
// else: a list inside a list is no item, nor is null.
func notAnItem(wf *Workflow, v any) string {
	switch value := v.(type) {
	case map[string]any:
		return ""
	case string:
		name, whole, ok := fedText(value)
		if !ok {
			return whatText(value)
		}
		if !whole {
			return ""
		}
		if declared, itemless := inputTypes(wf, name).itemless(); itemless {
			return "the workflow input " + name + ", which its schema declares of type " + declared
		}
		return ""
	}
	return kindOf(v)
}

// fedText reads a string fed to a port. ok is false where it is text, written as it is or with
// expressions embedded in it, which is always text; otherwise one expression fills it and keeps
// its type, and whole says whether that expression is a workflow input and nothing else, the
// input's name beside it.
func fedText(s string) (name string, whole, ok bool) {
	if !strings.Contains(s, "${{") {
		return "", false, false
	}
	t, err := expr.Interpolate(expr.ScopeStep, s)
	if err != nil {
		// Refused by checkExpressions before this is reached; an expression that does not
		// compile says nothing of its type.
		return "", false, true
	}
	if !t.Whole() {
		return "", false, false
	}
	m := inputReference.FindStringSubmatch(strings.TrimSpace(t.Programs()[0].Source()))
	for _, group := range m[min(len(m), 1):] {
		if group != "" {
			return group, true, true
		}
	}
	return "", false, true
}

// whatText is what text fed to a port is, which an embedded expression is converted to.
func whatText(s string) string {
	if strings.Contains(s, "${{") {
		return "text, since an expression embedded in a string is converted to text"
	}
	return kindOf(s)
}

// inputReference is an expression reading one workflow input and nothing else: dotted, with the
// names a dotted segment writes, a hyphen between two words among them, or in backticks, or
// bracketed and quoted, which writes any name. A hyphen followed by a digit is a subtraction, as
// the expression layer reads it, and is no reference.
var inputReference = regexp.MustCompile(`^workflow\.inputs(?:\.([A-Za-z0-9_]+(?:-[A-Za-z_][A-Za-z0-9_]*)*)|\.` + "`" + `([^` + "`" + `]+)` + "`" + `|\[\s*"([^"\\]+)"\s*\]|\[\s*'([^'\\]+)'\s*\])$`)

// declaredTypes is what a workflow input's schema says of the types its value takes, as far as
// one keyword says it: the type at its root and, for a list, the type of its items. Anything else
// a schema can say, a $ref, an anyOf, a const, narrows a value further and never widens it past
// type, which is why type alone can answer that a value is no batch, and why its absence answers
// nothing.
type declaredTypes struct {
	types []string
	items []string
}

// inputTypes reads the types an input declares, none where it is no input, has no schema, or
// a schema with no type.
func inputTypes(wf *Workflow, name string) declaredTypes {
	in, ok := wf.Inputs[name]
	if !ok || len(in.Schema) == 0 {
		return declaredTypes{}
	}
	var schema map[string]any
	if json.Unmarshal(in.Schema, &schema) != nil {
		return declaredTypes{}
	}
	d := declaredTypes{types: typeNames(schema["type"])}
	// A list whose first items prefixItems describes may hold an object there whatever items
	// says of the rest, so only items alone answers for every entry.
	if items, ok := schema["items"].(map[string]any); ok && schema["prefixItems"] == nil {
		d.items = typeNames(items["type"])
	}
	return d
}

// typeNames reads type, a name or a list of names, and none where it is neither.
func typeNames(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		names := make([]string, 0, len(t))
		for _, n := range t {
			name, ok := n.(string)
			if !ok {
				return nil
			}
			names = append(names, name)
		}
		return names
	}
	return nil
}

// batchless says whether no value the types admit is a batch, and what they are: a scalar, or a
// list whose items are no object. A list of them still holds a batch when it holds nothing, which
// is no reason to accept one a run refuses as soon as it holds anything.
func (d declaredTypes) batchless() (string, bool) {
	if len(d.types) == 0 {
		return "", false
	}
	for _, t := range d.types {
		switch t {
		case "string", "number", "integer", "boolean":
		case "array":
			if len(d.items) == 0 || slices.Contains(d.items, "object") || !known(d.items) {
				return "", false
			}
		default:
			return "", false
		}
	}
	return d.String(), true
}

// itemless says whether no value the types admit is an item, which only an object is.
func (d declaredTypes) itemless() (string, bool) {
	if len(d.types) == 0 || slices.Contains(d.types, "object") || !known(d.types) {
		return "", false
	}
	return d.String(), true
}

// known says whether every name is one of JSON Schema's seven types, since a name that is none
// says nothing of a value.
func known(types []string) bool {
	for _, t := range types {
		switch t {
		case "string", "number", "integer", "boolean", "null", "array", "object":
		default:
			return false
		}
	}
	return true
}

// String writes the types as a refusal names them: integer, string or null, array, whose items
// are of type string.
func (d declaredTypes) String() string {
	s := strings.Join(d.types, " or ")
	if slices.Contains(d.types, "array") && len(d.items) > 0 {
		s += ", whose items are of type " + strings.Join(d.items, " or ")
	}
	return s
}

// writtenMaxBytes is how much of a value a refusal writes back in the object it suggests, so that
// a list of a thousand numbers is not written back whole.
const writtenMaxBytes = 80

// written is a value as a flow mapping can hold it, which is its JSON form: a string quoted, so
// that the braces of an expression are not read as the mapping's own.
func written(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil || b.Len() > writtenMaxBytes {
		return "..."
	}
	return strings.TrimSuffix(b.String(), "\n")
}
