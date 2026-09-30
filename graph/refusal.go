package graph

import (
	"errors"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// ErrRefused is what every refusal of this package unwraps to, so that a caller can tell
// a workflow the language refuses from a failure of its own machinery with one errors.Is.
var ErrRefused = errors.New("the workflow is refused")

// Rule is what a document is refused by, spelled as the fixture corpus spells it.
//
// The spelling is the point. The released corpus names, for every invalid document, the
// rule it holds a reader to, and a test that puts a fixture beside the rule it pins is
// then one lookup rather than a translation table somebody has to keep in step. The
// corpus marks each invalid workflow refused_by "schema" or "validator": everything
// marked schema is shape, which closed decoding gives for nothing and which is refused
// here by an ordinary error naming the key, and the twenty marked validator are, item
// for item, the rules below that carry a corpus name.
type Rule string

// The rules the corpus names. Each one is a sentence of the documentation that no JSON
// Schema can express, because it needs the graph, the brick manifest, the included file
// or the expression language.
const (
	// RuleCycleInGraph is "cycles are rejected at validation, before registration.
	// Looping is done by calling a sub-workflow".
	RuleCycleInGraph Rule = "cycle-in-graph"
	// RuleNeedsUnknownStep is an edge from a step the workflow does not declare. "An
	// edge is the only form of dependency there is", so it has to name something that
	// exists.
	RuleNeedsUnknownStep Rule = "needs-unknown-step"
	// RuleEdgePortNotDeclared is an edge taking an output port nobody declares.
	RuleEdgePortNotDeclared Rule = "edge-port-not-declared"
	// RuleStepOutputNotInManifest is a step declaring an output port its brick manifest
	// does not carry: "outputs must be a subset of the ports in the brick manifest".
	RuleStepOutputNotInManifest Rule = "step-output-not-in-manifest"
	// RuleStepInputPortNotInManifest is a step feeding an input port nobody declares.
	// "The runner mounts one directory per port the brick declares, and there is no
	// directory for a port the manifest never named."
	RuleStepInputPortNotInManifest Rule = "step-input-port-not-in-manifest"
	// RuleOutputFromUnknownStep is a workflow output taken from a step the workflow
	// does not declare.
	RuleOutputFromUnknownStep Rule = "output-from-unknown-step"
	// RuleSecretNotDeclared is a step mounting a secret the workflow never names.
	// "Secrets are mounted by the name the secrets block lists them under, and only
	// secrets the owning namespace declares can be named there."
	RuleSecretNotDeclared Rule = "secret-not-declared"
	// RuleScriptWithoutOutputs is a script step that declares no output port. "The
	// image is a base image, no manifest is read and nothing about its ports is
	// inferred, so outputs must be declared."
	RuleScriptWithoutOutputs Rule = "script-without-outputs"
	// RuleMCPToolInputNotDeclared is a published tool naming something the workflow
	// does not declare. "The reference is to a workflow input or output, never to a
	// step or a port."
	RuleMCPToolInputNotDeclared Rule = "mcp-tool-input-not-declared"
	// RuleMCPToolInputWithoutSchema is a tool whose input is a workflow input carrying
	// no schema. "A tool has to publish an inputSchema, so the input it maps has to
	// have one."
	RuleMCPToolInputWithoutSchema Rule = "mcp-tool-input-without-schema"
	// RuleMCPDuplicateToolName is two published tools carrying the same name. "A tool
	// identifier is unique within the workflow, because clients hold it."
	RuleMCPDuplicateToolName Rule = "mcp-duplicate-tool-name"
	// RuleMCPInIncludedFile is an mcp block in an included file. "Reuse applies to
	// steps and defaults; the published surface is declared by the workflow that
	// publishes it, so that reading one file tells you everything that workflow
	// exposes."
	RuleMCPInIncludedFile Rule = "mcp-in-included-file"
	// RuleExpressionUnknownRoot is an expression reading a context root that is not
	// exposed. The roots are the ten of the table and there is no eleventh.
	RuleExpressionUnknownRoot Rule = "expression-unknown-root"
	// RuleExpressionItemOutsideFanOutItem is an expression reading item where the step
	// is not sharded per item, "which is what keeps the controller from loading a
	// namespace's data in order to schedule".
	RuleExpressionItemOutsideFanOutItem Rule = "expression-item-outside-fan-out-item"
	// RuleExpressionSecretInCondition is a condition reading a secret. "secrets is
	// exposed to params and to secrets and nowhere else."
	RuleExpressionSecretInCondition Rule = "expression-secret-in-condition"
)

// The rules the corpus does not carry, because a fixture cannot show them: two of them
// need a manifest the corpus keeps for other fixtures, two are read when a run is
// already going, and one is a grammar the released schema carries in a pattern rather
// than in a fixture of its own. They are spelled the same way all the same, so that one
// vocabulary names every refusal this package makes.
const (
	// RuleManifestMissing is a brick step whose manifest was not handed in. Reading
	// /agk/brick.yaml means pulling an image and pulling an image is executing, so the
	// manifests arrive as an argument and the evaluator says which ones it needs.
	RuleManifestMissing Rule = "manifest-missing"
	// RuleParamsAgainstManifest is a step parameter the manifest refuses: one it does
	// not declare, one it declares required and the step does not supply, or one whose
	// value departs from the schema the manifest gives it.
	RuleParamsAgainstManifest Rule = "params-against-manifest"
	// RuleZipLength is merge: zip on envelopes of differing lengths, which "produce a
	// validation failure".
	RuleZipLength Rule = "zip-length"
	// RuleMCPSyncTimeoutAboveCeiling is a sync tool waiting longer than the ceiling.
	// "A sync call is capped at 120 seconds because clients time out, and a workflow
	// that cannot finish inside it has to be async."
	RuleMCPSyncTimeoutAboveCeiling Rule = "mcp-sync-timeout-above-ceiling"
	// RuleMCPTimeoutOnAsyncTool is a timeout on an async tool, "where it would mean
	// nothing: the call returns the run identifier at once". The value is the corpus's
	// own name for the fixture that pins it.
	RuleMCPTimeoutOnAsyncTool Rule = "mcp-async-tool-with-timeout"
	// RuleExpressionSecretInText is a secret in a position inside a string. "An
	// expression that fills the whole value keeps its type; an expression embedded in
	// a string is converted to text", and a secret has no text: it is an opaque
	// reference, so the expression that reads one fills the whole value or nothing.
	RuleExpressionSecretInText Rule = "expression-secret-in-text"
	// RuleExpressionDoesNotCompile is an expression that is not one: it never closes,
	// it does not parse, or it compares values whose types cannot be compared. CEL
	// "evaluates in linear time, is mutation free, and not Turing-complete", and what
	// it cannot check before a run is the one thing this engine cannot promise.
	RuleExpressionDoesNotCompile Rule = "expression-does-not-compile"
)

// The rules the repository fixtures name, which are about the files of a tree together
// rather than about one document: what an include reaches, what an included file may carry,
// and the bound a port and a workflow output are held to in every file a version is made of.
// They are spelled as the corpus names them, so that a hook writing one on git's error stream
// and a fixture pinning it read the same word.
const (
	// RuleIncludeLeavesTree is a path include naming a file outside the repository tree. "A
	// path include resolves inside the same commit", and nothing outside the tree is readable
	// at all.
	RuleIncludeLeavesTree Rule = "include-leaves-tree"
	// RuleIncludeMissing is a path include naming a file the commit does not hold.
	RuleIncludeMissing Rule = "include-missing"
	// RuleIncludeCycle is an include coming back to a file still being resolved, which would
	// never finish resolving.
	RuleIncludeCycle Rule = "include-cycle"
	// The keys that make a document an entry point, refused in an included file one rule
	// each, as the fragment group files one fixture each and mcp-in-included-file already
	// was. "An included file is a fragment, not an entry point."
	RuleAPIVersionInIncludedFile  Rule = "api-version-in-included-file"
	RuleKindInIncludedFile        Rule = "kind-in-included-file"
	RuleMetadataInIncludedFile    Rule = "metadata-in-included-file"
	RuleInputsInIncludedFile      Rule = "inputs-in-included-file"
	RuleOutputsInIncludedFile     Rule = "outputs-in-included-file"
	RuleOnInIncludedFile          Rule = "on-in-included-file"
	RuleConcurrencyInIncludedFile Rule = "concurrency-in-included-file"
	RuleTimeoutInIncludedFile     Rule = "timeout-in-included-file"
)

// The rules about the triggers a workflow declares, each one what the schema lets through and
// the Triggers chapter refuses.
const (
	// RuleCronValueOutOfRange is a schedule whose five fields are cron's shape and name a value
	// its field does not count, a minute of 60, a step of 0, a range written backwards, or no
	// day that comes, the 30th of February: an occurrence that never comes.
	RuleCronValueOutOfRange Rule = "cron-value-out-of-range"
	// RuleTimezoneUnknown is a schedule's zone written as a name the IANA time zone database
	// does not hold, so that no instant can be read from it.
	RuleTimezoneUnknown Rule = "timezone-unknown"
	// RuleWebhookOutputNotDeclared is a sync webhook answering with an output the workflow does
	// not declare: what a caller is answered with is one of the workflow's own outputs.
	RuleWebhookOutputNotDeclared Rule = "webhook-output-not-declared"
	// RuleWebhookMapInputNotDeclared is a webhook's map filling an input the workflow does not
	// declare: map builds the run's inputs, each validated against its schema before the run
	// exists, and an input nobody declared has none.
	RuleWebhookMapInputNotDeclared Rule = "webhook-map-input-not-declared"
	// RuleEventMapInputNotDeclared is an event trigger's map filling an input the workflow does
	// not declare, for the reason a webhook's is refused.
	RuleEventMapInputNotDeclared Rule = "event-map-input-not-declared"
	// RuleWebhookDuplicatePath is two webhooks answering one path with one method: "within a
	// namespace a path and a method answer one trigger".
	RuleWebhookDuplicatePath Rule = "webhook-duplicate-path"
	// RulePortPastBound is a port past agk.PortMaxBytes, wherever it is written: "a port
	// becomes the file <name>.json, which a filesystem holds to 255 characters with its
	// suffix". The value is the name the corpus files the fixture pinning it under.
	RulePortPastBound Rule = "port-name-251-characters"
	// RuleWorkflowOutputPastBound is a workflow output past the same bound, for the same
	// reason.
	RuleWorkflowOutputPastBound Rule = "workflow-output-251-characters"
)

// Refusal is a workflow refused by a rule of the language, naming the step, the port and
// the rule, in the documentation's own words, and where in the tree it was written.
//
// Step and Port are empty where the rule is not about one: a cycle is about a list of
// steps and a published tool is about the workflow's own boundary. Detail is the
// sentence, and it quotes the documentation rather than describing the code, because the
// person reading it is holding the file and not this package.
//
// At is the file, line and column of the node refused, as far as the engine can know
// them: a hook writes a refusal as file:line:column: message (rule), and a person given a rule
// and a step still has to find which of four files wrote the value. A refusal about the tree
// or about a file as a whole names the file alone, and one about no file of the tree names
// none.
type Refusal struct {
	Step   agk.Step
	Port   agk.Port
	Rule   Rule
	Detail string
	At     Position
}

func (r *Refusal) Error() string {
	var b strings.Builder
	b.WriteString("graph: ")
	if at := r.At.String(); at != "" {
		b.WriteString(at + ": ")
	}
	if r.Step != "" {
		b.WriteString("step " + string(r.Step) + ": ")
	}
	if r.Port != "" {
		b.WriteString("port " + string(r.Port) + ": ")
	}
	b.WriteString(string(r.Rule))
	if r.Detail != "" {
		b.WriteString(": " + r.Detail)
	}
	return b.String()
}

// Unwrap makes every refusal one errors.Is, so that a caller separating a refused
// workflow from a failure of its own does not have to know the rules to do it.
func (r *Refusal) Unwrap() error { return ErrRefused }

// refuse is the one place a refusal is built, so that every rule is stated the same way
// and none of them is a fmt.Errorf somebody wrote in passing.
func refuse(rule Rule, step agk.Step, port agk.Port, detail string) *Refusal {
	return &Refusal{Step: step, Port: port, Rule: rule, Detail: detail}
}
