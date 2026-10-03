package version

import (
	"errors"
	"strings"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/language"
)

// Problem is a refusal of Check as every reader of one is told it: where it is, the rule, what is
// wrong, what was expected there and the topic of the language that explains it.
//
// "Give every validation error a JSON Pointer to the offending node, what was expected there, and
// the workflow.language topic covering it", and "return the same errors from agk validate, the
// pre-receive hook and the API": a person holding the file is sent to its line, a client holding the
// document as data to its node, and a model correcting a draft to the page that teaches what it got
// wrong, from the one refusal, so that no reader is told something another is not.
type Problem struct {
	// File, Line and Column are where the node refused was written, each counted from 1; Line and
	// Column are zero where the refusal is about the file as a whole, and File is empty where it is
	// about no file of the tree.
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`

	// Pointer is the node as a JSON Pointer into File, empty for the whole document.
	Pointer string `json:"pointer"`

	// Rule is the rule, spelled as the fixture corpus spells it.
	Rule string `json:"rule"`

	// Detail is what is wrong, in the documentation's words.
	Detail string `json:"detail"`

	// Expected is what was expected there.
	Expected string `json:"expected"`

	// Topic is the topic of workflow.language that covers the node.
	Topic string `json:"topic"`
}

// Explain is the problem a refusal of Check is, and false for an error that is no refusal, a store
// that could not be reached among them, which is the caller's to answer as the failure it is.
func Explain(err error) (Problem, bool) {
	var r *graph.Refusal
	if !errors.As(err, &r) {
		return Problem{}, false
	}
	p := Problem{File: r.At.File, Line: r.At.Line, Column: r.At.Column, Pointer: r.At.Pointer, Rule: string(r.Rule), Detail: detailOf(r)}
	p.Expected = expected[r.Rule]
	if p.Expected == "" {
		p.Expected = "what the rule " + string(r.Rule) + " holds the file to"
	}
	// The node says which topic covers it where the refusal names one, since the topic of a place
	// in the file is the language's to say; a refusal about a file as a whole, or about something no
	// one node holds, is placed by its rule.
	switch {
	case p.Pointer != "":
		p.Topic = language.TopicOf(p.Pointer)
	case topics[r.Rule] != "":
		p.Topic = topics[r.Rule]
	default:
		p.Topic = language.TopicOf("")
	}
	return p, true
}

// detailOf is the refusal's sentence with the step and the port it names, which Detail alone does
// not repeat.
func detailOf(r *graph.Refusal) string {
	var b strings.Builder
	if r.Step != "" {
		b.WriteString("step " + string(r.Step) + ": ")
	}
	if r.Port != "" {
		b.WriteString("port " + string(r.Port) + ": ")
	}
	b.WriteString(r.Detail)
	return strings.TrimSuffix(b.String(), ": ")
}

// Lines are the problem as a person reads it on git's error stream and from agk validate: where and
// the rule, then the detail, then what was expected at the node and the topic to read.
func (p Problem) Lines() []string {
	at := graph.Position{File: p.File, Line: p.Line, Column: p.Column}.String()
	head := p.Rule
	if at != "" {
		head = at + ": " + p.Rule
	}
	lines := []string{head}
	if p.Detail != "" {
		lines = append(lines, p.Detail)
	}
	node := "the document"
	if p.Pointer != "" {
		node = p.Pointer
	}
	lines = append(lines, "at "+node+", expected "+p.Expected, "read workflow.language, topic "+p.Topic)
	return lines
}

// expected is what each rule expects at the node it refuses, in the words a person correcting the
// file acts on.
var expected = map[graph.Rule]string{
	graph.RuleCycleInGraph:                    "a graph with no cycle, where no step needs, through other steps, what it publishes; a loop is a call of a sub-workflow",
	graph.RuleNeedsUnknownStep:                "the name of a step this workflow declares",
	graph.RuleEdgePortNotDeclared:             "an output port the step it names declares",
	graph.RuleStepOutputNotInManifest:         "only ports the brick manifest of the step's image declares",
	graph.RuleStepInputPortNotInManifest:      "an input port the brick manifest of the step's image declares",
	graph.RuleOutputFromUnknownStep:           "a step this workflow declares, and one of its output ports",
	graph.RuleSecretNotDeclared:               "a secret the workflow's secrets block lists",
	graph.RuleScriptWithoutOutputs:            "an outputs list naming the ports the script writes, since no manifest says them",
	graph.RuleMCPToolInputNotDeclared:         "an input or an output this workflow declares",
	graph.RuleMCPToolInputWithoutSchema:       "a schema on the input the tool publishes",
	graph.RuleMCPDuplicateToolName:            "a tool name no other tool of this workflow gives",
	graph.RuleMCPInIncludedFile:               "no mcp block in an included file: the workflow that publishes a tool declares it",
	graph.RuleExpressionUnknownRoot:           "one of the roots the expression table lists for this place",
	graph.RuleExpressionItemOutsideFanOutItem: "item read only in a step sharded with fan_out: item",
	graph.RuleExpressionSecretInCondition:     "no secret in a condition: secrets is read in params and in secrets alone",
	graph.RuleManifestMissing:                 "the brick manifest of the step's image, recorded for the repository as agk push records it",
	graph.RuleParamsAgainstManifest:           "the parameters the brick manifest declares, each as its schema says, the required ones given",
	graph.RuleZipLength:                       "envelopes of one length on every edge a zip merges",
	graph.RuleMCPSyncTimeoutAboveCeiling:      "a timeout of 120 seconds at most, or mode: async",
	graph.RuleMCPTimeoutOnAsyncTool:           "no timeout on an async tool",
	graph.RuleExpressionSecretInText:          "a secret filling the whole value, never part of a string",
	graph.RuleExpressionDoesNotCompile:        "a CEL expression that compiles: closed, parsed, and comparing values that can be compared",
	graph.RuleIncludeLeavesTree:               "a path inside the repository's tree",
	graph.RuleIncludeMissing:                  "a file the commit holds at that path",
	graph.RuleIncludeCycle:                    "includes that never come back to a file still being read",
	graph.RuleAPIVersionInIncludedFile:        "no apiVersion in an included file, which is a fragment",
	graph.RuleKindInIncludedFile:              "no kind in an included file, which is a fragment",
	graph.RuleMetadataInIncludedFile:          "no metadata in an included file, which is a fragment",
	graph.RuleInputsInIncludedFile:            "no inputs in an included file: the workflow's boundary is declared by the workflow",
	graph.RuleOutputsInIncludedFile:           "no outputs in an included file: the workflow's boundary is declared by the workflow",
	graph.RuleOnInIncludedFile:                "no on block in an included file: the workflow's triggers are declared by the workflow",
	graph.RuleConcurrencyInIncludedFile:       "no concurrency in an included file, which is the workflow's to declare",
	graph.RuleTimeoutInIncludedFile:           "no timeout in an included file, which is the workflow's to declare",
	graph.RuleCronValueOutOfRange:             "five cron fields naming an occurrence that comes",
	graph.RuleTimezoneUnknown:                 "a zone the IANA time zone database names, such as Europe/Paris",
	graph.RuleWebhookOutputNotDeclared:        "an output this workflow declares",
	graph.RuleWebhookMapInputNotDeclared:      "an input this workflow declares",
	graph.RuleEventMapInputNotDeclared:        "an input this workflow declares",
	graph.RuleWebhookDuplicatePath:            "one webhook for each path and method",
	graph.RulePortPastBound:                   "a port name of 250 characters at most",
	graph.RuleWorkflowOutputPastBound:         "a workflow output name of 250 characters at most",
	RuleEntryPointMissing:                     "agentiik.yaml at the root of the repository",
	RuleEntryPointBelowRoot:                   "agentiik.yaml at the root of the repository, not below it",
	RuleSymlinkInTree:                         "regular files and directories, and no symbolic link",
	RuleSubmoduleInTree:                       "regular files and directories, and no submodule",
	RuleNameNotUTF8:                           "names written in UTF-8",
	RuleDotGitInTree:                          "no .git anywhere in the tree",
	RuleBackslashInTree:                       "names with no backslash",
	RuleTreePathTooLong:                       "a path within the bound a tree's path is held to",
	RuleTreeNameTooLong:                       "a name within the bound a tree's name is held to",
	RuleMetadataNameNotRepository:             "metadata.name equal to the repository's name",
	RuleMetadataNamespaceNotRepository:        "metadata.namespace equal to the namespace the repository is in, or left out",
	RuleImageNotPinned:                        "an image by its digest, or by a tag the repository records a digest for, as agk push records it",
	RuleSecretNotDeclaredByNamespace:          "a secret the namespace declares",
}

// topics is the topic of a rule whose refusal names no node: one about the tree, a file as a whole
// or something written across several places.
var topics = map[graph.Rule]string{
	graph.RuleCycleInGraph:             "needs",
	graph.RuleZipLength:                "merge",
	graph.RuleManifestMissing:          "steps",
	graph.RuleParamsAgainstManifest:    "steps",
	graph.RuleScriptWithoutOutputs:     "script",
	graph.RuleSecretNotDeclared:        "secrets",
	graph.RuleIncludeLeavesTree:        "includes",
	graph.RuleIncludeMissing:           "includes",
	graph.RuleIncludeCycle:             "includes",
	graph.RuleMCPInIncludedFile:        "mcp",
	graph.RuleMCPToolInputNotDeclared:  "mcp",
	graph.RuleMCPDuplicateToolName:     "mcp",
	graph.RuleWebhookDuplicatePath:     "triggers",
	RuleImageNotPinned:                 "steps",
	RuleSecretNotDeclaredByNamespace:   "secrets",
	graph.RuleAPIVersionInIncludedFile: "includes",
	graph.RuleKindInIncludedFile:       "includes",
	graph.RuleMetadataInIncludedFile:   "includes",
}
