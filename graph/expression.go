package graph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/expr"
)

// The expression layer's half of Check: every ${{ }} the file carries is compiled, in
// the position it was written in, while the workflow is being read rather than while a
// run is under way.
//
// Compiling is the check. Package expr builds one environment per position out of the
// exposed-context table, so a position declares the roots it exposes and no others, and
// three of the rules the corpus names fall out of the compiler rather than out of a
// guard a later refactor can forget: a root outside the ten is an undeclared reference,
// item outside a shard is an undeclared reference, and a secret outside params and
// secrets is an undeclared reference. What is left for this file is the language: which
// position each keyword sits in, the one condition a position cannot state, and the
// wording of every refusal.
//
// Where each keyword sits, read off the table's third column:
//
//	on.webhook[].map, on.event[].filter   the on block, which reads trigger and event
//	if                                    a step, read once for the step
//	inputs                                a step, because a port is fed before the
//	                                      step is divided into shards
//	params                                the params of a step, and of a sharded step
//	                                      where the step is divided
//
// The condition the position cannot state is item's. The table gives item and matrix one
// availability, Shard, and a step sharded per batch and a step sharded per matrix
// combination are both shards; only the fan_out says whether this shard is an item. So
// the environment admits item wherever there is a shard, and the rule that it is
// "available only under fan_out: item" is applied here, over the roots the compiled
// expression actually reads.
//
// Two readings are taken here.
//
// A step keyword that is not params reads no secret. The table exposes secrets to params
// and to secrets alone, so an if, and an inputs, is compiled in a position that does not
// declare it, and a condition reading a secret is refused by the compiler for the same
// reason the documentation gives: "the controller therefore never loads whole envelopes
// into memory in order to schedule", and it never resolves a secret in order to schedule
// either.
//
// A workflow whose includes have not been resolved has its fan-out judged leniently.
// Parse is given one document, so a step whose strategy arrives from a block an included
// file carries has no fan_out in hand yet; compiling its params in the narrow position
// would refuse ${{ matrix.region }} on a file the language accepts. Such a step is
// compiled in the widest position and the item rule is left to Load, which has every
// block and settles it. A file that includes nothing is settled at Parse and is held to
// both.

// checkExpressions compiles every expression the workflow carries and holds each one to
// the rules the exposed-context table states. It is the last of Check's list, so a
// workflow whose graph does not hold together is refused by the graph first.
func checkExpressions(wf *Workflow) error {
	for i, hook := range wf.On.Webhook {
		for _, name := range slices.Sorted(maps.Keys(hook.Map)) {
			at := fmt.Sprintf("on.webhook[%d].map.%s", i, name)
			if err := holdValue(wf, expr.ScopeTrigger, nil, "", hook.Map[name], at); err != nil {
				return err
			}
		}
	}
	for i, event := range wf.On.Event {
		at := fmt.Sprintf("on.event[%d].filter", i)
		if err := holdValue(wf, expr.ScopeTrigger, nil, "", event.Filter, at); err != nil {
			return err
		}
	}

	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		where := "steps." + string(name)

		if err := holdValue(wf, expr.ScopeStep, &st, name, st.If, where+".if"); err != nil {
			return err
		}
		for _, port := range slices.Sorted(maps.Keys(st.Inputs)) {
			at := where + ".inputs." + string(port)
			if err := holdValue(wf, expr.ScopeStep, &st, name, st.Inputs[port], at); err != nil {
				return err
			}
		}
		scope := paramsScope(wf, st)
		for _, key := range slices.Sorted(maps.Keys(st.Params)) {
			at := where + ".params." + key
			if err := holdValue(wf, scope, &st, name, st.Params[key], at); err != nil {
				return err
			}
		}
	}
	return nil
}

// paramsScope says which of the two params positions a step's parameters sit in. A step
// that is divided reads the shard's own item and matrix beside everything the unsharded
// position reads; a step that runs as one container has no shard to be the current one.
func paramsScope(wf *Workflow, st Step) expr.Scope {
	if fanOutOf(&st) != FanOutNone || fanOutStillToArrive(wf, st) {
		return expr.ScopeShardParams
	}
	return expr.ScopeParams
}

// fanOutStillToArrive says whether a step's strategy could still come from a block this
// document has not been given. It is true only between Parse and Load, and only for a
// step that extends something, which is exactly the case where the file does not yet say
// how the step is divided.
func fanOutStillToArrive(wf *Workflow, st Step) bool {
	return !wf.resolved && st.Extends != ""
}

// holdValue compiles every expression one value carries. A parameter written as a map or
// a list carries its expressions inside it, and each one is written in the same position
// as the value it sits in.
func holdValue(wf *Workflow, sc expr.Scope, st *Step, name agk.Step, v any, at string) error {
	switch value := v.(type) {
	case string:
		if !strings.Contains(value, "${{") {
			return nil
		}
		return holdExpression(wf, sc, st, name, value, at)
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			if err := holdValue(wf, sc, st, name, value[key], at+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for i, sub := range value {
			if err := holdValue(wf, sc, st, name, sub, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdExpression compiles one string value and applies the rule the position cannot
// state.
func holdExpression(wf *Workflow, sc expr.Scope, st *Step, name agk.Step, value, at string) error {
	t, err := expr.Interpolate(sc, value)
	if err != nil {
		return expressionRefusal(wf, name, at, value, err)
	}
	if st == nil || fanOutOf(st) == FanOutItem || fanOutStillToArrive(wf, *st) {
		return nil
	}
	for _, p := range t.Programs() {
		if !slices.Contains(p.Roots(), expr.RootItem) {
			continue
		}
		return refuse(RuleExpressionItemOutsideFanOutItem, name, "", fmt.Sprintf(
			"%s reads item where the step is divided %s: item is the current item and is available only under fan_out: item, because item contents are not exposed to controller expressions and a test reads port metadata instead, inputs.in.count and inputs.in.empty%s",
			at, fanOutOf(st), writtenAt(wf, at)))
	}
	return nil
}

// expressionRefusal says which rule an expression broke, in the words the documentation
// states it in.
//
// The compiler's answer to all three is one answer, an undeclared reference, because one
// environment per position is what enforces the table. Which of the rules that is, is
// the language's to say, and it is said by the name the expression read: a name that is
// not one of the ten at all, item where there is no item, or a secret somewhere a secret
// is not exposed.
func expressionRefusal(wf *Workflow, name agk.Step, at, value string, err error) error {
	var undeclared *expr.UndeclaredRoot
	switch {
	case errors.As(err, &undeclared):
		switch undeclared.Name {
		case expr.RootItem.String():
			return refuse(RuleExpressionItemOutsideFanOutItem, name, "", fmt.Sprintf(
				"%s reads item, which %s does not expose: item is the current item and is available only under fan_out: item, because item contents are not exposed to controller expressions and a test reads port metadata instead, inputs.in.count and inputs.in.empty%s",
				at, undeclared.Scope, writtenAt(wf, at)))
		case expr.RootSecrets.String():
			return refuse(RuleExpressionSecretInCondition, name, "", fmt.Sprintf(
				"%s reads a secret, and %s does not expose one: secrets is exposed to params and to secrets and nowhere else, and what it exposes there is an opaque reference and never a value%s",
				at, undeclared.Scope, writtenAt(wf, at)))
		}
		return refuse(RuleExpressionUnknownRoot, name, "", fmt.Sprintf(
			"%s reads %s: %s, and the table of roots is closed%s",
			at, undeclared.Name, undeclared.Error(), writtenAt(wf, at)))
	case errors.Is(err, expr.ErrSecretOpaque):
		return refuse(RuleExpressionSecretInText, name, "", fmt.Sprintf(
			"%s puts a secret in a string: %v. A secret is an opaque reference, so the expression that reads one fills the whole value or nothing at all%s",
			at, err, writtenAt(wf, at)))
	default:
		return refuse(RuleExpressionDoesNotCompile, name, "", fmt.Sprintf(
			"%s is not an expression this engine can evaluate: %v%s", at, err, writtenAt(wf, at)))
	}
}

// writtenAt says where in the file a value was written, so that a person holding the
// file is sent to the line rather than to the key. It says nothing where the document
// does not carry a position, which is what a workflow built by a caller rather than read
// from a file would give.
func writtenAt(wf *Workflow, at string) string {
	line, column := position(wf.doc, "$."+at)
	if line == 0 {
		return ""
	}
	return fmt.Sprintf(", written at line %d column %d", line, column)
}
