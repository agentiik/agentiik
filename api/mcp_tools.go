package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/agentiik/agentiik/mcp"
)

// The user's server's tools: "namespaces, sharing, secret declarations, workflows, runs, runners and
// the caller's collections", each a route of the API made as the caller (mcp_dispatch.go), and each
// listed only to a caller who may use it, since "a tool the caller cannot use is absent rather than
// refused, as the protocol allows". What a caller may use is what GET /api/v1/me answers them:
// the permissions they hold at every scope, and whether they administer.

// routed is one tool of the user's server, declared as the route it is.
type routed struct {
	name, title, description string

	// input is the JSON Schema of the arguments, which a call is held to before anything is asked
	// of the API: "a malformed argument" is the protocol's error, said where it is.
	input string

	annotations *mcp.Annotations

	// offered says whether a caller holding what Effective answers may use the tool anywhere.
	offered func(Effective) bool

	// request is the route the arguments make, its query and its body, nil for none.
	request func(args map[string]any) (method, path string, query url.Values, body any)

	// read, where set, reads the route's answer itself rather than answering it as it came.
	read func(a *toolAnswer) (*mcp.CallResult, error)
}

// holds says whether a caller holds a permission at any scope, a namespace or a workflow of one.
func holds(p Permission) func(Effective) bool {
	return func(e Effective) bool {
		for _, set := range e.Permissions {
			if set.Has(p) {
				return true
			}
		}
		return false
	}
}

// holdsInNamespace says whether a caller holds a permission at a namespace's scope, which a route
// guarded at that scope asks for and a grant on one workflow never gives.
func holdsInNamespace(p Permission) func(Effective) bool {
	return func(e Effective) bool {
		for at, set := range e.Permissions {
			if at.Workflow == "" && set.Has(p) {
				return true
			}
		}
		return false
	}
}

// either is a caller who may use a tool by any of several.
func either(ways ...func(Effective) bool) func(Effective) bool {
	return func(e Effective) bool {
		for _, way := range ways {
			if way(e) {
				return true
			}
		}
		return false
	}
}

func anybody(Effective) bool       { return true }
func administers(e Effective) bool { return e.Admin }

// managing is grant:manage at a namespace's scope, which owning a namespace is, or administering.
func managing(e Effective) bool {
	if e.Admin {
		return true
	}
	for at, set := range e.Permissions {
		if at.Workflow == "" && set.Has(GrantManage) {
			return true
		}
	}
	return false
}

// The annotations of the three kinds of tool: one that reads, one that writes, and one that
// removes, which "destructiveHint" marks so that a client may ask before calling it.
func reads() *mcp.Annotations { return readOnly() }
func writes() *mcp.Annotations {
	return &mcp.Annotations{ReadOnlyHint: mcp.Hint(false), DestructiveHint: mcp.Hint(false), IdempotentHint: mcp.Hint(false), OpenWorldHint: mcp.Hint(false)}
}
func removes() *mcp.Annotations {
	return &mcp.Annotations{ReadOnlyHint: mcp.Hint(false), DestructiveHint: mcp.Hint(true), IdempotentHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
}

// Pieces of the argument schemas, written once.
const (
	argNamespace = `"namespace":{"type":"string","description":"The namespace, as namespace.list names it."}`
	argWorkflow  = `"workflow":{"type":"string","description":"The workflow's name in the namespace."}`
	argRun       = `"run":{"type":"string","description":"The run's identifier, as run.list or workflow.run answers it."}`

	argCollection = `"collection":{"type":"string","description":"The collection's identifier, as collection.list answers it."}`
)

// object is an arguments schema: the properties given, the ones required, and no other. A schema
// requiring nothing names no required, since JSON Schema 2020-12 holds required to an array and a
// client compiling the tool's inputSchema refuses a null there.
func object(required []string, props ...string) string {
	req := ""
	if len(required) > 0 {
		b, _ := json.Marshal(required)
		req = `,"required":` + string(b)
	}
	return `{"type":"object","properties":{` + strings.Join(props, ",") + `}` + req + `,"additionalProperties":false}`
}

// str and opt read an argument the schema already held to its type.
func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// tools are the user's server's tools, beside the four that teach and author a workflow.
func userTools() []routed {
	return []routed{
		{
			name: "namespace.list", title: "List namespaces",
			description: "The namespaces you hold a grant in, your personal one first, and every one for an administrator: each with its kind and its owner.",
			input:       object(nil), annotations: reads(), offered: anybody,
			request: func(map[string]any) (string, string, url.Values, any) { return "GET", "/api/v1/namespaces", nil, nil },
		},
		{
			name: "namespace.get", title: "Get a namespace",
			description: "One namespace: its kind, its owner, its quotas, and the names it held before it was renamed.",
			input:       object([]string{"namespace"}, argNamespace), annotations: reads(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/namespaces/" + segment(str(a, "namespace")), nil, nil
			},
		},
		{
			name: "namespace.create", title: "Create a namespace",
			description: "Creates a shared namespace that you own. An administrator may name another owner, a user or group:NAME, and set its quotas; anybody else is refused either, and the namespace takes the installation's default quotas.",
			input: object([]string{"name"},
				`"name":{"type":"string","description":"The namespace's name: lowercase words joined by hyphens."}`,
				`"owner":{"type":"string","description":"Another owner than yourself, a login or group:NAME. An administrator's alone."}`,
				`"quotas":{"type":"object","description":"The namespace's quotas, as namespace.quotas sets them. An administrator's alone."}`),
			annotations: writes(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) { return "POST", "/api/v1/namespaces", nil, a },
		},
		{
			name: "namespace.update", title: "Rename a namespace",
			description: "Renames a shared namespace you own. The old name stays its own as a former name, so that what was configured with it keeps reaching it.",
			input:       object([]string{"namespace", "name"}, argNamespace, `"name":{"type":"string","description":"The new name."}`),
			annotations: writes(), offered: managing,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PATCH", "/api/v1/namespaces/" + segment(str(a, "namespace")), nil, map[string]any{"name": a["name"]}
			},
		},
		{
			name: "namespace.delete", title: "Remove a namespace",
			description: "Removes a shared namespace you own that holds nothing: no workflow, run, secret, stored object or service account. One that holds something is refused, saying what.",
			input:       object([]string{"namespace"}, argNamespace), annotations: removes(), offered: managing,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", "/api/v1/namespaces/" + segment(str(a, "namespace")), nil, nil
			},
		},
		{
			name: "namespace.quotas", title: "Set a namespace's quotas",
			description: "Replaces a namespace's quotas and the runner pools it may use, whole: a bound left out is one removed. An administrator's alone.",
			input:       object([]string{"namespace", "quotas"}, argNamespace, `"quotas":{"type":"object","description":"max_runs_per_hour, max_concurrent_tasks, max_artifact_bytes, max_run_duration, max_retention_days and allowed_runner_pools, as the API's quotas record names them."}`),
			annotations: writes(), offered: administers,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PUT", "/api/v1/namespaces/" + segment(str(a, "namespace")) + "/quotas", nil, a["quotas"]
			},
		},
		{
			name: "grant.list", title: "List grants",
			description: "The grants and denies written on a namespace, or those that apply to one of its workflows where workflow is given, each with its identifier, principal, role or deny and expiry.",
			input:       object([]string{"namespace"}, argNamespace, argWorkflow), annotations: reads(), offered: either(holds(GrantManage), administers),
			request: func(a map[string]any) (string, string, url.Values, any) { return "GET", grantsPath(a), nil, nil },
		},
		{
			name: "grant.create", title: "Share",
			description: "Shares a namespace, or one of its workflows where workflow is given, with a user, group:NAME or a service account NS/NAME: a role (viewer, operator, editor, owner), or a deny of one permission, with an optional expiry.",
			input: object([]string{"namespace", "principal"}, argNamespace, argWorkflow,
				`"principal":{"type":"string","description":"Who is granted: a login, group:NAME, or NS/NAME for a service account."}`,
				`"role":{"type":"string","enum":["viewer","operator","editor","owner"],"description":"The role granted. Give a role or a deny, not both."}`,
				`"deny":{"type":"string","description":"One permission denied, such as run:read_data. Give a role or a deny, not both."}`,
				`"expires_at":{"type":"string","format":"date-time","description":"When the grant ends, an RFC 3339 instant."}`),
			annotations: writes(), offered: either(holds(GrantManage), administers),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", grantsPath(a), nil, pick(a, "principal", "role", "deny", "expires_at")
			},
		},
		{
			name: "grant.revoke", title: "Revoke a grant",
			description: "Revokes one grant or deny, by the identifier grant.list answers, written on the namespace or, where workflow is given, on that workflow. It applies from the next request.",
			input:       object([]string{"namespace", "id"}, argNamespace, argWorkflow, `"id":{"type":"string","description":"The grant's identifier."}`),
			annotations: removes(), offered: either(holds(GrantManage), administers),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", grantsPath(a) + "/" + segment(str(a, "id")), nil, nil
			},
		},
		{
			name: "secret.list", title: "List secret declarations",
			description: "A namespace's secret declarations: each secret's name, provider, path and the file a step is given it at. Never a value.",
			input:       object([]string{"namespace"}, argNamespace), annotations: reads(), offered: holdsInNamespace(WorkflowRead),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/" + segment(str(a, "namespace")) + "/secrets", nil, nil
			},
		},
		{
			name: "secret.declare", title: "Declare a secret",
			description: "Declares a secret by the provider that keeps its value and the path it is read at there, or changes a declaration. It never takes a value: a builtin secret's value is written with the console, agk or the API, where the person writing it is the one holding it.",
			input: object([]string{"namespace", "name", "provider"}, argNamespace,
				`"name":{"type":"string","description":"The secret's name, as a workflow's secrets block names it."}`,
				`"provider":{"type":"string","description":"The store the value is kept in, such as env, vault or aws-secrets-manager."}`,
				`"path":{"type":"string","description":"Where the provider keeps the value."}`),
			annotations: writes(), offered: holdsInNamespace(SecretWrite),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PUT", "/api/v1/" + segment(str(a, "namespace")) + "/secrets/" + segment(str(a, "name")), nil, pick(a, "provider", "path")
			},
		},
		{
			name: "secret.remove", title: "Remove a secret declaration",
			description: "Removes one secret's declaration, and a builtin secret's value with it. A workflow naming it is refused at its next push, and a run of it at its first redemption.",
			input:       object([]string{"namespace", "name"}, argNamespace, `"name":{"type":"string","description":"The secret's name."}`),
			annotations: removes(), offered: holdsInNamespace(SecretWrite),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", "/api/v1/" + segment(str(a, "namespace")) + "/secrets/" + segment(str(a, "name")), nil, nil
			},
		},
		{
			name: "workflow.list", title: "List workflows",
			description: "The workflows of a namespace whose runs you read, each with when it was created and its latest run.",
			input:       object([]string{"namespace"}, argNamespace), annotations: reads(), offered: either(holds(WorkflowRead), holds(RunRead)),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/" + segment(str(a, "namespace")) + "/workflows", nil, nil
			},
		},
		{
			name: "workflow.get", title: "Get a workflow",
			description: "A workflow's repository and the version its default branch's head is: the commit, its clone URL, and the graph it resolves to, includes resolved and images by digest. The head's commit is the parent a workflow.commit names.",
			input:       object([]string{"namespace", "workflow"}, argNamespace, argWorkflow), annotations: reads(), offered: holds(WorkflowRead),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/" + segment(str(a, "namespace")) + "/workflows/" + segment(str(a, "workflow")), nil, nil
			},
		},
		{
			name: "workflow.create", title: "Create a workflow",
			description: "Creates an empty workflow repository in a namespace, its default branch unborn: commit its first agentiik.yaml with workflow.commit, naming no parent.",
			input: object([]string{"namespace", "name"}, argNamespace,
				`"name":{"type":"string","description":"The workflow's name, which its repository and metadata.name carry."}`,
				`"default_branch":{"type":"string","description":"Its default branch, main where left out."}`,
				`"protected":{"type":"boolean","description":"Whether moving the default branch takes grant:manage."}`),
			annotations: writes(), offered: holdsInNamespace(WorkflowWrite),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", "/api/v1/" + segment(str(a, "namespace")) + "/workflows", nil, pick(a, "name", "default_branch", "protected")
			},
		},
		{
			name: "workflow.update", title: "Change a workflow",
			description: "Renames a workflow, moves it to another namespace, or names and protects its default branch: changes holds the fields to change, as the API's PATCH reads them.",
			input: object([]string{"namespace", "workflow", "changes"}, argNamespace, argWorkflow,
				`"changes":{"type":"object","description":"name to rename it; namespace to move it to another you own; default_branch and protected, under grant:manage.","properties":{"name":{"type":"string"},"namespace":{"type":"string"},"default_branch":{"type":"string"},"protected":{"type":"boolean"}},"additionalProperties":false,"minProperties":1}`),
			annotations: writes(), offered: holds(WorkflowWrite),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PATCH", "/api/v1/" + segment(str(a, "namespace")) + "/workflows/" + segment(str(a, "workflow")), nil, a["changes"]
			},
		},
		{
			name: "workflow.delete", title: "Delete a workflow",
			description: "Deletes a workflow with its runs: its runs still going are cancelled and the data of every run expires at once.",
			input:       object([]string{"namespace", "workflow"}, argNamespace, argWorkflow), annotations: removes(), offered: holds(WorkflowDelete),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", "/api/v1/" + segment(str(a, "namespace")) + "/workflows/" + segment(str(a, "workflow")), nil, nil
			},
		},
		{
			name: "workflow.run", title: "Run a workflow",
			description: "Starts a run of a workflow, at the head of its default branch, of the branch or tag ref names, or of a commit that is a version, with the inputs given, each held to its schema before the run exists. Follow it with run.get.",
			input: object([]string{"namespace", "workflow"}, argNamespace, argWorkflow,
				`"ref":{"type":"string","description":"A branch or a tag, the default branch where left out."}`,
				`"commit":{"type":"string","description":"A commit that is a version, in place of ref."}`,
				`"inputs":{"type":"object","description":"The run's inputs, by the names the workflow declares."}`),
			annotations: writes(), offered: holds(WorkflowRun),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", "/api/v1/" + segment(str(a, "namespace")) + "/workflows/" + segment(str(a, "workflow")) + "/runs", nil, pick(a, "ref", "commit", "inputs")
			},
		},
		{
			name: "run.list", title: "List runs",
			description: "The runs you read, newest first, narrowed to a namespace, a workflow or a state where given, each with what started it and how it ended.",
			input: object(nil, argNamespace, argWorkflow,
				`"state":{"type":"string","description":"queued, running, waiting, succeeded, failed, cancelled or timed_out."}`,
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"The most runs answered."}`),
			annotations: reads(), offered: holds(RunRead),
			request: func(a map[string]any) (string, string, url.Values, any) {
				q := url.Values{}
				for _, key := range []string{"namespace", "workflow", "state"} {
					if v := str(a, key); v != "" {
						q.Set(key, v)
					}
				}
				if n, ok := a["limit"].(json.Number); ok {
					q.Set("limit", n.String())
				}
				return "GET", "/api/v1/runs", q, nil
			},
		},
		{
			name: "run.get", title: "Get a run",
			description: "A run's state and each step's verdict, what started it and the principal it is attributed to; its inputs only to whoever holds run:read_data.",
			input:       object([]string{"run"}, argRun), annotations: reads(), offered: holds(RunRead),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/runs/" + segment(str(a, "run")), nil, nil
			},
		},
		{
			name: "run.logs", title: "Read a step's log",
			description: "The tail of a step's log, secrets masked, as its runner shipped it: up to the last lines asked for, 200 where left out. A step still running answers what it has written so far.",
			input: object([]string{"run", "step"}, argRun,
				`"step":{"type":"string","description":"The step's name."}`,
				`"lines":{"type":"integer","minimum":1,"maximum":2000,"description":"How many of the last lines, 200 where left out."}`),
			annotations: reads(), offered: holds(RunRead),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/runs/" + segment(str(a, "run")) + "/steps/" + segment(str(a, "step")) + "/logs", nil, nil
			},
		},
		{
			name: "run.output", title: "Read a run's output",
			description: "One output of a finished run, its envelope as the workflow declares it. Requires run:read_data, which reading a run's data takes.",
			input:       object([]string{"run", "name"}, argRun, `"name":{"type":"string","description":"The output's name, as the workflow declares it."}`),
			annotations: reads(), offered: holds(RunReadData),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/runs/" + segment(str(a, "run")) + "/outputs/" + segment(str(a, "name")), nil, nil
			},
		},
		{
			name: "run.cancel", title: "Cancel a run",
			description: "Asks for a run in flight to be cancelled, which the controller carries out: its steps under way end cancelled and those not reached pending.",
			input:       object([]string{"run"}, argRun), annotations: writes(), offered: holds(WorkflowRun),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", "/api/v1/runs/" + segment(str(a, "run")) + "/cancel", nil, map[string]any{}
			},
		},
		{
			name: "run.replay", title: "Replay a run",
			description: "Starts a new run of the commit a run pinned, over the inputs it was started with, from the step named, reusing everything upstream of it.",
			input:       object([]string{"run", "step"}, argRun, `"step":{"type":"string","description":"The step the replay starts from."}`),
			annotations: writes(), offered: holds(WorkflowRun),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", "/api/v1/runs/" + segment(str(a, "run")) + "/replay", nil, map[string]any{"step": a["step"]}
			},
		},
		{
			name: "collection.list", title: "List your collections",
			description: "Your collections, the connectors you assemble from workflows you may run, each one a tool: each with the URL a client is given, its members, and the tool each offers or why it offers none, no_mcp_block or not_runnable.",
			input:       object(nil), annotations: reads(), offered: anybody,
			request: func(map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/me/collections", nil, nil
			},
		},
		{
			name: "collection.get", title: "Get a collection",
			description: "One of your collections: its URL, its members and what each offers.",
			input:       object([]string{"collection"}, argCollection), annotations: reads(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "GET", "/api/v1/me/collections/" + segment(str(a, "collection")), nil, nil
			},
		},
		{
			name: "collection.create", title: "Create a collection",
			description: "Makes a collection of yours, holding no workflow yet, and answers it with the URL to give a client. Add workflows to it with collection.add. A principal holds 50 collections at most.",
			input: object([]string{"name"},
				`"name":{"type":"string","description":"The collection's name, unique among yours: letters, digits, hyphens and underscores."}`,
				`"description":{"type":"string","maxLength":280,"description":"What the collection is for, one line."}`),
			annotations: writes(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "POST", "/api/v1/me/collections", nil, pick(a, "name", "description")
			},
		},
		{
			name: "collection.update", title: "Rename or describe a collection",
			description: "Changes one of your collections' name, description or both. Its URL never changes, so every client configured with it keeps reaching it.",
			input: object([]string{"collection"}, argCollection,
				`"name":{"type":"string","description":"The new name."}`,
				`"description":{"type":"string","maxLength":280,"description":"The new description, the empty string clearing it."}`),
			annotations: writes(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PATCH", "/api/v1/me/collections/" + segment(str(a, "collection")), nil, pick(a, "name", "description")
			},
		},
		{
			name: "collection.delete", title: "Delete a collection",
			description: "Removes one of your collections: its URL answers 404 from the next request, and every client configured with it lists nothing. No workflow and no run changes.",
			input:       object([]string{"collection"}, argCollection), annotations: removes(), offered: anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", "/api/v1/me/collections/" + segment(str(a, "collection")), nil, nil
			},
		},
		{
			name: "collection.add", title: "Add a workflow to a collection",
			description: "Adds a workflow you may run to one of your collections, or changes how a member is read, written whole: ref, the branch or the tag it is read at, its default branch where left out, and as, the name its tool goes by, the one its mcp block gives where left out. A workflow whose entry point declares no mcp block is added and offers nothing until one does. A tool name another member gives is refused; as resolves it.",
			input: object([]string{"collection", "namespace", "workflow"}, argCollection, argNamespace, argWorkflow,
				`"ref":{"type":"string","description":"The branch or the tag the member is read at, never a commit."}`,
				`"as":{"type":"string","description":"The name its tool goes by in this collection."}`),
			annotations: writes(), offered: holds(WorkflowRun),
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "PUT", "/api/v1/me/collections/" + segment(str(a, "collection")) + "/members/" + segment(str(a, "namespace")) + "/" + segment(str(a, "workflow")), nil, pick(a, "ref", "as")
			},
		},
		{
			name: "collection.remove", title: "Take a workflow out of a collection",
			description: "Takes a workflow out of one of your collections: its tool leaves the next tools/list, and the workflow and its runs are untouched.",
			input:       object([]string{"collection", "namespace", "workflow"}, argCollection, argNamespace, argWorkflow),
			annotations: &mcp.Annotations{ReadOnlyHint: mcp.Hint(false), DestructiveHint: mcp.Hint(false), IdempotentHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)},
			offered:     anybody,
			request: func(a map[string]any) (string, string, url.Values, any) {
				return "DELETE", "/api/v1/me/collections/" + segment(str(a, "collection")) + "/members/" + segment(str(a, "namespace")) + "/" + segment(str(a, "workflow")), nil, nil
			},
		},
		{
			name: "runner.list", title: "List runners",
			description: "The runner inventory: each runner's pool, labels, capacity, last heartbeat and state. An administrator's alone.",
			input:       object(nil), annotations: reads(), offered: administers,
			request: func(map[string]any) (string, string, url.Values, any) { return "GET", "/api/v1/runners", nil, nil },
		},
	}
}

// grantsPath is the grants of a namespace, or of one of its workflows where the arguments name one.
func grantsPath(a map[string]any) string {
	p := "/api/v1/" + segment(str(a, "namespace"))
	if w := str(a, "workflow"); w != "" {
		p += "/workflows/" + segment(w)
	}
	return p + "/grants"
}

// pick is the arguments named, those given, as a route's body.
func pick(a map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := a[k]; ok {
			out[k] = v
		}
	}
	return out
}

// tool is a routed tool as the MCP server offers it, for the request r.
func (m *MCP) tool(r *http.Request, t routed) mcp.Tool {
	return mcp.Tool{
		Name: t.name, Title: t.title, Description: t.description,
		InputSchema: json.RawMessage(t.input), Annotations: t.annotations,
		Call: func(ctx context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			args, err := argumentsOf(t, arguments)
			if err != nil {
				return nil, err
			}
			method, path, query, body := t.request(args)
			if t.name == "run.logs" {
				return m.logTail(r, path, args)
			}
			a, err := m.dispatch(r, t.name, method, path, query, body)
			if err != nil {
				return nil, err
			}
			if t.read != nil {
				return t.read(a)
			}
			return result(a)
		},
	}
}

// schemasOf are the tools' argument schemas, compiled once each.
var schemasOf sync.Map

// argumentsOf holds a call's arguments to the tool's schema, and answers them read, numbers kept as
// written. An argument the schema refuses is the protocol's error, saying where and what was
// expected, before anything is asked of the API.
func argumentsOf(t routed, arguments json.RawMessage) (map[string]any, error) {
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	compiled, ok := schemasOf.Load(t.name)
	if !ok {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(t.input))
		if err != nil {
			return nil, err
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("tool:"+t.name, doc); err != nil {
			return nil, err
		}
		s, err := c.Compile("tool:" + t.name)
		if err != nil {
			return nil, err
		}
		compiled, _ = schemasOf.LoadOrStore(t.name, s)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return nil, mcp.InvalidParams("%s takes its arguments as one JSON object: %v", t.name, err)
	}
	if err := compiled.(*jsonschema.Schema).Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return nil, mcp.InvalidParams("%s refuses its arguments: %s", t.name, strings.ReplaceAll(ve.Error(), "\n", " "))
		}
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(arguments))
	d.UseNumber()
	var args map[string]any
	if err := d.Decode(&args); err != nil {
		return nil, mcp.InvalidParams("%s takes its arguments as one JSON object", t.name)
	}
	return args, nil
}

// logTail reads a step's log stream for what it holds now, and answers its last lines: the stream
// goes on while the step runs, so it is read for what it delivers at once and then let go.
func (m *MCP) logTail(r *http.Request, path string, args map[string]any) (*mcp.CallResult, error) {
	most := 200
	if n, ok := args["lines"].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			most = int(v)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	a, err := m.dispatch(r.WithContext(ctx), "run.logs", "GET", path, nil, nil)
	if err != nil {
		return nil, err
	}
	if a.status != http.StatusOK {
		return result(a)
	}
	var lines []string
	var event string
	scan := bufio.NewScanner(bytes.NewReader(a.body.Bytes()))
	scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scan.Scan() {
		line := scan.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "line":
			var l struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l) == nil {
				lines = append(lines, l.Text)
			}
		case strings.HasPrefix(line, "data: ") && event == "gap":
			lines = append(lines, "(lines written and not kept)")
		}
	}
	if len(lines) > most {
		lines = lines[len(lines)-most:]
	}
	text := strings.Join(lines, "\n")
	if text == "" {
		text = "The step has written nothing yet."
	}
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(text)}, StructuredContent: map[string]any{"lines": lines}}, nil
}
