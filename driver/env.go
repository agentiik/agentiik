package driver

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// The environment variables of the brick contract, in the order the table lists them.
// They are constants because the same spelling has to appear in the driver that sets
// them, in a test that reads them back and in nothing else: a variable named twice in
// two places is a variable that gets renamed in one.
const (
	EnvRunID     = "AGK_RUN_ID"
	EnvWorkflow  = "AGK_WORKFLOW"
	EnvNamespace = "AGK_NAMESPACE"
	EnvStep      = "AGK_STEP"
	EnvAttempt   = "AGK_ATTEMPT"
	EnvShard     = "AGK_SHARD"
	EnvDeadline  = "AGK_DEADLINE"
	EnvRepo      = "AGK_REPO"
	EnvCommit    = "AGK_COMMIT"
	EnvOutPorts  = "AGK_OUT_PORTS"

	// EnvParamPrefix is what a script step's parameters are exported under. It is
	// the one variable name the table does not list, because it is named where the
	// script keywords are: "params has no manifest to be validated against here, so
	// each entry is written to /agk/params.json and exported as AGK_PARAM_<NAME>".
	EnvParamPrefix = "AGK_PARAM_"
)

// environment is what the container reads its own identity from.
//
// A task is self-contained and a brick never sees the graph it came from, so everything
// here comes off the Task and nothing is looked up. The order is the table's order, so
// that a container's environment read in a diagnostic dump reads like the page.
//
// A variable with nothing to carry is absent rather than empty. The table already says
// so of one of them, AGK_SHARD being "absent when there is no fan-out", and the same
// reading applied to the rest is what lets a brick test for a value and get one answer
// instead of two.
//
// deadline is the one value that is not read off the Task, and it is an argument for the
// reason it cannot be: a task carrying a timeout and no moment lands on a moment only the
// dispatch fixes, and that is the moment the watch will stop the container at. Reading
// t.Deadline here instead would leave a brick told nothing while the runner still stops
// it, which is a stop at a moment nobody named in the one place a brick would look.
func environment(t graph.Task, deadline time.Time) []string {
	env := make([]string, 0, 10+len(t.Params))
	add := func(name, value string) {
		if value == "" {
			return
		}
		env = append(env, name+"="+value)
	}

	add(EnvRunID, string(t.Run))
	// The namespaced name and version, separated by @, which is what a run already
	// carries: finance/monthly-invoicing@a3f9c1e. Composing it here out of the
	// namespace and the workflow would namespace a name that is namespaced already.
	add(EnvWorkflow, t.Workflow)
	add(EnvNamespace, t.Namespace)
	add(EnvStep, string(t.Step))
	// An attempt is always numbered, from 1, so it is written whatever it says. A
	// brick reading AGK_ATTEMPT to tell a retry from a first run needs the number to
	// be there on the first run too.
	add(EnvAttempt, strconv.Itoa(t.Attempt))
	// Shard.String is empty for a task with no fan-out, which is exactly the case the
	// table calls absent, so the rule is agk's and is not restated here.
	add(EnvShard, t.Shard.String())
	add(EnvDeadline, deadlineText(deadline))
	// The mount root of the repository tree, which this driver always binds at
	// RepoDir. It is a variable rather than a constant inside the brick so that a
	// step reads the path it was given rather than one it assumed.
	add(EnvRepo, RepoDir)
	add(EnvCommit, t.Commit)
	add(EnvOutPorts, outPorts(t.Outputs))

	return append(env, params(t)...)
}

// deadlineText writes the moment the container will be stopped at, in UTC, "RFC 3339" as
// the table says. A task with no deadline carries no variable: an empty AGK_DEADLINE
// would be a promise of a stop at a moment nobody named.
func deadlineText(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// outPorts writes the declared output ports, comma separated, in the order the step
// declared them. That is the order brick.Collect is given and the order the workflow
// file wrote, so the variable and the collection agree about what this step publishes.
func outPorts(ports []agk.Port) string {
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, string(p))
	}
	return strings.Join(names, ",")
}

// params exports a script step's parameters, and only a script step's.
//
// The rule belongs to the script keywords and is read as narrowly as it is written.
// A brick declares its parameters in a manifest, where one may be marked sensitive, and
// they reach the container through /agk/params.json; putting them in the environment as
// well would put a sensitive value in the one place the secrets rule refuses to put a
// secret, "readable by its children" and in "diagnostic dumps". A script step has no
// manifest and therefore no sensitive parameter to expose, which is why the
// documentation grants the export exactly there.
//
// Sorted by name, so that a container's environment is the same twice for the same task.
func params(t graph.Task) []string {
	if !isScript(t) || len(t.Params) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.Params))
	for _, name := range slices.Sorted(maps.Keys(t.Params)) {
		variable := EnvParamPrefix + strings.ToUpper(name)
		if !isEnvName(variable) {
			// A parameter name is ^[A-Za-z_][A-Za-z0-9_]*$ precisely "because
			// the parameter becomes a key of /agk/params.json for the container
			// to read", and the schema refuses max-retries where it is written.
			// One that arrives here anyway is still delivered in that file; it
			// is only the export that has nowhere to go, since no shell can read
			// a variable it cannot spell.
			continue
		}
		out = append(out, variable+"="+paramValue(t.Params[name]))
	}
	return out
}

// isScript says whether this task runs commands rather than a brick's own entry point.
// Any of the three keywords makes it one, because all three run in the same container
// under the same shell.
func isScript(t graph.Task) bool {
	return len(t.Script) > 0 || len(t.BeforeScript) > 0 || len(t.AfterScript) > 0
}

// paramValue writes one parameter as a shell would want to read it.
//
// A string is exported as itself and never as a quoted JSON string, because the
// documentation's own example concatenates one into a URL:
// "$AGK_PARAM_ENDPOINT/check". Anything that is not a scalar is exported as compact
// JSON, which is what jq on the other side of the pipe expects and what
// /agk/params.json carries for the same value.
func paramValue(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case float64:
		// 'f' and not 'g': a parameter written 1000000 in the workflow file is
		// read back as 1e+06 by the exponent form, and a step that puts it in a
		// URL sends a number nobody wrote.
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case json.Number:
		return value.String()
	default:
		b, err := json.Marshal(value)
		if err != nil {
			// A parameter arrives resolved and validated against the manifest
			// schema, so it is a JSON value by the time it reaches here. One
			// that will not marshal has no text to export and the file is still
			// where it is read from.
			return ""
		}
		return string(b)
	}
}

// isEnvName says whether a name is one a process can carry and a shell can read.
func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
		if i > 0 {
			ok = ok || c >= '0' && c <= '9'
		}
		if !ok {
			return false
		}
	}
	return true
}
