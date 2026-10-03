package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agentiik/agentiik/mcp"
)

// The tools that author a workflow: workflow.validate, "the only authority on whether a draft is
// legal", and workflow.commit, which "makes a real git commit". Each is its route, made as the
// caller.

// filesSchema is the files a draft lays over a tree or a commit writes, as both routes read them.
const filesSchema = `{"type":"object","description":"Each path of the repository's tree mapped to its new text, UTF-8, or to null to remove the file. agentiik.yaml at the root is the entry point. Every other file of the tree is kept as it is.","minProperties":1,"maxProperties":4096,"additionalProperties":{"type":["string","null"]}}`

// whereSchema are the two arguments that name a workflow.
const whereSchema = `"namespace":{"type":"string","description":"The namespace the workflow is in, as namespace.list names it."},"workflow":{"type":"string","description":"The workflow's name in the namespace, the name of its repository."}`

// workflowValidate is workflow.validate, POST /api/v1/{ns}/workflows/{name}/validate.
func (m *MCP) workflowValidate(r *http.Request) mcp.Tool {
	return mcp.Tool{
		Name:        "workflow.validate",
		Title:       "Validate a draft",
		Description: "Judges a draft as the push would judge it, and commits nothing: the files are laid over the tree of ref, the default branch's head where it is left out, and checked by the pre-receive hook's own check, includes and inheritance resolved, the graph built, ports held to the brick manifests the repository records, secrets to the namespace's declarations. It is the only authority on whether a draft is legal. A draft it accepts is answered with what the version would hold; one it refuses with where the problem is, as a file, a line and a JSON Pointer, what was expected there, and the topic of workflow.language that explains it. Validate until it passes, then commit with workflow.commit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + whereSchema + `,"ref":{"type":"string","description":"The branch, the tag or the version the files are laid over. The default branch's head where left out."},"files":` + filesSchema + `},"required":["namespace","workflow","files"],"additionalProperties":false}`),
		Annotations: readOnly(),
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Namespace string             `json:"namespace"`
				Workflow  string             `json:"workflow"`
				Ref       string             `json:"ref,omitempty"`
				Files     map[string]*string `json:"files"`
			}
			if err := strictly(arguments, &in); err != nil {
				return nil, mcp.InvalidParams("workflow.validate takes namespace, workflow, files and ref: %v", err)
			}
			if in.Namespace == "" || in.Workflow == "" || len(in.Files) == 0 {
				return nil, mcp.InvalidParams("workflow.validate takes the namespace and the workflow the draft is of, and the files it lays over the tree")
			}
			body := map[string]any{"files": in.Files}
			if in.Ref != "" {
				body["ref"] = in.Ref
			}
			a, err := m.dispatch(r, "workflow.validate", "POST", "/api/v1/"+segment(in.Namespace)+"/workflows/"+segment(in.Workflow)+"/validate", nil, body)
			if err != nil {
				return nil, err
			}
			return result(a)
		},
	}
}

// workflowCommit is workflow.commit, POST /api/v1/{ns}/workflows/{name}/commits.
func (m *MCP) workflowCommit(r *http.Request) mcp.Tool {
	return mcp.Tool{
		Name:        "workflow.commit",
		Title:       "Commit files",
		Description: "Commits files onto a branch as you, and pushes the commit through the pre-receive hook, which refuses a workflow that does not validate: validate with workflow.validate first. parent is the commit the files were read at, which the branch must still point at; it is left out only for the first commit of an empty repository. The commit is a real git commit, marked as made through MCP. A commit onto the default branch makes the version runs start from.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + whereSchema + `,"branch":{"type":"string","description":"The branch committed to, the default branch where left out, created at parent where it does not exist."},"parent":{"type":"string","description":"The commit the files were read at, forty hexadecimal digits, as workflow.get answers it."},"message":{"type":"string","minLength":1,"description":"The commit's message, which git log shows: what changed and why."},"files":` + filesSchema + `},"required":["namespace","workflow","message","files"],"additionalProperties":false}`),
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Hint(false), DestructiveHint: mcp.Hint(false), IdempotentHint: mcp.Hint(false), OpenWorldHint: mcp.Hint(false)},
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Namespace string             `json:"namespace"`
				Workflow  string             `json:"workflow"`
				Branch    string             `json:"branch,omitempty"`
				Parent    string             `json:"parent,omitempty"`
				Message   string             `json:"message"`
				Files     map[string]*string `json:"files"`
			}
			if err := strictly(arguments, &in); err != nil {
				return nil, mcp.InvalidParams("workflow.commit takes namespace, workflow, branch, parent, message and files: %v", err)
			}
			if in.Namespace == "" || in.Workflow == "" || in.Message == "" || len(in.Files) == 0 {
				return nil, mcp.InvalidParams("workflow.commit takes the namespace and the workflow, a message and the files it writes")
			}
			body := map[string]any{"message": in.Message, "files": in.Files}
			if in.Branch != "" {
				body["branch"] = in.Branch
			}
			if in.Parent != "" {
				body["parent"] = in.Parent
			}
			a, err := m.dispatch(r, "workflow.commit", "POST", "/api/v1/"+segment(in.Namespace)+"/workflows/"+segment(in.Workflow)+"/commits", nil, body)
			if err != nil {
				return nil, err
			}
			return result(a)
		},
	}
}
