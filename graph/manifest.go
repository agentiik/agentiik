package graph

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/schema"
)

// The rules that hold a step and the brick it runs together. They are the workflow's half
// of a contract whose other half is /agk/brick.yaml: the manifest says what the brick
// reads, writes and needs, and the step says what it feeds, publishes and supplies.
//
// They live here and not in package brick because a refusal has to name the step, which
// is something a manifest has never heard of, and because a manifest is read once per
// image while these are read once per step.

// checkAgainstManifest holds one step to the manifest of its image.
//
// A step with no manifest to be held to is passed over, and there are two of them, each
// for a reason the documentation states. A script step's image "is treated as a base
// image and nothing about its ports is inferred", so there is nothing to be a subset of;
// a sub-workflow call runs no container of its own.
func checkAgainstManifest(name agk.Step, st Step, manifests map[string]brick.Manifest) error {
	if st.Image == "" || len(st.Script) > 0 {
		return nil
	}
	m, ok := manifests[st.Image]
	if !ok {
		return refuse(RuleManifestMissing, name, "", fmt.Sprintf("the manifest of %s was not handed in: reading /agk/brick.yaml means pulling an image, and pulling an image is executing, so the manifests are fetched by the caller between Check and Build and Images says which ones", st.Image))
	}

	if err := outputsInManifest(name, st, m); err != nil {
		return err
	}
	if err := inputsInManifest(name, st, m); err != nil {
		return err
	}
	// Only what the file already settles is checked here. A parameter written as an
	// expression is a value the run has, not a value the file has, so it is validated
	// where it is resolved, against the same schemas, by the same function.
	return paramsAgainstManifest(name, st, m, st.Params, false)
}

// outputsInManifest is the subset rule: "outputs must be a subset of the ports in the
// brick manifest".
//
// One port is exempt, and it is the one the engine writes itself. A key join publishes
// what it could not match on the step's own unmatched port, "without the container ever
// touching it, which is why a step doing a join declares it in outputs". Holding that
// port to the manifest would ask a brick to declare a port it never writes.
func outputsInManifest(name agk.Step, st Step, m brick.Manifest) error {
	declared := m.OutputPorts()
	for _, port := range st.Outputs {
		if slices.Contains(declared, port) {
			continue
		}
		if port == unmatchedPort && st.Merge == MergeJoin {
			continue
		}
		carries := "no output port"
		if len(declared) > 0 {
			carries = portList(declared)
		}
		return refuse(RuleStepOutputNotInManifest, name, port, fmt.Sprintf("the step declares the output %s and the manifest of %s carries %s: outputs must be a subset of the ports in the brick manifest", port, m.Metadata.Name, carries))
	}
	return nil
}

// inputsInManifest holds every port a step feeds to the ports the brick reads. "The
// runner mounts one directory per port the brick declares, and there is no directory for
// a port the manifest never named."
//
// A port is fed two ways and both are held to it: by the inputs keyword, which "feeds a
// port from a workflow input or an expression, with no dependency on another step", and
// by the as of an edge, which is the port an upstream output arrives on.
func inputsInManifest(name agk.Step, st Step, m brick.Manifest) error {
	declared := m.InputPorts()
	fed := make([]agk.Port, 0, len(st.Inputs)+len(st.Needs))
	fed = append(fed, slices.Sorted(maps.Keys(st.Inputs))...)
	for _, e := range st.Needs {
		if !slices.Contains(fed, e.As) {
			fed = append(fed, e.As)
		}
	}
	for _, port := range fed {
		if slices.Contains(declared, port) {
			continue
		}
		carries := "no input port"
		if len(declared) > 0 {
			carries = portList(declared)
		}
		return refuse(RuleStepInputPortNotInManifest, name, port, fmt.Sprintf("the step feeds %s and the manifest of %s carries %s: the runner mounts one directory per port the brick declares, and there is no directory for a port the manifest never named", port, m.Metadata.Name, carries))
	}
	return nil
}

// paramsAgainstManifest holds a step's parameters to what the manifest says about them:
// that it declares them, that what it declares required is supplied, and that each value
// satisfies the schema written beside it.
//
// resolved says which end of the run this is. Before a run, a parameter written as an
// expression is a value nobody has yet, so only what the file settles is checked and the
// rest waits; when a task is built, every expression has a value and every parameter is
// checked. The rules are the same in both places, which is the point of one function:
// "params: brick parameters, validated against the manifest schema once expressions are
// resolved".
func paramsAgainstManifest(name agk.Step, st Step, m brick.Manifest, params map[string]any, resolved bool) error {
	for _, key := range slices.Sorted(maps.Keys(params)) {
		if _, ok := m.Spec.Params[key]; !ok {
			declares := "no parameter"
			if len(m.Spec.Params) > 0 {
				declares = strings.Join(slices.Sorted(maps.Keys(m.Spec.Params)), ", ")
			}
			return refuse(RuleParamsAgainstManifest, name, "", fmt.Sprintf("the step supplies the parameter %s and the manifest of %s declares %s: a parameter is read from /agk/params.json by the brick that declared it, and one it never declared is one nothing reads", key, m.Metadata.Name, declares))
		}
	}

	for _, key := range slices.Sorted(maps.Keys(m.Spec.Params)) {
		declared := m.Spec.Params[key]
		value, supplied := params[key]
		if !supplied {
			if declared.Required {
				return refuse(RuleParamsAgainstManifest, name, "", fmt.Sprintf("the manifest of %s declares the parameter %s required and the step supplies no value: a missing one is refused when the workflow is validated rather than inside a container", m.Metadata.Name, key))
			}
			continue
		}
		if !resolved && carriesExpression(value) {
			// The value is an expression, so what it will be is the run's to say. It
			// is checked again, against this same schema, when the task is built.
			continue
		}
		if secret, ok := value.(SecretParam); ok {
			// "sensitive marks a parameter that may carry a secret value, and is
			// the only route by which a secret reaches a brick as a parameter
			// rather than as a mounted file." A parameter the manifest has not
			// marked is not that route. There is nothing to hold to the schema
			// either way: what travels is the name the workflow declared, and the
			// value is put in /agk/params.json by the runner, in the container.
			if !declared.Sensitive {
				return refuse(RuleParamsAgainstManifest, name, "", fmt.Sprintf("the parameter %s is given the secret %s and the manifest of %s does not mark it sensitive: sensitive is the only route by which a secret reaches a brick as a parameter rather than as a mounted file", key, secret.Secret, m.Metadata.Name))
			}
			continue
		}
		if len(declared.Schema) == 0 {
			continue
		}
		s, err := schema.NewCompiler(nil).CompileAt(m.Document(), "#/spec/params/"+key)
		if err != nil {
			return fmt.Errorf("graph: step %s: the manifest of %s declares the parameter %s with a schema that does not compile: %w", name, m.Metadata.Name, key, err)
		}
		if err := s.Validate(value); err != nil {
			return refuse(RuleParamsAgainstManifest, name, "", fmt.Sprintf("the parameter %s departs from the schema the manifest of %s declares for it: %v", key, m.Metadata.Name, err))
		}
	}
	return nil
}

// carriesExpression says whether a value is one the run has yet to settle.
//
// It is the ${{ }} delimiter and nothing more: what the expression says, which roots it
// may read and what it evaluates to are the expression layer's, and the only question
// here is whether the file already knows the value. A parameter that is a map or a list
// carrying an expression anywhere inside it is such a value too, because the whole of it
// is what the schema will be shown.
func carriesExpression(v any) bool {
	switch value := v.(type) {
	case string:
		return strings.Contains(value, "${{")
	case map[string]any:
		for _, sub := range value {
			if carriesExpression(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range value {
			if carriesExpression(sub) {
				return true
			}
		}
	}
	return false
}
