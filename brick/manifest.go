package brick

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/agentiik/agentiik/agk"
)

// ManifestPath is where a brick carries its manifest inside its image. An image becomes
// a brick by carrying this file, and the file is the brick's own half of the contract:
// what it reads, what it writes, what it needs and what account it runs as.
const ManifestPath = "/agk/brick.yaml"

// Manifest is /agk/brick.yaml as a value.
//
// It is read here rather than in the evaluator because it is the brick's half of the
// contract and this package already is the two edges of the container. A second reader
// in the evaluator would be a second answer to what a manifest is, and agk brick test
// gets this one with no evaluator in reach.
//
// What this type applies is the manifest's own rules: the five required keys, the closed
// blocks wherever the document is the engine's and the open ones wherever the author is
// writing JSON Schema, the name grammar, the version grammar, a secret mounted under
// /agk/secrets/ and a CPU request written as a string. The rules that hold a manifest
// and a workflow together, that a step's outputs are a subset of the ports declared here
// and that its params satisfy the schemas declared here, stay in the evaluator, which is
// where a refusal can name the step.
type Manifest struct {
	APIVersion string
	Kind       string
	Metadata   ManifestMetadata
	Spec       ManifestSpec

	// document is the manifest as JSON, which is what a same-document pointer resolves
	// against. It is kept rather than rebuilt because a pointer has to reach the
	// document the author wrote, definitions included, and marshalling this struct back
	// would produce a document of this package's making.
	document []byte
}

// ManifestMetadata is what the brick is called and which release this is.
type ManifestMetadata struct {
	Name    string
	Version string
	Summary string
}

// ManifestSpec is what the brick reads, what it writes, and what it needs from its
// container.
type ManifestSpec struct {
	Inputs      map[agk.Port]ManifestPort
	Outputs     map[agk.Port]ManifestPort
	Params      map[string]Param
	Secrets     []Secret
	Runtime     Runtime
	Resources   ManifestResources
	Definitions map[string]json.RawMessage
}

// ManifestPort is one declared port. It carries a schema and nothing else, and the
// schema itself is optional: "a port declared with no schema accepts whatever arrives,
// which is what a passthrough brick says".
//
// Schema is the JSON Schema document as it was written, not a compiled one. Compiling it
// is the evaluator's business and needs a JSON Schema implementation, which this package
// does not carry: keeping the bytes is what lets the manifest be read anywhere.
type ManifestPort struct {
	Schema json.RawMessage
}

// Param is one declared parameter: a JSON Schema subschema, and the one keyword that is
// not JSON Schema's.
//
// Required is required: true written on the parameter itself, saying that a step must
// supply it. The array form of the key keeps its JSON Schema meaning and stays in
// Schema, where it names the members the value must carry; only the boolean is the
// language's own, and it is lifted out of Schema so that what is left is a document a
// JSON Schema implementation will compile.
type Param struct {
	Schema    json.RawMessage
	Required  bool
	Sensitive bool
}

// Secret is one secret the brick needs, by the name the workflow will answer with.
type Secret struct {
	Name     string
	Mount    string
	Optional bool
}

// Runtime is what the runner builds the host configuration from.
type Runtime struct {
	Network   string
	User      string
	Streaming bool
}

// ManifestResources is what the brick asks for, before the runner policy and the
// namespace quota cap it.
type ManifestResources struct {
	CPU    string
	Memory string
	PIDs   int
}

// Document is the manifest as a JSON document: the bytes a #/spec/definitions/<name>
// pointer resolves against.
//
// "A pointer inside a manifest is a JSON Pointer resolved against the manifest document
// itself", so a port schema written as { $ref: "#/spec/definitions/order" } can only be
// compiled with the whole manifest in hand. One difference from the file as it was
// written is recorded here rather than left to be discovered: a parameter's boolean
// required is removed, because it is the language's keyword and not JSON Schema's, and a
// document keeping it does not compile.
func (m Manifest) Document() []byte { return m.document }

// Port answers with the port the manifest declares under that name, an input before an
// output. The two blocks are separate in the contract, one mounted under /agk/in/ and
// one collected from /agk/out/ports/, so a name written in both is one brick reading and
// writing the same thing and either answer is the same schema.
func (m Manifest) Port(name agk.Port) (ManifestPort, bool) {
	if p, ok := m.Spec.Inputs[name]; ok {
		return p, true
	}
	p, ok := m.Spec.Outputs[name]
	return p, ok
}

// InputPorts are the ports the brick reads, in name order.
func (m Manifest) InputPorts() []agk.Port { return portNamesOf(m.Spec.Inputs) }

// OutputPorts are the ports the brick writes, in name order.
func (m Manifest) OutputPorts() []agk.Port { return portNamesOf(m.Spec.Outputs) }

func portNamesOf(ports map[agk.Port]ManifestPort) []agk.Port {
	names := make([]agk.Port, 0, len(ports))
	for name := range ports {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// The grammars the manifest is written on. Each one is narrower than it looks and the
// documentation says why: a brick name is what a person types and what the catalog files
// a page under; a version moves with the brick and not with the image tag; a parameter
// name becomes a key of /agk/params.json and AGK_PARAM_<NAME>; a secret is mounted on
// tmpfs as one file directly under /agk/secrets/ and nowhere else. The file name keeps a
// dot, because client.key is what a key file is called, and begins with a letter or a digit,
// which is what refuses . and .., the directory itself and its parent rather than a file in
// it.
var (
	brickName      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	identifier     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	semanticNumber = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	paramName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	definitionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	accountName    = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*(?::[A-Za-z0-9_.][A-Za-z0-9_.-]*)?$`)
	rootAccount    = regexp.MustCompile(`^(?:0+|root)(?::|$)`)
	secretMount    = regexp.MustCompile(`^/agk/secrets/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cpuRequest     = regexp.MustCompile(`^(?:[0-9]*[1-9][0-9]*(?:\.[0-9]+)?|[0-9]+\.[0-9]*[1-9][0-9]*)$`)
	memoryRequest  = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
)

// ParseManifest reads /agk/brick.yaml and applies the manifest's own rules.
//
// The document is read with a YAML 1.2 parser and then read by hand, closed, on the same
// ground the envelope is: a manifest that does not hold together has to be told what is
// wrong with it in the words the documentation uses, and a JSON Schema error names a
// keyword and a JSON Pointer. The blocks the engine owns are closed, so "an unknown key
// on the root, on metadata, on spec, on a port entry, on runtime, on resources or on a
// secret entry fails publication rather than being ignored". The places where the author
// is writing JSON Schema stay open and travel through untouched: a params entry, a
// definitions entry and a port schema "carry the whole 2020-12 vocabulary".
func ParseManifest(doc []byte) (Manifest, error) {
	var v any
	if err := yaml.Unmarshal(doc, &v); err != nil {
		return Manifest{}, fmt.Errorf("the manifest is not a YAML document: %w", err)
	}
	value, err := jsonValue(v, "")
	if err != nil {
		return Manifest{}, err
	}
	root, err := block(value, "the manifest", "apiVersion", "kind", "metadata", "spec")
	if err != nil {
		return Manifest{}, err
	}

	var m Manifest
	// apiVersion is read before anything else, because it decides how every other key
	// is read, and kind is what tells a manifest from a workflow entry point: the two
	// documents share the same apiVersion.
	if m.APIVersion, err = constant(root, "apiVersion", "agentiik.dev/v1", "the manifest"); err != nil {
		return Manifest{}, err
	}
	if m.Kind, err = constant(root, "kind", "Brick", "the manifest"); err != nil {
		return Manifest{}, err
	}
	if m.Metadata, err = metadataOf(root); err != nil {
		return Manifest{}, err
	}
	if m.Spec, err = specOf(root); err != nil {
		return Manifest{}, err
	}

	// The document is kept as JSON so that a same-document pointer resolves against it,
	// with the boolean required lifted out of every parameter first: it is the
	// language's keyword and a JSON Schema implementation refuses a document carrying
	// it, so the manifest that is kept is the manifest as the engine reads it.
	stripParamRequired(root)
	if m.document, err = json.Marshal(root); err != nil {
		return Manifest{}, fmt.Errorf("the manifest cannot be read as a JSON document: %w", err)
	}
	return m, nil
}

func metadataOf(root map[string]any) (ManifestMetadata, error) {
	raw, ok := root["metadata"]
	if !ok {
		return ManifestMetadata{}, fmt.Errorf("the manifest declares no metadata: a brick carries an identity, and metadata.name and metadata.version are two of the five keys a manifest must declare")
	}
	b, err := block(raw, "metadata", "name", "version", "summary")
	if err != nil {
		return ManifestMetadata{}, err
	}

	var m ManifestMetadata
	if m.Name, err = text(b, "name", "metadata", true); err != nil {
		return ManifestMetadata{}, err
	}
	if !brickName.MatchString(m.Name) {
		return ManifestMetadata{}, fmt.Errorf("metadata.name is %q, which is not a brick name: lowercase letters and digits in hyphen-separated words, because a person types a brick name and the catalog files a page under it", m.Name)
	}
	if m.Version, err = text(b, "version", "metadata", true); err != nil {
		return ManifestMetadata{}, err
	}
	if !semanticNumber.MatchString(m.Version) {
		return ManifestMetadata{}, fmt.Errorf("metadata.version is %q, which is not a semantic version: the manifest carries the bare number, 1.4.0, and not the v the project tags with", m.Version)
	}
	if m.Summary, err = text(b, "summary", "metadata", false); err != nil {
		return ManifestMetadata{}, err
	}
	return m, nil
}

func specOf(root map[string]any) (ManifestSpec, error) {
	raw, ok := root["spec"]
	if !ok {
		return ManifestSpec{}, fmt.Errorf("the manifest declares no spec: a brick says what it needs from its container, and the runner builds the host configuration from it")
	}
	b, err := block(raw, "spec", "inputs", "outputs", "params", "secrets", "runtime", "resources", "definitions")
	if err != nil {
		return ManifestSpec{}, err
	}

	var s ManifestSpec
	if s.Inputs, err = portsOf(b, "inputs"); err != nil {
		return ManifestSpec{}, err
	}
	if s.Outputs, err = portsOf(b, "outputs"); err != nil {
		return ManifestSpec{}, err
	}
	if s.Params, err = paramsOf(b); err != nil {
		return ManifestSpec{}, err
	}
	if s.Secrets, err = secretsOf(b); err != nil {
		return ManifestSpec{}, err
	}
	if s.Runtime, err = runtimeOf(b); err != nil {
		return ManifestSpec{}, err
	}
	if s.Resources, err = resourcesOf(b); err != nil {
		return ManifestSpec{}, err
	}
	if s.Definitions, err = definitionsOf(b); err != nil {
		return ManifestSpec{}, err
	}
	return s, nil
}

func portsOf(spec map[string]any, key string) (map[agk.Port]ManifestPort, error) {
	raw, ok := spec[key]
	if !ok {
		return nil, nil
	}
	b, err := block(raw, "spec."+key, anyKey)
	if err != nil {
		return nil, err
	}
	ports := make(map[agk.Port]ManifestPort, len(b))
	for _, name := range sortedKeys(b) {
		port := agk.Port(name)
		if err := port.Validate(); err != nil {
			return nil, fmt.Errorf("spec.%s carries the port %q: %w. A port name travels three times over, as a directory under /agk/in/, as a file name under /agk/out/ports/ and as one entry of the comma-separated AGK_OUT_PORTS", key, name, err)
		}
		entry, err := block(b[name], "spec."+key+"."+name, "schema")
		if err != nil {
			return nil, fmt.Errorf("%w. A port entry carries schema and nothing else: what a step must feed is decided by the workflow, not asserted by the port", err)
		}
		schema, err := document(entry, "schema", "spec."+key+"."+name)
		if err != nil {
			return nil, err
		}
		ports[port] = ManifestPort{Schema: schema}
	}
	return ports, nil
}

func paramsOf(spec map[string]any) (map[string]Param, error) {
	raw, ok := spec["params"]
	if !ok {
		return nil, nil
	}
	b, err := block(raw, "spec.params", anyKey)
	if err != nil {
		return nil, err
	}
	params := make(map[string]Param, len(b))
	for _, name := range sortedKeys(b) {
		if !paramName.MatchString(name) {
			return nil, fmt.Errorf("spec.params carries the parameter %q, which is not an identifier: a parameter name is a key of /agk/params.json and is exported as AGK_PARAM_<NAME>, so it cannot carry a hyphen", name)
		}
		// A parameter declaration is open: it is the author's own JSON Schema, and a
		// keyword added inside it is none of the engine's business.
		entry, ok := b[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("spec.params.%s is not a JSON Schema subschema: a parameter is declared by the schema its value has to satisfy", name)
		}
		p := Param{}
		switch req := entry["required"].(type) {
		case nil, []any:
			// Absent, or the array form, which keeps its JSON Schema meaning on a
			// parameter that is itself an object and stays in the subschema.
		case bool:
			p.Required = req
		default:
			return nil, fmt.Errorf("spec.params.%s writes required as %s: it is either the boolean the language adds, saying a step must supply the parameter, or the array JSON Schema already means", name, kindOf(req))
		}
		if sensitive, ok := entry["sensitive"]; ok {
			b, ok := sensitive.(bool)
			if !ok {
				return nil, fmt.Errorf("spec.params.%s writes sensitive as %s, and it is a boolean: it marks a parameter that may carry a secret value", name, kindOf(sensitive))
			}
			p.Sensitive = b
		}
		schema, err := json.Marshal(withoutBooleanRequired(entry))
		if err != nil {
			return nil, fmt.Errorf("spec.params.%s cannot be read as a JSON Schema document: %w", name, err)
		}
		p.Schema = schema
		params[name] = p
	}
	return params, nil
}

func secretsOf(spec map[string]any) ([]Secret, error) {
	raw, ok := spec["secrets"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.secrets is not a list: a brick says which secrets it needs, one entry each")
	}
	secrets := make([]Secret, 0, len(list))
	for i, item := range list {
		where := fmt.Sprintf("spec.secrets[%d]", i)
		b, err := block(item, where, "name", "mount", "optional")
		if err != nil {
			return nil, err
		}
		var s Secret
		if s.Name, err = text(b, "name", where, true); err != nil {
			return nil, err
		}
		if len(s.Name) > agk.IdentifierMaxBytes {
			return nil, fmt.Errorf("%s names the secret %.64s..., which is %d characters long: it is the name the workflow gives the secret, and a name is at most %d, since a step is given the value in a file of that name wherever the brick says nothing else", where, s.Name, len(s.Name), agk.IdentifierMaxBytes)
		}
		if !identifier.MatchString(s.Name) {
			return nil, fmt.Errorf("%s names the secret %q, which is not an identifier: the brick says what it needs and the workflow decides which secret of its namespace answers to it, so it is the same name written on the same grammar", where, s.Name)
		}
		if s.Mount, err = text(b, "mount", where, false); err != nil {
			return nil, err
		}
		if s.Mount != "" && !secretMount.MatchString(s.Mount) {
			return nil, fmt.Errorf("%s mounts the secret at %q: the runner mounts a secret value on tmpfs as one file directly under /agk/secrets/, named with a letter or a digit and then letters, digits, dots, hyphens and underscores, and nowhere else", where, s.Mount)
		}
		if s.Optional, err = boolean(b, "optional", where); err != nil {
			return nil, err
		}
		secrets = append(secrets, s)
	}
	return secrets, nil
}

func runtimeOf(spec map[string]any) (Runtime, error) {
	raw, ok := spec["runtime"]
	if !ok {
		return Runtime{}, fmt.Errorf("the manifest declares no spec.runtime: the runner builds the host configuration from it, and spec.runtime.user is one of the five keys a manifest must declare")
	}
	b, err := block(raw, "spec.runtime", "network", "user", "streaming")
	if err != nil {
		return Runtime{}, err
	}

	var r Runtime
	if r.Network, err = text(b, "network", "spec.runtime", false); err != nil {
		return Runtime{}, err
	}
	if r.Network == "" {
		r.Network = "none"
	}
	if !slices.Contains([]string{"none", "egress", "internal"}, r.Network) {
		return Runtime{}, fmt.Errorf("spec.runtime.network is %q: a network posture is none, egress or internal, and there is no posture that puts a container on the host's", r.Network)
	}
	if r.User, err = text(b, "user", "spec.runtime", true); err != nil {
		return Runtime{}, fmt.Errorf("%w. It is required because the rule that refuses root has to have something to read: a manifest that leaves the account to the image gives publication nothing to check", err)
	}
	if !accountName.MatchString(r.User) {
		return Runtime{}, fmt.Errorf("spec.runtime.user is %q, which is not an account: it is a name or a uid, optionally with a group after a colon", r.User)
	}
	if rootAccount.MatchString(r.User) {
		return Runtime{}, fmt.Errorf("spec.runtime.user is %q: root, 0 and 0:0 are refused, because a read-only root filesystem and dropped capabilities are worth little to a process running as uid 0. The group half is not read, so nonroot:0 is accepted", r.User)
	}
	if r.Streaming, err = boolean(b, "streaming", "spec.runtime"); err != nil {
		return Runtime{}, err
	}
	return r, nil
}

func resourcesOf(spec map[string]any) (ManifestResources, error) {
	raw, ok := spec["resources"]
	if !ok {
		return ManifestResources{}, nil
	}
	b, err := block(raw, "spec.resources", "cpu", "memory", "pids")
	if err != nil {
		return ManifestResources{}, err
	}

	var r ManifestResources
	if cpu, ok := b["cpu"]; ok {
		s, ok := cpu.(string)
		if !ok {
			return ManifestResources{}, fmt.Errorf("spec.resources.cpu is written as %s: it is a quoted decimal string above zero, \"0.5\" or \"1\", so that half a core reads as 0.5 wherever it travels", kindOf(cpu))
		}
		if !cpuRequest.MatchString(s) {
			return ManifestResources{}, fmt.Errorf("spec.resources.cpu is %q: it is a decimal above zero, written as a string", s)
		}
		r.CPU = s
	}
	if memory, ok := b["memory"]; ok {
		s, ok := memory.(string)
		if !ok || !memoryRequest.MatchString(s) {
			return ManifestResources{}, fmt.Errorf("spec.resources.memory is %v: it is a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so 512M is refused", memory)
		}
		r.Memory = s
	}
	if pids, ok := b["pids"]; ok {
		n, err := whole(pids)
		if err != nil || n < 1 {
			return ManifestResources{}, fmt.Errorf("spec.resources.pids is %v: it is a whole number of processes, one or more", pids)
		}
		r.PIDs = n
	}
	return r, nil
}

func definitionsOf(spec map[string]any) (map[string]json.RawMessage, error) {
	raw, ok := spec["definitions"]
	if !ok {
		return nil, nil
	}
	b, err := block(raw, "spec.definitions", anyKey)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]json.RawMessage, len(b))
	for _, name := range sortedKeys(b) {
		if !definitionName.MatchString(name) {
			return nil, fmt.Errorf("spec.definitions carries %q, which is not a definition name: it is named by a pointer, #/spec/definitions/%s", name, name)
		}
		doc, err := json.Marshal(b[name])
		if err != nil {
			return nil, fmt.Errorf("spec.definitions.%s cannot be read as a JSON Schema document: %w", name, err)
		}
		definitions[name] = doc
	}
	return definitions, nil
}

// stripParamRequired removes the language's own boolean required from every parameter of
// the document that is kept, leaving the array form, which is JSON Schema's.
func stripParamRequired(root map[string]any) {
	spec, ok := root["spec"].(map[string]any)
	if !ok {
		return
	}
	params, ok := spec["params"].(map[string]any)
	if !ok {
		return
	}
	for name, raw := range params {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		params[name] = withoutBooleanRequired(entry)
	}
}

func withoutBooleanRequired(entry map[string]any) map[string]any {
	if _, ok := entry["required"].(bool); !ok {
		return entry
	}
	kept := make(map[string]any, len(entry))
	for k, v := range entry {
		if k == "required" {
			continue
		}
		kept[k] = v
	}
	return kept
}

// anyKey says that a block is keyed by names the author chooses rather than by a fixed
// set: the ports, the params and the definitions are maps, and it is their keys that are
// held to a grammar.
const anyKey = "*"

// block reads one mapping and refuses a key the block does not define, which is what
// makes the document closed wherever it is the engine's own: "a misspelled key is found
// where it was written".
func block(v any, where string, defined ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a block: it is written as a mapping of keys to values", where)
	}
	if slices.Contains(defined, anyKey) {
		return m, nil
	}
	for _, key := range sortedKeys(m) {
		if !slices.Contains(defined, key) {
			return nil, fmt.Errorf("%s carries the key %q, which the contract does not define: it carries %s", where, key, strings.Join(defined, ", "))
		}
	}
	return m, nil
}

func constant(b map[string]any, key, want, where string) (string, error) {
	got, err := text(b, key, where, true)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("%s declares %s %q, and %s reads %q and nothing else", where, key, got, key, want)
	}
	return got, nil
}

func text(b map[string]any, key, where string, required bool) (string, error) {
	v, ok := b[key]
	if !ok {
		if required {
			return "", fmt.Errorf("%s declares no %s", where, key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s is written as %s, and it is text", where, key, kindOf(v))
	}
	return s, nil
}

func boolean(b map[string]any, key, where string) (bool, error) {
	v, ok := b[key]
	if !ok {
		return false, nil
	}
	got, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s.%s is written as %s, and it is a boolean", where, key, kindOf(v))
	}
	return got, nil
}

// document returns one key of a block as the JSON document it is, which is how a schema
// travels out of here: as the bytes the author wrote, for whoever has a JSON Schema
// implementation to compile.
func document(b map[string]any, key, where string) (json.RawMessage, error) {
	v, ok := b[key]
	if !ok {
		return nil, nil
	}
	switch v.(type) {
	case map[string]any, bool:
	default:
		return nil, fmt.Errorf("%s.%s is written as %s: a JSON Schema document is an object of keywords, or a boolean", where, key, kindOf(v))
	}
	doc, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s.%s cannot be read as a JSON Schema document: %w", where, key, err)
	}
	return doc, nil
}

func whole(v any) (int, error) {
	switch n := v.(type) {
	case uint64:
		return int(n), nil
	case int64:
		return int(n), nil
	case int:
		return n, nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("%v is not a whole number", v)
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("%v is not a whole number", v)
}

// jsonValue puts a decoded YAML value on JSON's own terms, which is what every schema
// here is written in. A mapping keyed by anything but text is refused where it is
// written: JSON has no such key, and a manifest is a JSON document written in YAML.
func jsonValue(v any, where string) (any, error) {
	switch value := v.(type) {
	case map[string]any:
		converted := make(map[string]any, len(value))
		for k, sub := range value {
			c, err := jsonValue(sub, join(where, k))
			if err != nil {
				return nil, err
			}
			converted[k] = c
		}
		return converted, nil
	case map[any]any:
		converted := make(map[string]any, len(value))
		for k, sub := range value {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%s is keyed by %v, which is not text: a manifest is a JSON document written in YAML, and JSON has no key but a string", where, k)
			}
			c, err := jsonValue(sub, join(where, key))
			if err != nil {
				return nil, err
			}
			converted[key] = c
		}
		return converted, nil
	case []any:
		converted := make([]any, len(value))
		for i, sub := range value {
			c, err := jsonValue(sub, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return nil, err
			}
			converted[i] = c
		}
		return converted, nil
	default:
		return v, nil
	}
}

func join(where, key string) string {
	if where == "" {
		return key
	}
	return where + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// kindOf says what a value is in the file's own terms, so that a refusal about the wrong
// shape reads as the language and not as the reader: an author holding the file has
// written a number, a list or a block, and has never written a json.Number.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case string:
		return "text"
	case bool:
		return "a boolean"
	case json.Number, uint64, int64, float64, int:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a block"
	}
	return "something the language does not write"
}
