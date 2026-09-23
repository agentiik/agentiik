package expr

import (
	"fmt"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Context is what an expression is evaluated against: the ten roots, as ten named
// fields. A field and not a map entry, because a root that is not set is then an absence
// the compiler can see rather than a lookup that returns nothing at the moment the
// expression runs. The evaluator fills the fields the position exposes; the rest stay
// zero and are never declared, so an expression could not have read them.
//
// Everything here is a document value: maps of strings to values that came out of JSON.
// Nothing in this package knows what a step, a port or an envelope is, and nothing here
// holds the contents of one.
type Context struct {
	// Workflow is name, namespace, version and inputs. inputs here is the run's
	// validated workflow inputs, which is what workflow.inputs.orders reads.
	Workflow map[string]any

	// Run is id, started_at, attempt, trigger_kind and triggered_by.
	Run map[string]any

	// Trigger is body, headers, query and scheduled_for: what started the run.
	Trigger map[string]any

	// Event is the CloudEvents 1.0 attributes of the event an event trigger matched.
	Event map[string]any

	// Vars is the workflow and namespace variables, already merged.
	Vars map[string]any

	// Inputs is the metadata of the current step's input ports, by port name. It is
	// metadata and never contents: that is the rule the whole table is shaped by.
	Inputs map[string]PortMeta

	// Steps is the status and output port metadata of the steps already decided, by
	// step name.
	Steps map[string]StepMeta

	// Item is the current item, in the document form it travels in, and it is set
	// only where the step is sharded per item.
	Item any

	// Matrix is the current combination, one value per matrix key.
	Matrix map[string]any

	// Secrets is the secrets the workflow names, by name, as references. A Secret
	// carries a name and never a value: the value is resolved by the runner, in the container,
	// and never in the process that evaluates this.
	Secrets map[string]Secret
}

// PortMeta is the whole of what an expression may see of a port: how many items it
// carries, whether it is empty, and how many bytes it is. Contents are deliberately
// absent. The controller therefore never loads an envelope in order to schedule, which
// is what keeps it from growing with throughput and keeps one namespace's business data
// out of a process that serves every namespace.
type PortMeta struct {
	Count int
	Empty bool
	Bytes int64
}

// StepMeta is what steps.<id> exposes: the step's status, and the same port metadata for
// each of its output ports.
type StepMeta struct {
	Status  string
	Outputs map[string]PortMeta
}

// activation gives the evaluator the roots this position declares and no others. A root
// the position does not expose is not in the activation at all, so a Context carrying
// more than the position allows cannot widen what an expression already compiled against.
func (c Context) activation(sc Scope) map[string]any {
	act := make(map[string]any, len(scopeRoots[sc]))
	for _, r := range scopeRoots[sc] {
		switch r {
		case RootWorkflow:
			act[r.String()] = c.Workflow
		case RootRun:
			act[r.String()] = c.Run
		case RootTrigger:
			act[r.String()] = c.Trigger
		case RootEvent:
			act[r.String()] = c.Event
		case RootVars:
			act[r.String()] = c.Vars
		case RootInputs:
			act[r.String()] = ports(c.Inputs)
		case RootSteps:
			act[r.String()] = steps(c.Steps)
		case RootItem:
			act[r.String()] = c.Item
		case RootMatrix:
			act[r.String()] = c.Matrix
		case RootSecrets:
			act[r.String()] = secrets(c.Secrets)
		}
	}
	return act
}

// ports writes port metadata the way an expression reads it. The struct is the shape a
// caller fills; the map is the shape inputs.in.count selects through, and building it
// here means the caller never writes the three key names itself.
func ports(in map[string]PortMeta) map[string]any {
	out := make(map[string]any, len(in))
	for name, p := range in {
		out[name] = map[string]any{
			"count": int64(p.Count),
			"empty": p.Empty,
			"bytes": p.Bytes,
		}
	}
	return out
}

// steps writes step metadata the way steps.<id>.status and steps.<id>.outputs.<port>
// read it.
func steps(in map[string]StepMeta) map[string]any {
	out := make(map[string]any, len(in))
	for name, s := range in {
		out[name] = map[string]any{
			"status":  s.Status,
			"outputs": ports(s.Outputs),
		}
	}
	return out
}

// secrets widens the map so that the adapter sees each value as the opaque CEL value it
// is. A map[string]Secret would be reflected over rather than adapted entry by entry.
func secrets(in map[string]Secret) map[string]any {
	out := make(map[string]any, len(in))
	for name, s := range in {
		out[name] = s
	}
	return out
}

// native turns an evaluated value into the Go value the workflow language carries: the
// document types, plus a timestamp and a duration because CEL has them, plus a Secret
// because a secret filling a whole value is a reference the caller passes on.
//
// It is written out rather than left to reflection so that what can come out of an
// expression is a list in one place: a value that leaves here is about to be written into
// a task's params, and a value with no document form has no business being there.
func native(v ref.Val) (any, error) {
	if v == nil {
		return nil, nil
	}
	if types.IsError(v) {
		if err, ok := v.Value().(error); ok {
			return nil, err
		}
		return nil, fmt.Errorf("the expression evaluated to an error")
	}
	if types.IsUnknown(v) {
		return nil, fmt.Errorf("the expression evaluated to an unknown value, which means a root was not set")
	}
	switch v := v.(type) {
	case Secret:
		return v, nil
	case types.Bool:
		return bool(v), nil
	case types.Bytes:
		return []byte(v), nil
	case types.Double:
		return float64(v), nil
	case types.Int:
		return int64(v), nil
	case types.String:
		return string(v), nil
	case types.Uint:
		return uint64(v), nil
	case types.Duration:
		return v.Duration, nil
	case types.Timestamp:
		return v.Time, nil
	case types.Null:
		return nil, nil
	}
	switch v := v.(type) {
	case traits.Lister:
		size, ok := v.Size().(types.Int)
		if !ok {
			return nil, fmt.Errorf("the expression evaluated to a list of no known length")
		}
		out := make([]any, 0, int(size))
		for i := types.Int(0); i < size; i++ {
			elem, err := native(v.Get(i))
			if err != nil {
				return nil, err
			}
			out = append(out, elem)
		}
		return out, nil
	case traits.Mapper:
		out := make(map[string]any)
		for it := v.Iterator(); it.HasNext() == types.True; {
			key := it.Next()
			name, err := mapKey(key)
			if err != nil {
				return nil, err
			}
			value, err := native(v.Get(key))
			if err != nil {
				return nil, err
			}
			out[name] = value
		}
		return out, nil
	}
	return nil, fmt.Errorf("the expression evaluated to a %s, which has no document form", v.Type().TypeName())
}

// mapKey names a map key the way a document names one. CEL allows an integer or a
// boolean key and a document does not, so the key is written in its own text form, which
// is the reading that keeps a map usable as a value rather than refusing it whole.
func mapKey(key ref.Val) (string, error) {
	if s, ok := key.(types.String); ok {
		return string(s), nil
	}
	text := key.ConvertToType(types.StringType)
	if s, ok := text.(types.String); ok {
		return string(s), nil
	}
	return "", fmt.Errorf("a map key of type %s has no document form", key.Type().TypeName())
}
