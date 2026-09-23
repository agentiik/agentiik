package graph

import (
	"encoding/json"

	"github.com/goccy/go-yaml/ast"

	"github.com/agentiik/agentiik/agk"
)

// Workflow is agentiik.yaml as a value: "the source of truth for what a workflow does".
//
// Everything the file declares is here and nothing else is. It is not the source of
// truth for who may do what, so no grant appears; it is not a run, so no state appears;
// and it holds no compiled schema, because compiling one needs the repository tree and
// the tree is the caller's to provide.
//
// A Workflow arrives from Parse or from Load and not from a literal. The unexported
// fields below are what resolution and a refusal need: the steps as they were written,
// the hidden blocks they extend, and the document itself, so that a position can name
// the line an expression was written on.
type Workflow struct {
	APIVersion  string
	Kind        string
	Metadata    Metadata
	Inputs      map[string]Input
	Outputs     map[string]Output
	On          Trigger
	MCP         *MCP
	Include     []Include
	Vars        Vars
	Secrets     []string
	Defaults    Defaults
	Concurrency Concurrency
	Timeout     Duration
	Steps       map[agk.Step]Step

	// values and blocks are the steps and the hidden blocks as the file wrote them,
	// every keyword still optional. Resolution runs over them, and Load runs it again
	// once the included fragments have joined them, which is why they outlive Parse.
	values map[agk.Step]stepValues
	blocks map[string]stepValues

	// doc is the parsed document, kept so that a refusal can point at the line and
	// column a value was written at rather than at a key somewhere in the file.
	doc *ast.File

	// resolved says whether extends had everything it needed. A file that declares
	// includes has not been given them by Parse, so an extends naming a block this
	// document does not carry is left for Load rather than refused here.
	resolved bool
}

// Metadata is what the workflow is called and which namespace owns it. "Ownership is not
// only naming: a workflow reaches the secrets, the quotas and the runner pools of the
// namespace that owns it, and of no other."
type Metadata struct {
	Name      string
	Namespace string
	Labels    map[string]string
}

// Input is one declared workflow input: the boundary a trigger fills.
//
// Schema is the JSON Schema document as it was written, uncompiled, because compiling it
// resolves a $ref against the commit's tree and the tree is not this package's to hold.
// Required and Default are the two keywords that are not JSON Schema's.
type Input struct {
	Schema   json.RawMessage
	Required bool
	Default  any
}

// Output is one declared workflow output: a view of one step port.
//
// From names the step and the port, and never more than one of each: "an output that
// concatenated several would hide which step produced what".
type Output struct {
	From struct {
		Step agk.Step
		Port agk.Port
	}
	Schema json.RawMessage
	Retain Retain
}

// Retain is how long the artifacts of an output stay fetchable, and how many times they
// may be fetched.
//
// Fetches is zero where the file wrote none, which is "for as many fetches as anyone
// makes". Only a workflow output may carry one: "an artifact travelling between two
// steps is read once per shard and again by a replay".
type Retain struct {
	For     Duration
	Fetches int
}

// Trigger is the on block: what starts a run of this workflow.
//
// The block is spelled on:, which is why the file has to be read by a YAML 1.2 parser: a
// 1.1 parser reads that bare key as the boolean true.
type Trigger struct {
	Schedule []Schedule
	Webhook  []Webhook
	Event    []Event
}

// Schedule is a five-field cron expression read in the timezone written beside it.
type Schedule struct {
	Cron     string
	Timezone string
	Jitter   Duration
	CatchUp  bool
}

// Webhook is one HTTP entry point. "Paths are namespaced as /hooks/<namespace>/<path>",
// which is the API's doing and not written here.
type Webhook struct {
	Path     string
	Method   string
	Auth     string
	Response string
	Map      map[string]any
}

// Event is a CloudEvents 1.0 subscription, with a CEL filter over the attributes and
// over data.
type Event struct {
	Type   string
	Source string
	Filter string
}

// Vars are the workflow variables, read by expressions under the vars root.
type Vars map[string]any

// Include is one entry of the include block: either a file of this repository, resolved
// inside the same commit, or another repository at a ref.
//
// "A workflow include must carry ref, a path include must not, and a ref beside a path is
// refused. Requiring it is not tidiness: an unpinned cross-repository include would let
// another repository change what this commit does."
type Include struct {
	Path     string
	Workflow WorkflowRef
}

// Concurrency is the policy a trigger follows when a run of the same workflow is still
// going. "Concurrency groups are scoped to the namespace, so one namespace can never
// block or cancel another's runs."
type Concurrency struct {
	Group            string
	CancelInProgress bool
}

// MCP is the published surface: which inputs and outputs a model-driven client may
// reach, under what names.
//
// A workflow carries a pointer to one rather than a value, because "an empty list
// publishes a server with nothing on it, which is not the same as declaring no mcp block
// at all: the first serves an empty tool list, the second serves nothing and returns
// 404".
type MCP struct {
	Name        string
	Description string
	Tools       []Tool
}

// Tool is one published tool. Name, Description and Input are the three the language
// requires: "a tool published without a description is exactly the tool a model has no
// way to decide to call".
type Tool struct {
	Name        string
	Title       string
	Description string
	Input       ToolIO
	Output      *ToolIO
	Mode        ToolMode
	Timeout     Duration
	Annotations Annotations
}

// ToolIO names the workflow input or output a tool is a view of. Exactly one of the two
// is set, decided by the key it was written under: "the reference is to a workflow input
// or output, never to a step or a port".
type ToolIO struct {
	Input  string
	Output string
}

// ToolMode is how a call waits. "sync waits for the run and returns the output; async
// returns the run identifier at once."
type ToolMode int

const (
	ToolSync ToolMode = iota
	ToolAsync
)

func (m ToolMode) String() string {
	if m == ToolAsync {
		return "async"
	}
	return "sync"
}

// Annotations are the protocol's own hints, "passed through unchanged. They are hints to
// a client, never a permission: what a tool may do is decided by the grants, not by a
// field in the file."
//
// Each one is a pointer because a hint the file does not write is not the same as a hint
// written false, and what is published is what was written.
type Annotations struct {
	ReadOnlyHint    *bool
	IdempotentHint  *bool
	DestructiveHint *bool
	OpenWorldHint   *bool
}

// Defaults is the execution settings a workflow applies to every step that does not say
// otherwise, and it is exactly the set the documentation lists.
//
// "What it may not carry: image, script, needs, inputs, outputs, params, if, merge,
// strategy, extends and workflow are refused there. Anything deciding what a step runs,
// or where it sits in the graph, belongs to the step: the cut is what keeps the graph
// readable from the steps block alone." The cut is this type: a step is these keywords
// and the ones that decide what it runs, and defaults is these alone.
//
// Every field is optional. A pointer distinguishes a value the file wrote from one it
// did not, which matters for every boolean here: cache: false and no cache at all are
// different things to a step that extends a block setting it true.
type Defaults struct {
	Timeout         *Duration
	Retain          *Retain
	Retry           *Retry
	Resources       *Resources
	Network         *Network
	EgressAllow     []string
	RunsOn          []string
	Secrets         []string
	Cache           *bool
	ContinueOnError *bool
	Idempotent      *bool
	Files           []FileSelector
	Shell           []string
	BeforeScript    []string
	AfterScript     []string
	When            []When
}

// Step is one step of the graph, with every keyword resolved: what arrived from an
// include, then from the blocks it extends, then from defaults, then what the step wrote
// itself.
//
// It carries an image or a call and never both, and a call "runs no container of its
// own", so Script, BeforeScript, AfterScript and Shell are refused beside one.
type Step struct {
	Image string
	Call  *Call

	Needs   []Edge
	Inputs  map[agk.Port]any
	Outputs []agk.Port
	Params  map[string]any

	If       string
	When     []When
	Merge    Merge
	Join     Join
	Strategy Strategy

	Retry           Retry
	Timeout         Duration
	Retain          Retain
	ContinueOnError bool
	Resources       Resources
	Network         Network
	EgressAllow     []string
	RunsOn          []string
	Secrets         []string
	Cache           bool
	Idempotent      bool
	Files           []FileSelector

	Script       []string
	BeforeScript []string
	AfterScript  []string
	Shell        []string

	// Extends is the hidden block this step was written against, kept after resolution
	// because a run detail that says where a setting came from reads better than one
	// that shows the setting alone.
	Extends string
}

// Call is a step that calls a sub-workflow instead of running a container: "workflow:
// <namespace>/<name>, with @<ref> appended to pin a tag or a commit, or in long form
// { workflow, ref }".
type Call struct {
	Workflow string
	Ref      string
}

// ref is the call as a reference, which is the shape an include and a run already speak.
func (c Call) ref() WorkflowRef {
	r, err := parseWorkflowRef(c.Workflow, "the call")
	if err != nil {
		return WorkflowRef{}
	}
	r.Ref = c.Ref
	return r
}

// Edge is one inbound dependency: an output port of one step feeding an input port of
// this one. "That is the only form of dependency: there is no implicit stage, and the
// order in which steps appear in the file has no bearing on scheduling."
//
// The short form of needs is a step name alone, which is "{ step: name, port: out, as:
// in }" written out.
type Edge struct {
	Step agk.Step
	Port agk.Port
	As   agk.Port
}

// Merge is how several edges arriving on one input port become the single envelope that
// port carries.
type Merge int

const (
	MergeWaitAll Merge = iota
	MergeZip
	MergeJoin
	MergeFirst
)

func (m Merge) String() string {
	switch m {
	case MergeZip:
		return "zip"
	case MergeJoin:
		return "join"
	case MergeFirst:
		return "first"
	default:
		return "wait_all"
	}
}

// Join is the key join: "a JSON path, for example join: { on: "$.data.customer_id" }".
type Join struct {
	On string
}

// FanOut is how a step is divided into shards.
type FanOut int

const (
	FanOutNone FanOut = iota
	FanOutItem
	FanOutBatch
	FanOutMatrix
)

func (f FanOut) String() string {
	switch f {
	case FanOutItem:
		return "item"
	case FanOutBatch:
		return "batch(n)"
	case FanOutMatrix:
		return "matrix"
	default:
		return "none"
	}
}

// Strategy is the strategy block: how the step is divided, how much of it runs at once,
// what combinations it runs over, and whether one failing shard stops its siblings.
//
// Batch is the size written inside batch(n), and it is zero unless FanOut is FanOutBatch.
type Strategy struct {
	FanOut      FanOut
	Batch       int
	MaxParallel int
	Matrix      map[string][]any
	FailFast    bool
}

// When is one upstream state that allows a step to start. "Defaults to [succeeded]."
//
// Pending and running are not among them, and neither is cancelled: when reads the
// verdict of a step that has ended, and the four names are the four the language writes.
type When int

const (
	WhenSucceeded When = iota
	WhenFailed
	WhenSkipped
	WhenAlways
)

// String names the state the way when writes it, which is also the way a step verdict
// writes it: the comparison the language asks for is between the two, so one spelling
// serves both and a refusal quotes the list the author wrote.
func (w When) String() string {
	switch w {
	case WhenFailed:
		return "failed"
	case WhenSkipped:
		return "skipped"
	case WhenAlways:
		return "always"
	default:
		return "succeeded"
	}
}

// Retry is the retry policy: how many further attempts, on which kinds of failure, and
// how long the wait between them grows.
type Retry struct {
	Max     int
	On      []agk.Failure
	Backoff Backoff
}

// Backoff is how the wait between attempts grows. "exponential is the only type there
// is: the wait starts at base, lengthens after each failure and never goes past max."
type Backoff struct {
	Type BackoffType
	Base Duration
	Max  Duration
}

// BackoffType is the shape of the wait. There is one, and it is named rather than
// assumed so that a file saying type: exponential reads as what it is.
type BackoffType int

const BackoffExponential BackoffType = iota

func (b BackoffType) String() string { return "exponential" }

// Resources is what a step asks for, "capped by the runner policy and by the namespace
// quota".
//
// CPU and Memory are text because the language writes them as text: "cpu and memory are
// written exactly as the brick manifest writes them, quotation marks included: cpu: 1
// unquoted is refused", so that half a core reads as 0.5 wherever it travels.
type Resources struct {
	CPU    string
	Memory string
	PIDs   int
}

// Network is the posture a step's container gets. "Every task gets its own network, and
// there is no posture that puts a container on the host's."
type Network int

const (
	NetworkNone Network = iota
	NetworkEgress
	NetworkInternal
)

func (n Network) String() string {
	switch n {
	case NetworkEgress:
		return "egress"
	case NetworkInternal:
		return "internal"
	default:
		return "none"
	}
}

// FileSelector narrows what a step receives from the repository tree, "which it
// otherwise gets whole under /agk/repo/".
//
// The short form is a path or a glob and leaves To and Mode empty; the long form
// "relocates a path to wherever a tool insists on finding it". Narrowing is "an
// optimisation for large repositories, never a requirement, and never a permission
// boundary".
type FileSelector struct {
	From string
	To   string
	Mode string
}

// SecretMount is one secret a task is given: the name the workflow knows it by, and the
// path under /agk/secrets/ the brick manifest asks for it at.
//
// It carries no value and never will. "The runner mounts secret values on tmpfs" and the
// evaluator decides which secrets a task gets, not what they are.
type SecretMount struct {
	Name  string
	Mount string
}

// stepValues is a step as the file wrote it, before anything was resolved: every keyword
// optional, so that a value coming from an include, from a block it extends, from
// defaults or from the step itself can be told from one that is simply absent.
//
// It embeds Defaults because that type is exactly the keywords a step shares with
// defaults; what is written out beside it is what "belongs to the step".
type stepValues struct {
	Defaults

	Image    *string
	Call     *Call
	Needs    []Edge
	Inputs   map[agk.Port]any
	Outputs  []agk.Port
	Params   map[string]any
	If       *string
	Merge    *Merge
	Join     *Join
	Strategy *Strategy
	Script   []string
	Extends  string
}
