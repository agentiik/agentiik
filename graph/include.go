package graph

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// Load reads the entry point out of the repository tree and resolves what it includes.
//
// "Resolution is ordered: includes first, in declaration order, then extends depth-first,
// then defaults, then the step's own values." That is an order of application, and the
// last writer wins, which is what makes "then the step's own values" the last of them
// mean anything.
//
// The tree is an fs.FS already pinned to the commit, so "a path include resolves inside
// the same commit, so it can never be stale and nothing has to pin it", and it cannot
// name a file outside the tree. A workflow include is reached through remote, because
// resolving one is reaching another repository at a ref, and reaching another repository
// is not something this package does: remote answers the other repository's tree at the
// commit the ref resolved to, and this package reads it. A nil remote refuses every
// workflow include.
//
// It reads a tree a version is about to be made of, with Parse and ParseFragment, so a port and a
// workflow output are held to agk.PortMaxBytes in every file. LoadStored reads what a version
// already holds.
func Load(fsys fs.FS, entry string, remote Remote) (*Workflow, error) {
	return load(fsys, entry, remote, Parse, ParseFragment)
}

// LoadStored is Load for a version already stored: the same resolution over the same files, with
// every rule but the ones added since a version could be stored, the bound a port and a workflow
// output are written to, agk.PortMaxBytes, among them.
//
// A version recorded before that bound was may name a port or an output of 251 to 255 characters,
// and its runs, its replays and the rebuild of its graph have to go on as they did before the
// upgrade, since nothing may break for what an installation already holds. The bound is refused
// where a version is made, by agk validate, agk run --local, agk push and the push route, and
// never where one is read back, which is package version's Build.
func LoadStored(fsys fs.FS, entry string, remote Remote) (*Workflow, error) {
	return load(fsys, entry, remote, parse, parseFragment)
}

// Remote reaches the repositories a workflow include names.
//
// "A workflow include must carry ref, a tag or a commit, and needs workflow:read on that
// repository", and both the ref and the permission are the caller's to settle: which tag
// names which commit, who is asking and whether they may, are the installation's. What
// comes back is the other repository's tree at the commit the ref resolved to, and that
// commit, whole, which the resolved graph records so that the other repository moving a
// tag afterwards changes nothing this one does.
type Remote interface {
	Include(ref WorkflowRef) (fs.FS, string, error)
}

// libraryEntry is the file a workflow include reads in the other repository: "its root
// agentiik.yaml, written as a fragment".
const libraryEntry = "agentiik.yaml"

// LoadLibrary reads a library repository's root agentiik.yaml as a fragment and resolves what it
// includes, which is what "its own hook validates as a fragment" means: the file read on a
// fragment's terms, every path include inside the tree and every workflow include reached through
// remote, each at most once and none coming back to a file still being resolved. It answers what
// was included, in the order it applied.
//
// Nothing else is held of it. A step the library writes may extend a block the workflow including
// it declares, or be completed by that workflow's defaults, so the steps are resolved where the
// library is included, against everything the workflow reaches, and never on their own here.
func LoadLibrary(fsys fs.FS, remote Remote) ([]Included, error) {
	return loadLibrary(fsys, remote, ParseFragment)
}

// LoadStoredLibrary is LoadLibrary for a library already stored, with the rules it was stored
// under, as LoadStored is Load's.
func LoadStoredLibrary(fsys fs.FS, remote Remote) ([]Included, error) {
	return loadLibrary(fsys, remote, parseFragment)
}

func loadLibrary(fsys fs.FS, remote Remote, fragment func([]byte) (*Fragment, error)) ([]Included, error) {
	if fsys == nil {
		return nil, fmt.Errorf("the library cannot be loaded: its root %s is read out of the tree of a commit", libraryEntry)
	}
	doc, err := fs.ReadFile(fsys, libraryEntry)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", libraryEntry, err)
	}
	f, err := fragment(doc)
	if err != nil {
		return nil, inFile(err, libraryEntry)
	}
	f.src.name = libraryEntry
	held := &included{
		blocks:   map[string]stepValues{},
		values:   map[agk.Step]stepValues{},
		vars:     Vars{},
		visited:  map[string]bool{},
		open:     map[string]bool{libraryEntry: true},
		fragment: fragment,
		remote:   remote,
	}
	if err := held.gather(fileTree{fsys: fsys}, ".", f.include, f.includeAt); err != nil {
		return nil, err
	}
	return held.applied, nil
}

// load is Load and LoadStored, reading the entry point with workflow and every file it includes
// with fragment.
func load(fsys fs.FS, entry string, remote Remote, workflow func([]byte) (*Workflow, error), fragment func([]byte) (*Fragment, error)) (*Workflow, error) {
	if fsys == nil {
		return nil, fmt.Errorf("the workflow cannot be loaded: a run is pinned to a commit and the tree of that commit is what the entry point is read out of")
	}
	if !fs.ValidPath(entry) {
		return nil, fmt.Errorf("%q is not a path in the repository tree: agentiik.yaml at the root is the entry point", entry)
	}
	doc, err := fs.ReadFile(fsys, entry)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", entry, err)
	}
	wf, err := workflow(doc)
	if err != nil {
		return nil, inFile(err, entry)
	}
	wf.src.name = entry

	// What the includes carry, in declaration order, each one overriding what came
	// before it and all of them under the entry point's own.
	held := &included{
		blocks:   map[string]stepValues{},
		values:   map[agk.Step]stepValues{},
		vars:     Vars{},
		visited:  map[string]bool{},
		open:     map[string]bool{entry: true},
		fragment: fragment,
		remote:   remote,
	}
	if err := held.gather(fileTree{fsys: fsys}, path.Dir(entry), wf.Include, wf.includeAt); err != nil {
		return nil, err
	}

	// The entry point is applied over what it included, keyword by keyword, so that a
	// file reading its own line is reading what wins.
	blocks := held.blocks
	maps.Copy(blocks, wf.blocks)
	values := make(map[agk.Step]stepValues, len(held.values)+len(wf.values))
	for name, v := range held.values {
		values[name] = v
	}
	for name, own := range wf.values {
		if from, ok := values[name]; ok {
			applyValues(&from, own)
			values[name] = from
			continue
		}
		values[name] = own
	}
	vars := held.vars
	maps.Copy(vars, wf.Vars)
	secrets := union(held.secrets, wf.Secrets)
	secretAt := held.secretAt
	for name, at := range wf.secretAt {
		if _, first := secretAt[name]; !first {
			if secretAt == nil {
				secretAt = map[string]origin{}
			}
			secretAt[name] = at
		}
	}
	defaults := held.defaults
	applyDefaults(&defaults, wf.Defaults)

	wf.blocks, wf.values, wf.Defaults = blocks, values, defaults
	if len(vars) > 0 {
		wf.Vars = vars
	}
	if len(secrets) > 0 {
		wf.Secrets = secrets
	}
	wf.secretAt = secretAt
	wf.included = held.applied
	// Everything that will ever be included has been, so an extends naming a block
	// nobody carries is a refusal now rather than a value quietly missing.
	if err := wf.resolveSteps(true); err != nil {
		return nil, err
	}
	return wf, nil
}

// inFile names the file a refusal read out of one document is about, which the reader of the
// document could not: it was handed bytes, and the loader knows which file of the tree they were.
func inFile(err error, name string) error {
	var r *Refusal
	if errors.As(err, &r) && r.At.File == "" {
		r.At.File = name
	}
	return err
}

// fileTree is one tree an include resolves in: this commit's, or the commit of another repository
// a workflow include resolved to. label is how a file of it is named where a refusal names one:
// empty for this tree, whose files are named by their paths, and the repository and the ref for
// another, git's <ref>:<path>.
//
// What resolution remembers having read is keyed apart from that: by the path alone in this tree,
// and behind a null byte in another, which no path of a tree holds. A file of one is never taken
// for the file of the same path in the other, nor a workflow include for a file of this tree whose
// path happens to be finance/common@v2.1.0.
type fileTree struct {
	fsys  fs.FS
	label string
	key   string
}

func (t fileTree) name(p string) string { return t.label + p }
func (t fileTree) id(p string) string   { return t.key + p }

// workflowKey is what resolution remembers a workflow include by.
func workflowKey(ref WorkflowRef) string { return "\x00" + ref.text() }

// included is what the include block has contributed so far, and what it has already
// read.
//
// Two files including a third is reuse working as it should, so a file already included
// is passed over rather than read twice: it would contribute the same blocks the second
// time. A file that includes itself, directly or through another, is the other thing
// entirely, and open is the chain being resolved at this moment, which is what tells one
// from the other.
type included struct {
	blocks   map[string]stepValues
	values   map[agk.Step]stepValues
	vars     Vars
	secrets  []string
	secretAt map[string]origin
	defaults Defaults
	visited  map[string]bool
	open     map[string]bool

	// applied is every include in the order it applied, what a file includes before the file
	// itself.
	applied []Included

	// fragment reads an included file: ParseFragment where a version is made, and its stored
	// reading where one is read back.
	fragment func([]byte) (*Fragment, error)
	remote   Remote
}

// gather resolves one include list in declaration order. A fragment's own includes are
// resolved before the fragment itself, which is the same rule one level down: what a file
// includes sits under what the file writes.
//
// at is where each include of the list is written, which is what a refusal of one points at.
func (in *included) gather(t fileTree, dir string, includes []Include, at []origin) error {
	for i, include := range includes {
		written := Position{}
		if i < len(at) {
			written = at[i].value()
		}
		var f *Fragment
		var key, root, below string
		var within fileTree
		var applied Included
		switch {
		case include.Path != "":
			name, err := inside(dir, include.Path)
			if err != nil {
				return placed(refuse(RuleIncludeLeavesTree, "", "", err.Error()), written)
			}
			key = t.id(name)
			if in.open[key] {
				return placed(refuse(RuleIncludeCycle, "", "", fmt.Sprintf("the include of %s comes back to a file that is still being resolved: a file cannot include itself, directly or through another, since its resolution would never end", t.name(name))), written)
			}
			if in.visited[key] {
				continue
			}
			doc, err := fs.ReadFile(t.fsys, name)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return placed(refuse(RuleIncludeMissing, "", "", fmt.Sprintf("including %s: %v. A path include resolves against the directory of the file naming it, inside the same commit, so the file has to be in the tree the run was pinned to, at %s", include.Path, err, name)), written)
			case err != nil:
				return fmt.Errorf("including %s: %w", include.Path, err)
			}
			if f, err = in.fragment(doc); err != nil {
				return fmt.Errorf("including %s: %w", include.Path, inFile(err, t.name(name)))
			}
			within, below, root = t, path.Dir(name), key
			f.src.name = t.name(name)
			if t.label == "" {
				applied = Included{Path: name}
			}
		default:
			ref := include.Workflow
			key = workflowKey(ref)
			if in.open[key] {
				return placed(refuse(RuleIncludeCycle, "", "", fmt.Sprintf("the include of %s comes back to a workflow that is still being resolved: a file cannot include itself, directly or through another, since its resolution would never end", ref.text())), written)
			}
			if in.visited[key] {
				continue
			}
			if in.remote == nil {
				return placed(fmt.Errorf("the workflow include %s was not resolved: a workflow include reaches another repository at a ref, requires workflow:read on it, and nothing here reaches another repository", ref.text()), written)
			}
			fsys, commit, err := in.remote.Include(ref)
			if err != nil {
				return placed(fmt.Errorf("the workflow include %s: %w", ref.text(), err), written)
			}
			// "A workflow include reads the other repository's root agentiik.yaml,
			// written as a fragment", and its own path includes resolve inside that
			// repository, at that commit.
			within = fileTree{fsys: fsys, label: ref.text() + ":", key: key + ":"}
			doc, err := fs.ReadFile(fsys, libraryEntry)
			if err != nil {
				return placed(fmt.Errorf("the workflow include %s: reading its root %s: %w. A workflow include reads the other repository's root %s, written as a fragment", ref.text(), libraryEntry, err, libraryEntry), written)
			}
			if f, err = in.fragment(doc); err != nil {
				return fmt.Errorf("the workflow include %s: %w", ref.text(), inFile(err, within.name(libraryEntry)))
			}
			below, root = ".", within.id(libraryEntry)
			f.src.name = within.name(libraryEntry)
			applied = Included{Workflow: ref, Commit: commit}
		}

		// What this fragment includes sits under what it writes itself, which is the
		// same rule one level down. A library's root file is open under its own path as
		// well as the include's, so that one of its files including it again is the
		// ring it is.
		in.open[key], in.open[root] = true, true
		err := in.gather(within, below, f.include, f.includeAt)
		delete(in.open, key)
		delete(in.open, root)
		in.visited[key], in.visited[root] = true, true
		if err != nil {
			return err
		}
		if applied != (Included{}) {
			in.applied = append(in.applied, applied)
		}
		maps.Copy(in.blocks, f.blocks)
		for name, own := range f.values {
			if from, ok := in.values[name]; ok {
				applyValues(&from, own)
				in.values[name] = from
				continue
			}
			in.values[name] = own
		}
		maps.Copy(in.vars, f.vars)
		in.secrets = union(in.secrets, f.secrets)
		for name, at := range f.secretAt {
			if _, first := in.secretAt[name]; !first {
				if in.secretAt == nil {
					in.secretAt = map[string]origin{}
				}
				in.secretAt[name] = at
			}
		}
		applyDefaults(&in.defaults, f.defaults)
	}
	return nil
}

// placed puts a refusal where the include it is about is written. A failure that is not a
// refusal of the language, another repository that could not be reached, is said at the same
// place all the same.
func placed(err error, at Position) error {
	var r *Refusal
	if errors.As(err, &r) {
		r.At = at
		return r
	}
	if at.File == "" {
		return err
	}
	return fmt.Errorf("%s: %w", at, err)
}

// union is the secrets two files name, each once, in the order they were first named.
//
// A name is not a value that one file can override in another: it says a secret is used, and
// the namespace says where it lives. So an include and the file including it naming the same
// secret agree rather than collide, and the only thing left to decide is the order, which is
// the order the files were read in.
func union(held, own []string) []string {
	out := slices.Clone(held)
	for _, name := range own {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// inside resolves an include path against the file that wrote it and refuses one that
// leaves the tree. "Everything in the tree is readable by anyone who can read the
// workflow", and nothing outside it is readable at all.
func inside(dir, name string) (string, error) {
	joined := path.Join(dir, name)
	if strings.HasPrefix(name, "/") {
		joined = path.Clean(strings.TrimPrefix(name, "/"))
	}
	if !fs.ValidPath(joined) || strings.HasPrefix(joined, "../") {
		return "", fmt.Errorf("the include of %q leaves the repository tree: a path include resolves inside the same commit", name)
	}
	return joined, nil
}

// resolveSteps turns the steps as they were written into the steps the graph runs, in
// the order the language fixes.
//
// complete says whether every block that will ever exist is in hand. Parse is given one
// document, so a file that declares includes has not been given the blocks they carry and
// an extends naming one is left standing; Load has them all, and an extends naming
// nothing is refused there.
func (w *Workflow) resolveSteps(complete bool) error {
	steps := make(map[agk.Step]Step, len(w.values))
	origins := make(map[agk.Step]map[string]origin, len(w.values))
	for _, name := range slices.Sorted(maps.Keys(w.values)) {
		st, written, err := resolveStep(name, w.values[name], w.blocks, w.Defaults, complete)
		if err != nil {
			return err
		}
		steps[name], origins[name] = st, written
	}
	w.Steps = steps
	w.origins = origins
	w.resolved = complete
	return nil
}

// resolveStep applies, in order, the blocks the step extends from the outermost inwards,
// then defaults, then what the step wrote itself. It answers the step, and where each keyword
// that won was written.
func resolveStep(name agk.Step, own stepValues, blocks map[string]stepValues, defaults Defaults, complete bool) (Step, map[string]origin, error) {
	names, chain, err := ancestry(name, own, blocks, complete)
	if err != nil {
		return Step{}, nil, err
	}

	var merged stepValues
	for _, layer := range chain {
		applyValues(&merged, layer)
	}
	applyValues(&merged, stepValues{Defaults: defaults})
	applyValues(&merged, own)

	// before_script and after_script are the exception to the last writer winning:
	// they are "merged from defaults and extends outwards in", so every layer's
	// commands are kept and the step's own are the innermost. after_script is merged
	// the same way, because the two keywords are one mechanism read from both ends of
	// the script, and a cleanup a block contributes is exactly what a step extending it
	// asked for.
	merged.BeforeScript = slices.Clone(defaults.BeforeScript)
	merged.AfterScript = slices.Clone(defaults.AfterScript)
	for _, layer := range chain {
		merged.BeforeScript = append(merged.BeforeScript, layer.BeforeScript...)
		merged.AfterScript = append(merged.AfterScript, layer.AfterScript...)
	}
	merged.BeforeScript = append(merged.BeforeScript, own.BeforeScript...)
	merged.AfterScript = append(merged.AfterScript, own.AfterScript...)

	st := materialise(merged)
	st.Extends = names
	return st, merged.written, nil
}

// ancestry is the blocks a step extends, outermost first, and their names in the order the
// step reaches them, nearest first. "extends depth-first" is what this walk is: a block that
// itself extends another is resolved before the block that named it, so the values nearest
// the step are the ones applied last.
//
// A block this document does not carry yet is named all the same, so that a step read before
// its includes still says it extends something.
func ancestry(name agk.Step, own stepValues, blocks map[string]stepValues, complete bool) ([]string, []stepValues, error) {
	var names []string
	var chain []stepValues
	seen := map[string]bool{}
	for at := own.Extends; at != ""; {
		if seen[at] {
			return nil, nil, fmt.Errorf("step %s extends %s, which comes back to itself: a hidden block is inherited from, and inheritance that returns to where it started never resolves", name, at)
		}
		seen[at] = true
		names = append(names, at)
		block, ok := blocks[at]
		if !ok {
			if !complete {
				// A file that declares includes has not been given them yet, and the
				// block may be in one of them. Load settles it.
				break
			}
			return nil, nil, fmt.Errorf("step %s extends %s, which nothing declares: only a hidden block can be extended, and it is declared at the root of the entry point, at the root of an included file, or among the steps", name, at)
		}
		chain = append(chain, block)
		at = block.Extends
	}
	slices.Reverse(chain)
	return names, chain, nil
}

// applyValues writes everything src says over dst, keyword by keyword. A keyword src
// did not write leaves dst as it was, which is the whole of what inheritance is here.
func applyValues(dst *stepValues, src stepValues) {
	applyDefaults(&dst.Defaults, src.Defaults)

	if src.Image != nil {
		dst.Image = src.Image
		dst.Call = nil
	}
	if src.Call != nil {
		dst.Call = src.Call
		dst.Image = nil
	}
	if src.Needs != nil {
		dst.Needs = src.Needs
	}
	if src.Inputs != nil {
		dst.Inputs = src.Inputs
	}
	if src.Outputs != nil {
		dst.Outputs = src.Outputs
	}
	if src.Params != nil {
		// Parameters merge by name rather than wholesale: a block that sets an
		// endpoint and a step that adds a currency are the two halves of one call, and
		// a step overriding one parameter has not withdrawn the others.
		if dst.Params == nil {
			dst.Params = map[string]any{}
		}
		maps.Copy(dst.Params, src.Params)
	}
	if src.If != nil {
		dst.If = src.If
	}
	if src.Merge != nil {
		dst.Merge = src.Merge
		dst.Join = src.Join
	}
	if src.Strategy != nil {
		dst.Strategy = src.Strategy
	}
	if src.Script != nil {
		dst.Script = src.Script
	}
	if src.Extends != "" {
		dst.Extends = src.Extends
	}
}

// applyDefaults writes the execution settings src carries over dst.
func applyDefaults(dst *Defaults, src Defaults) {
	if src.Timeout != nil {
		dst.Timeout = src.Timeout
	}
	if src.Retain != nil {
		dst.Retain = src.Retain
	}
	if src.Retry != nil {
		dst.Retry = src.Retry
	}
	if src.Resources != nil {
		dst.Resources = src.Resources
	}
	if src.Network != nil {
		dst.Network = src.Network
	}
	if src.EgressAllow != nil {
		dst.EgressAllow = src.EgressAllow
	}
	if src.RunsOn != nil {
		dst.RunsOn = src.RunsOn
	}
	if src.Secrets != nil {
		dst.Secrets = src.Secrets
	}
	if src.Cache != nil {
		dst.Cache = src.Cache
	}
	if src.ContinueOnError != nil {
		dst.ContinueOnError = src.ContinueOnError
	}
	if src.Idempotent != nil {
		dst.Idempotent = src.Idempotent
	}
	if src.Files != nil {
		dst.Files = src.Files
	}
	if src.Shell != nil {
		dst.Shell = src.Shell
	}
	if src.BeforeScript != nil {
		dst.BeforeScript = src.BeforeScript
	}
	if src.AfterScript != nil {
		dst.AfterScript = src.AfterScript
	}
	if src.When != nil {
		dst.When = src.When
	}
	// Where each keyword src writes was written moves with it. The map is copied before it
	// is written to, since a layer is applied to many steps and each keeps its own.
	if len(src.written) > 0 {
		written := maps.Clone(dst.written)
		if written == nil {
			written = make(map[string]origin, len(src.written))
		}
		maps.Copy(written, src.written)
		dst.written = written
	}
}

// The default shell, which is the one the documentation writes: "Defaults to
// ["/bin/sh", "-e"]".
var defaultShell = []string{"/bin/sh", "-e"}

// materialise turns the merged values into the step, applying the defaults the language
// states for a keyword nobody wrote.
//
// Only two of them are written in rather than left to whoever reads the step later.
// idempotent is "a boolean, true by default", and it decides whether a lost task is
// requeued and whether the step can be cached, which are two questions asked far from
// here. shell is the interpreter for script, and a task carries "everything a driver
// needs and nothing it has to look up", so a step that runs commands says which shell
// runs them.
func materialise(v stepValues) Step {
	var s Step
	if v.Image != nil {
		s.Image = *v.Image
	}
	s.Call = v.Call
	s.Needs = v.Needs
	s.Inputs = v.Inputs
	s.Outputs = v.Outputs
	s.Params = v.Params
	if v.If != nil {
		s.If = *v.If
	}
	s.When = v.When
	if v.Merge != nil {
		s.Merge = *v.Merge
	}
	if v.Join != nil {
		s.Join = *v.Join
	}
	if v.Strategy != nil {
		s.Strategy = *v.Strategy
	}
	if v.Retry != nil {
		s.Retry = *v.Retry
	}
	if v.Timeout != nil {
		s.Timeout = *v.Timeout
	}
	if v.Retain != nil {
		s.Retain = *v.Retain
	}
	if v.ContinueOnError != nil {
		s.ContinueOnError = *v.ContinueOnError
	}
	if v.Resources != nil {
		s.Resources = *v.Resources
	}
	if v.Network != nil {
		s.Network = *v.Network
	}
	s.EgressAllow = v.EgressAllow
	s.RunsOn = v.RunsOn
	s.Secrets = v.Secrets
	if v.Cache != nil {
		s.Cache = *v.Cache
	}
	s.Idempotent = true
	if v.Idempotent != nil {
		s.Idempotent = *v.Idempotent
	}
	s.Files = v.Files
	s.Script = v.Script
	s.BeforeScript = v.BeforeScript
	s.AfterScript = v.AfterScript
	s.Shell = v.Shell
	if len(s.Shell) == 0 && len(s.Script)+len(s.BeforeScript)+len(s.AfterScript) > 0 {
		s.Shell = defaultShell
	}
	return s
}
