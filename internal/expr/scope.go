package expr

import (
	"strconv"
	"strings"
)

// Root is one root of the exposed-context table. There are ten and the table is closed:
// an expression reading an eleventh name is reading something the language does not
// define, and that is a refusal rather than an empty value.
type Root int

// The ten roots, in the order the table lists them. Their contents are the table's
// second column, quoted here so that a reader of this file need not hold the
// documentation open beside it.
const (
	RootWorkflow Root = iota + 1 // name, namespace, version, inputs
	RootRun                      // id, started_at, attempt, trigger_kind, triggered_by
	RootTrigger                  // body, headers, query, scheduled_for
	RootEvent                    // the CloudEvents 1.0 attributes, data among them
	RootVars                     // workflow and namespace variables
	RootInputs                   // metadata of the current step's input ports
	RootSteps                    // steps.<id>.status and steps.<id>.outputs.<port>
	RootItem                     // the current item, under fan_out: item alone
	RootMatrix                   // the current combination
	RootSecrets                  // opaque references, never values
)

// rootNames is the spelling an expression writes each root under. It is indexed by the
// Root, so that the constant order above and the names here cannot drift apart.
var rootNames = [...]string{
	RootWorkflow: "workflow",
	RootRun:      "run",
	RootTrigger:  "trigger",
	RootEvent:    "event",
	RootVars:     "vars",
	RootInputs:   "inputs",
	RootSteps:    "steps",
	RootItem:     "item",
	RootMatrix:   "matrix",
	RootSecrets:  "secrets",
}

// String gives the root the name an expression writes it under.
func (r Root) String() string {
	if r > 0 && int(r) < len(rootNames) {
		return rootNames[r]
	}
	return "root(" + strconv.Itoa(int(r)) + ")"
}

// ParseRoot reads a root by the name an expression writes it under, and says whether
// that name is one of the ten. A caller holding a name that came out of an expression
// asks this rather than comparing against a list of its own.
func ParseRoot(name string) (Root, bool) {
	for i, n := range rootNames {
		if n != "" && n == name {
			return Root(i), true
		}
	}
	return 0, false
}

// Scope is a position in the workflow file, named by what that position may read. It is
// the table's third column, which is why there are five of them and not one per keyword:
// the table distinguishes the on block, a step, the shard of a step, and the two
// positions secrets reaches.
type Scope int

// The five positions the table distinguishes.
const (
	// ScopeTrigger is the on block. trigger is the request or the schedule that
	// started the run, and event is the CloudEvents document an event trigger
	// matched, which is why both are here and neither is anywhere else in this list
	// except where the table says so.
	ScopeTrigger Scope = iota + 1

	// ScopeStep is a step keyword read once for the step, if and when among them.
	// It reads port metadata and the status of other steps, and it reads no secret:
	// that is the rule a condition reading a secret breaks.
	ScopeStep

	// ScopeParams is params and secrets of a step that is not sharded. It is the
	// one position besides ScopeShardParams where secrets is exposed, and it reads
	// trigger because the table says step parameters do.
	ScopeParams

	// ScopeShard is a step keyword read once per shard. It adds item and matrix,
	// which exist only where there is a shard to be the current one.
	ScopeShard

	// ScopeShardParams is params and secrets of a sharded step: what ScopeParams
	// reads, and the shard's own item and matrix with it.
	ScopeShardParams
)

// scopeRoots is the table's third column, read the other way round: what each position
// exposes rather than where each root is available. It is the whole of the rule, in one
// place, and a compilation environment is built from it and from nothing else.
//
// item and matrix sit together in the two shard positions because the table gives them
// one availability, Shard. The extra condition the table writes into item's contents,
// available only under fan_out: item, is not a position and cannot be a Scope: a step
// sharded per batch and a step sharded per matrix combination are both shards. A caller
// that knows the fan-out, which is package graph and not this package, holds an
// expression to that condition by reading Program.Roots, which names every root the
// expression actually reads.
var scopeRoots = map[Scope][]Root{
	ScopeTrigger: {RootWorkflow, RootRun, RootTrigger, RootEvent, RootVars},
	ScopeStep:    {RootWorkflow, RootRun, RootVars, RootInputs, RootSteps},
	ScopeParams:  {RootWorkflow, RootRun, RootTrigger, RootVars, RootInputs, RootSteps, RootSecrets},
	ScopeShard:   {RootWorkflow, RootRun, RootVars, RootInputs, RootSteps, RootItem, RootMatrix},
	ScopeShardParams: {
		RootWorkflow, RootRun, RootTrigger, RootVars, RootInputs, RootSteps,
		RootItem, RootMatrix, RootSecrets,
	},
}

// scopeNames names a position as an error message names it, which is by what an author
// would call the place they wrote the expression.
var scopeNames = map[Scope]string{
	ScopeTrigger:     "the on block",
	ScopeStep:        "a step",
	ScopeParams:      "the params and secrets of a step",
	ScopeShard:       "a sharded step",
	ScopeShardParams: "the params and secrets of a sharded step",
}

// Roots gives the roots this position exposes, in the table's order. The slice is the
// caller's own: the table is not reachable through it.
func (s Scope) Roots() []Root {
	roots := scopeRoots[s]
	out := make([]Root, len(roots))
	copy(out, roots)
	return out
}

// String names the position as an error message names it.
func (s Scope) String() string {
	if n, ok := scopeNames[s]; ok {
		return n
	}
	return "scope(" + strconv.Itoa(int(s)) + ")"
}

// declares says whether this position exposes that root. It is the whole of what
// compilation asks of the table.
func (s Scope) declares(r Root) bool {
	for _, have := range scopeRoots[s] {
		if have == r {
			return true
		}
	}
	return false
}

// rootList writes a set of roots the way a sentence ends on it, so that a refusal can
// say what the position does expose without a caller assembling the list itself.
func rootList(roots []Root) string {
	names := make([]string, len(roots))
	for i, r := range roots {
		names[i] = r.String()
	}
	switch len(names) {
	case 0:
		return "nothing"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
