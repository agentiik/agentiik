package graph

import (
	"fmt"
	"time"
)

// The mcp block is the published surface: "a workflow becomes callable by a model-driven
// client when, and only when, the entry point says so". It is part of the language rather
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
// Three of the documentation's six bullets are refused earlier, by the reader, because
// they are about one tool and not about the workflow: a tool with no name, no description
// or no input, and an input.from or output.from naming a step rather than a workflow
// input. What is left needs the workflow's own boundary, which is what this sees.
func checkMCP(wf *Workflow) error {
	if wf.MCP == nil {
		return nil
	}

	seen := make(map[string]bool, len(wf.MCP.Tools))
	for _, tool := range wf.MCP.Tools {
		if seen[tool.Name] {
			return refuse(RuleMCPDuplicateToolName, "", "", fmt.Sprintf("two published tools carry the name %s: a tool identifier is unique within the workflow, and stable across commits, because clients hold it. Renaming one is removing a tool and adding another", tool.Name))
		}
		seen[tool.Name] = true

		// "Names one declared workflow input, whose JSON Schema becomes the tool's
		// inputSchema as it stands."
		input, ok := wf.Inputs[tool.Input.Input]
		if !ok {
			return refuse(RuleMCPToolInputNotDeclared, "", "", fmt.Sprintf("the tool %s takes the input %s, which the workflow does not declare: the reference is to a workflow input or output, never to a step or a port, which is what keeps the graph free to change beneath a published tool", tool.Name, tool.Input.Input))
		}
		if len(input.Schema) == 0 {
			return refuse(RuleMCPToolInputWithoutSchema, "", "", fmt.Sprintf("the tool %s takes the input %s, which carries no schema: a tool has to publish an inputSchema, so the input it maps has to have one", tool.Name, tool.Input.Input))
		}

		// An output is optional, "and an output carrying no schema publishes none: a
		// tool may have an effect and return only that it succeeded". What it may not
		// be is a name the workflow does not declare, which is the same bullet the
		// input is refused by and carries the same rule.
		if tool.Output != nil {
			if _, ok := wf.Outputs[tool.Output.Output]; !ok {
				return refuse(RuleMCPToolInputNotDeclared, "", "", fmt.Sprintf("the tool %s returns the output %s, which the workflow does not declare: the reference is to a workflow input or output, never to a step or a port", tool.Name, tool.Output.Output))
			}
		}

		// The two rules about how long a call waits are the reader's, because they are
		// about the tool alone. They are asked again here so that a workflow reaching
		// Check by any other route is held to them too, and asking twice costs a
		// comparison.
		if err := toolTimeout(tool); err != nil {
			return err
		}
	}
	return nil
}

// toolTimeout holds a published tool to the two rules about how long a call waits.
func toolTimeout(tool Tool) error {
	switch {
	case tool.Mode == ToolAsync && tool.Timeout != 0:
		return refuse(RuleMCPTimeoutOnAsyncTool, "", "", fmt.Sprintf("the tool %s is async and waits %s: an async call returns the run identifier at once, to be followed with the platform server's run.get, so a timeout on it would mean nothing", tool.Name, tool.Timeout))
	case tool.Mode == ToolSync && time.Duration(tool.Timeout) > toolTimeoutCeiling:
		return refuse(RuleMCPSyncTimeoutAboveCeiling, "", "", fmt.Sprintf("the tool %s is sync and waits %s: a sync call is capped at 120 seconds, because clients time out, and a workflow that cannot finish inside it must be async. The cap is on the value and not on the unit, so 120s, 2m and 120000ms are the longest forms it accepts", tool.Name, tool.Timeout))
	}
	return nil
}
