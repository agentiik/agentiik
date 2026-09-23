package graph

import (
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
// name a file outside the tree. A workflow include arrives already fetched, in remote,
// because resolving one is reaching another repository at a ref, and reaching another
// repository is not something this package does.
func Load(fsys fs.FS, entry string, remote map[WorkflowRef]Fragment) (*Workflow, error) {
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
	wf, err := Parse(doc)
	if err != nil {
		return nil, err
	}

	// What the includes carry, in declaration order, each one overriding what came
	// before it and all of them under the entry point's own.
	held := &included{
		blocks:  map[string]stepValues{},
		values:  map[agk.Step]stepValues{},
		vars:    Vars{},
		visited: map[string]bool{},
		open:    map[string]bool{entry: true},
	}
	if err := held.gather(fsys, path.Dir(entry), wf.Include, remote); err != nil {
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
	defaults := held.defaults
	applyDefaults(&defaults, wf.Defaults)

	wf.blocks, wf.values, wf.Defaults = blocks, values, defaults
	if len(vars) > 0 {
		wf.Vars = vars
	}
	if len(secrets) > 0 {
		wf.Secrets = secrets
	}
	// Everything that will ever be included has been, so an extends naming a block
	// nobody carries is a refusal now rather than a value quietly missing.
	if err := wf.resolveSteps(true); err != nil {
		return nil, err
	}
	return wf, nil
}

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
	defaults Defaults
	visited  map[string]bool
	open     map[string]bool
}

// gather resolves one include list in declaration order. A fragment's own includes are
// resolved before the fragment itself, which is the same rule one level down: what a file
// includes sits under what the file writes.
func (in *included) gather(fsys fs.FS, dir string, includes []Include, remote map[WorkflowRef]Fragment) error {
	for _, include := range includes {
		var f Fragment
		var at string
		switch {
		case include.Path != "":
			name, err := inside(dir, include.Path)
			if err != nil {
				return err
			}
			if in.open[name] {
				return fmt.Errorf("the include of %s comes back to a file that is still being resolved: a file cannot include itself, directly or through another", name)
			}
			if in.visited[name] {
				continue
			}
			doc, err := fs.ReadFile(fsys, name)
			if err != nil {
				return fmt.Errorf("including %s: %w. A path include resolves inside the same commit, so the file has to be in the tree the run was pinned to", include.Path, err)
			}
			parsed, err := ParseFragment(doc)
			if err != nil {
				return fmt.Errorf("including %s: %w", include.Path, err)
			}
			f, at = *parsed, name
		default:
			ref := include.Workflow
			at = ref.text()
			if in.open[at] {
				return fmt.Errorf("the include of %s comes back to a workflow that is still being resolved", at)
			}
			if in.visited[at] {
				continue
			}
			fragment, ok := remote[ref]
			if !ok {
				return fmt.Errorf("the workflow include %s was not resolved: a workflow include reaches another repository at a ref, requires workflow:read on it, and arrives here already fetched", at)
			}
			// "A path include resolves inside the same commit", and the commit a
			// fetched fragment was written in is the other repository's. Resolving
			// one against the tree in hand would read a file of this repository in
			// another's name, which is the one thing a pinned include exists to
			// prevent, so the caller that fetched the fragment resolves its paths
			// there and hands over what came back.
			for _, nested := range fragment.include {
				if nested.Path != "" {
					return fmt.Errorf("the workflow include %s carries a path include of its own, %q: a path include resolves inside the same commit, and that commit is %s's rather than the one this run is pinned to, so a fetched fragment arrives with its own paths already resolved", at, nested.Path, at)
				}
			}
			f = fragment
		}

		// What this fragment includes sits under what it writes itself, which is the
		// same rule one level down.
		in.open[at] = true
		err := in.gather(fsys, dirOf(at, include), f.include, remote)
		delete(in.open, at)
		in.visited[at] = true
		if err != nil {
			return err
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
		applyDefaults(&in.defaults, f.defaults)
	}
	return nil
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

// dirOf is the directory a fragment's own path includes resolve against: its own, for a
// file of this repository. A fragment fetched from another repository resolves its paths
// in that repository, which this one never reads, so the caller resolves them there and
// hands over what came back.
func dirOf(at string, include Include) string {
	if include.Path == "" {
		return "."
	}
	return path.Dir(at)
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
	for _, name := range slices.Sorted(maps.Keys(w.values)) {
		st, err := resolveStep(name, w.values[name], w.blocks, w.Defaults, complete)
		if err != nil {
			return err
		}
		steps[name] = st
	}
	w.Steps = steps
	w.resolved = complete
	return nil
}

// resolveStep applies, in order, the blocks the step extends from the outermost inwards,
// then defaults, then what the step wrote itself.
func resolveStep(name agk.Step, own stepValues, blocks map[string]stepValues, defaults Defaults, complete bool) (Step, error) {
	chain, err := ancestry(name, own, blocks, complete)
	if err != nil {
		return Step{}, err
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

	return materialise(merged), nil
}

// ancestry is the blocks a step extends, outermost first. "extends depth-first" is what
// this walk is: a block that itself extends another is resolved before the block that
// named it, so the values nearest the step are the ones applied last.
func ancestry(name agk.Step, own stepValues, blocks map[string]stepValues, complete bool) ([]stepValues, error) {
	var chain []stepValues
	seen := map[string]bool{}
	for at := own.Extends; at != ""; {
		if seen[at] {
			return nil, fmt.Errorf("step %s extends %s, which comes back to itself: a hidden block is inherited from, and inheritance that returns to where it started never resolves", name, at)
		}
		seen[at] = true
		block, ok := blocks[at]
		if !ok {
			if !complete {
				// A file that declares includes has not been given them yet, and the
				// block may be in one of them. Load settles it.
				break
			}
			return nil, fmt.Errorf("step %s extends %s, which nothing declares: only a hidden block can be extended, and it is declared at the root of the entry point, at the root of an included file, or among the steps", name, at)
		}
		chain = append(chain, block)
		at = block.Extends
	}
	slices.Reverse(chain)
	return chain, nil
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
	s.Extends = v.Extends
	return s
}
