package graph

import (
	"fmt"
	"time"
)

// The mcp block is the published surface: "a workflow is callable by a model-driven client only
// when its root mcp block says how: one block, one tool, whose arguments are the workflow's
// inputs". It is part of the language rather
// than a setting in the console because it belongs to the version, so publishing a tool,
// renaming one or withdrawing one is a commit.
//
// The rules below are "language rules, so they are checked before the push is accepted
// rather than at the first call. A workflow whose published surface does not hold
// together cannot reach the branch."

// toolTimeoutCeiling is how long a sync call may wait. "Capped at 120 seconds, because
// clients time out; a workflow that cannot finish inside it must be async. The cap is on
// the value and not on the unit, so 120s, 2m and 120000ms are the longest forms it
// accepts and an hour cannot be written at all."
const toolTimeoutCeiling = 120 * time.Second

// checkMCP holds the published surface together.
//
// The documentation's other refusals are the reader's, because they are about the tool alone:
// no description, an invalid name, an output that is not a name, an input with no schema, and
// the two rules about how long a call waits. What is left needs the workflow's boundary, which
// is what this sees: the output the tool returns has to be one the workflow declares.
func checkMCP(wf *Workflow) error {
	if wf.MCP == nil {
		return nil
	}
	// "The name of one declared workflow output, optional. Its envelope becomes the result
	// and its schema the outputSchema."
	if out := wf.MCP.Output; out != "" {
		if _, ok := wf.Outputs[out]; !ok {
			return refuse(RuleMCPOutputNotDeclared, "", "", fmt.Sprintf("the tool %s returns the output %s, which the workflow does not declare: a tool is a view of the workflow's own boundary, so its result is one of the workflow's outputs, never a step or a port, which is what keeps the graph free to change beneath it", wf.MCP.Name, out))
		}
	}
	// The two rules about how long a call waits are the reader's, because they are about the
	// tool alone. They are asked again here so that a workflow reaching Check by any other
	// route is held to them too, and asking twice costs a comparison.
	return toolTimeout(wf.MCP)
}

// toolTimeout holds a published tool to the two rules about how long a call waits.
func toolTimeout(tool *MCP) error {
	switch {
	case tool.Mode == ToolAsync && tool.Timeout != 0:
		return refuse(RuleMCPTimeoutOnAsyncTool, "", "", fmt.Sprintf("the tool %s is async and waits %s: an async call returns the run identifier at once, to be followed with the user's server's run.get, so a timeout on it would mean nothing", tool.Name, tool.Timeout))
	case tool.Mode == ToolSync && time.Duration(tool.Timeout) > toolTimeoutCeiling:
		return refuse(RuleMCPSyncTimeoutAboveCeiling, "", "", fmt.Sprintf("the tool %s is sync and waits %s: a sync call is capped at 120 seconds, because clients time out, and a workflow that cannot finish inside it must be async. The cap is on the value and not on the unit, so 120s, 2m and 120000ms are the longest forms it accepts", tool.Name, tool.Timeout))
	}
	return nil
}
