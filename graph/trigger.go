package graph

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/internal/cron"
	"github.com/agentiik/agentiik/internal/expr"
)

// newTriggerRules are the shape rules of v0.5.0 about the on block, which the schema states and a
// version stored before them may break: a schedule's five fields written in cron's grammar and its
// zone as a name, never Local; a webhook's path in segments no proxy normalises out of its prefix;
// a sync response naming the output it answers with; and an event trigger naming something it
// listens for, a type, a source or a filter, since one naming none would start a run on every
// event its namespace can see. They are newRules', applied where a
// version is made, so that a version stored before them keeps rebuilding and running; whether its
// triggers can be armed is Armable's to say.
func (w *Workflow) newTriggerRules() error {
	for i, s := range w.On.Schedule {
		where := fmt.Sprintf("on.schedule[%d]", i)
		if !cronShape.MatchString(s.Cron) {
			return fmt.Errorf("%s.cron is %q, and a field of a five-field cron is *, a value, a range a-b, * or a range stepped by /n, or a list of those: ? and L are other schedulers' grammars, and a value stepped alone, 5/15, means 5-59/15 to some crons and nothing to others", where, s.Cron)
		}
		switch {
		case s.Timezone == "Local":
			return fmt.Errorf("%s.timezone is Local, the installation's own zone, and one file would then fire at different instants on two installations: name the zone, Europe/Paris, or leave it out for UTC", where)
		case s.Timezone != "" && (len(s.Timezone) > 64 || !zoneName.MatchString(s.Timezone)):
			return fmt.Errorf("%s.timezone is %q, which is not written as a zone of the IANA time zone database, Europe/Paris or UTC: an offset such as +02:00 does not move with daylight saving, which is what a named zone is for", where, s.Timezone)
		}
	}
	for i, h := range w.On.Webhook {
		where := fmt.Sprintf("on.webhook[%d]", i)
		if len(h.Path) > 255 || !webhookPath.MatchString(h.Path) {
			return fmt.Errorf("%s.path is %q: a webhook path is segments each beginning with a slash, then a letter, a digit, _, ~ or -, at most 255 characters, so that no segment is . or .. and no path climbs out of /hooks/<namespace>/ once a proxy normalises it", where, h.Path)
		}
		if h.Response == "sync" && h.Output == "" {
			return fmt.Errorf("%s answers response: sync and names no output: a sync response returns one workflow output, and output says which", where)
		}
	}
	for i, e := range w.On.Event {
		if e.Type == "" && e.Source == "" && e.Filter == "" {
			return fmt.Errorf("on.event[%d] names no type, no source and no filter, and would start a run on every event its namespace can see: a subscription names what it listens for", i)
		}
	}
	return nil
}

// Armable says whether the triggers a workflow declares can be armed: held to the rules a version
// is made by today, whatever its version was stored under. A version stored before them keeps
// running when somebody asks for a run, and its schedules, webhooks and events are armed once a
// version that passes lands on the default branch.
func (w *Workflow) Armable() error {
	if err := w.newTriggerRules(); err != nil {
		return err
	}
	return triggersHold(w)
}

// checkTriggers holds the on block to the rules the schema cannot state: that each schedule
// names occurrences that come, read in a zone the time zone database holds, and that each
// webhook answers with, and fills, what the workflow declares, one trigger to a path and a
// method. They arrived with v0.5.0, so a version stored before them is not held to them where it
// is read back, and keeps rebuilding and running; Armable holds it to them all the same before
// its triggers are armed.
func checkTriggers(wf *Workflow) error {
	if !wf.made {
		return nil
	}
	return triggersHold(wf)
}

func triggersHold(wf *Workflow) error {
	on := origin{src: wf.src, path: []any{"on"}}
	for i, s := range wf.On.Schedule {
		at := on.at("schedule", i)
		if _, err := cron.Parse(s.Cron); err != nil {
			return place(refuse(RuleCronValueOutOfRange, "", "", fmt.Sprintf("the schedule %d names an occurrence that never comes: %v", i, err)), at.at("cron").value())
		}
		if _, err := cron.Zone(s.Timezone); err != nil {
			return place(refuse(RuleTimezoneUnknown, "", "", fmt.Sprintf("the schedule %d is read in a zone that cannot be read: %v", i, err)), at.at("timezone").value())
		}
	}

	answered := map[[2]string]int{}
	for i, w := range wf.On.Webhook {
		at := on.at("webhook", i)
		if w.Output != "" {
			if _, ok := wf.Outputs[w.Output]; !ok {
				return place(refuse(RuleWebhookOutputNotDeclared, "", "", fmt.Sprintf("the webhook %s answers with the output %s, which the workflow does not declare: a sync response returns one of the workflow's own outputs, %s", w.Path, w.Output, declared(wf.Outputs))), at.at("output").value())
			}
		}
		for _, name := range keysOf(w.Map) {
			if _, ok := wf.Inputs[name]; !ok {
				return place(refuse(RuleWebhookMapInputNotDeclared, "", "", fmt.Sprintf("the webhook %s fills the input %s, which the workflow does not declare: map builds the run's inputs, each validated against its schema before the run exists, and an input nobody declared has none, %s", w.Path, name, declared(wf.Inputs))), at.at("map", name).key())
			}
		}
		pair := [2]string{w.Path, w.Method}
		if first, ok := answered[pair]; ok {
			return place(refuse(RuleWebhookDuplicatePath, "", "", fmt.Sprintf("the webhooks %d and %d both answer %s %s: a request would start whichever the engine found first, so a path and a method answer one trigger", first, i, w.Method, w.Path)), at.at("path").value())
		}
		answered[pair] = i
	}
	return nil
}

// declared says what a workflow declares under inputs or outputs, for a refusal naming one it
// does not.
func declared[V any](m map[string]V) string {
	if len(m) == 0 {
		return "and it declares none"
	}
	return "and it declares " + strings.Join(slices.Sorted(maps.Keys(m)), ", ")
}

// Fired is what a trigger's expressions read of the run it fires before the run exists: the
// request or the occurrence under trigger, the event under event, and the kind and the principal
// under run. The run's id and start are not yet, and an expression of the on block reading them is
// refused where it is evaluated.
type Fired struct {
	Commit      string
	Trigger     map[string]any
	Event       map[string]any
	TriggerKind string
	TriggeredBy string
}

// Fill evaluates what an entry of the on block fills the workflow inputs with, its map, over what
// fired it: "Build workflow inputs with map over the trigger context". A string carrying an
// expression is evaluated, and one filling the whole value keeps its type; a map or a list is
// evaluated through; anything else is the literal the file wrote. What it answers is bound by the
// one path every run takes, which holds each input to its schema before the run exists.
func (g *Graph) Fill(m map[string]any, f Fired) (map[string]any, error) {
	wf := g.wf
	c := expr.Context{
		Workflow: map[string]any{
			"name": wf.Metadata.Name, "namespace": wf.Metadata.Namespace, "version": f.Commit,
			// The inputs are what this fills: read here, they are the ones nothing filled yet.
			"inputs": map[string]any{},
		},
		Run:     map[string]any{"attempt": int64(1), "trigger_kind": f.TriggerKind, "triggered_by": f.TriggeredBy},
		Trigger: f.Trigger,
		Event:   f.Event,
		Vars:    map[string]any(wf.Vars),
	}
	out := make(map[string]any, len(m))
	for _, name := range slices.Sorted(maps.Keys(m)) {
		v, err := fill(c, m[name])
		if err != nil {
			return nil, fmt.Errorf("map.%s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// fill evaluates one value of a map, through the maps and lists it holds.
func fill(c expr.Context, v any) (any, error) {
	switch value := v.(type) {
	case string:
		if !strings.Contains(value, "${{") {
			return value, nil
		}
		t, err := expr.Interpolate(expr.ScopeWebhook, value)
		if err != nil {
			return nil, err
		}
		return expr.Evaluate(t, c)
	case map[string]any:
		out := make(map[string]any, len(value))
		for _, key := range slices.Sorted(maps.Keys(value)) {
			resolved, err := fill(c, value[key])
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(value))
		for _, elem := range value {
			resolved, err := fill(c, elem)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	}
	return v, nil
}
