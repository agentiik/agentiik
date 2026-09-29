package version

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// Check judges a tree as the version it would be made of, and is the one place that is done.
//
// agk validate and agk run --local call it on a working tree, agk push on the commit it is about
// to push, the push route on the tree a push carries, and the pre-receive hook on every tip a
// git push moves, so that the command line and the hook can never disagree: "agk validate and
// agk push make the same checks where they reach the stores the hook reads". What differs
// between them is what they reach, which is what c says, and never a rule.
//
// In order: the tree itself, where it is a commit's (no symbolic link, no submodule, no name
// that is not UTF-8 or is .git somewhere, no path past its bound); the entry point, at the root
// where c names none; the entry point and everything it includes, read and resolved; the name
// the push is made under; the graph; the declared inputs; each image a step names by a tag,
// pinned to the digest its repository recorded for it; each brick step, held to the manifest
// recorded for its image; the secrets, against the pusher's secret:use and the namespace's
// declarations. The first refusal is answered, placed where the value refused was written.
//
// A commit already stored is judged by the rules it was stored under and no rule added since,
// which c.Stored says: "a push of a commit already stored is answered as that version,
// unchanged, without judging it again by rules added after it was stored", so that an agk of the
// release that stored it, pushing it again after an upgrade, meets no refusal it did not meet
// then. What stood then still applies: the graph, the manifests it was held to, secret:use and
// the inputs' declaration.
func Check(ctx context.Context, tree fs.FS, c Checking) (*Checked, error) {
	if tree == nil {
		return nil, fmt.Errorf("version: there is no tree to check, and a version is the commit a tree is")
	}
	made := !c.Stored

	if c.Committed && made {
		if err := checkTree(tree); err != nil {
			return nil, err
		}
	}
	entry := c.Entry
	if entry == "" {
		entry = EntryPoint
		if made {
			if err := entryPoint(tree); err != nil {
				return nil, err
			}
		}
	}

	// What a version is rebuilt from is what resolution read, recorded as it read it: exact by
	// construction, and the only way to be exact, since an include may itself include and
	// working out the closure by parsing the include blocks again would be this package
	// reimplementing resolution in order to agree with resolution.
	watched := &watcher{under: tree, read: map[string][]byte{}}
	load := graph.Load
	if c.Stored {
		load = graph.LoadStored
	}
	var remote graph.Remote
	if c.Include != nil {
		remote = reaching{ctx: ctx, include: c.Include}
	}
	wf, err := load(watched, entry, remote)
	if err != nil {
		return nil, err
	}
	if made {
		if err := underItsName(wf, c); err != nil {
			return nil, err
		}
	}
	if err := graph.Check(wf); err != nil {
		return nil, err
	}
	// The boundary a trigger fills, and a schema that does not compile is a workflow whose
	// first run cannot start. Compiled against the tree rather than what resolution read,
	// since a schema's $ref names a file of the commit, which is not an include.
	if _, err := wf.DeclaredInputs(tree); err != nil {
		return nil, err
	}

	written := make(map[agk.Step]string, len(wf.Steps))
	for name, st := range wf.Steps {
		written[name] = st.Image
	}
	pins, err := pinned(ctx, wf, c)
	if err != nil {
		return nil, err
	}

	checked := &Checked{Workflow: wf, Commit: c.Commit, Version: db.Version{Entry: entry, Includes: map[string][]byte{}, Manifests: map[string][]byte{}}}
	if len(pins) > 0 {
		checked.Version.Images = pins
	}
	if c.Manifest != nil {
		read, err := manifestsOf(ctx, wf, c)
		if err != nil {
			return nil, err
		}
		manifests := make(map[string]brick.Manifest, len(read))
		for image, m := range read {
			manifests[image] = m
		}
		if checked.Graph, err = graph.Build(wf, manifests); err != nil {
			return nil, err
		}
		// Kept by the reference each step wrote, which is how a version holds them: the
		// digest a tag was resolved to is beside it in Images.
		for name, st := range wf.Steps {
			if m, ok := read[st.Image]; ok && len(st.Script) == 0 {
				checked.Version.Manifests[written[name]] = m.Document()
			}
		}
	}

	if err := secretsOf(ctx, wf, c); err != nil {
		return nil, err
	}

	for p, body := range watched.read {
		if p == entry {
			checked.Version.Document = body
			continue
		}
		checked.Version.Includes[p] = body
	}
	if checked.Version.Document == nil {
		return nil, fmt.Errorf("version: the entry point %s was not read", entry)
	}
	return checked, nil
}

// Checking is what Check is told beside the tree.
type Checking struct {
	// Entry is the path of the entry point in the tree. Empty is a repository's own,
	// agentiik.yaml at the root, where a tree holding none is refused naming the one it holds
	// below, if any: what a hook and agk push judge. agk validate names the file -f names, in
	// the tree of the directory it is in.
	Entry string

	// Commit is the commit the tree is, whole, which the resolved graph records. A working
	// tree is no commit.
	Commit string

	// Committed says the tree is a commit's, which is held to the rules every tree a version is
	// made of is held to. A working copy is not a commit's tree: its .git is the repository
	// itself, and what agk push reads out of it is.
	Committed bool

	// Namespace and Repository are where the push is made, which the file's metadata has to
	// agree with: metadata.name is the repository's name, and metadata.namespace, where the
	// file writes one, the namespace. Either is empty where the caller does not know it, and
	// then nothing is held to it: agk validate knows neither, and agk push names the
	// repository after the file.
	Namespace  string
	Repository string

	// Stored says the commit is a version already, which is judged by the rules it was stored
	// under and none added since.
	Stored bool

	Resolvers
}

// Resolvers reach what a version is judged against beyond its own tree: the installation's stores,
// the repositories a workflow include names and the pusher's permissions. Each is optional, and
// one left out is a check not made, which is how agk validate, reaching none of the
// installation's stores, makes every check it can and none it cannot.
type Resolvers struct {
	// Pin answers the digest an image named by a tag is pinned to, name@sha256:<hex> of the
	// tag's own repository, for the step that first names it. ErrNotHeld where none is:
	// "a server runs every image by the digest its tag was pinned to, and the hook reaches no
	// registry to resolve one". Left out, an image keeps the tag it was written with.
	Pin func(ctx context.Context, reference string, step agk.Step) (string, error)

	// Manifest answers the brick manifest recorded for an image, as its document, for the
	// step that first runs it as a brick. ErrNotHeld where none is, which refuses the step:
	// reading one out of the image would be pulling it. Left out, no step is held to a
	// manifest and no graph is built.
	Manifest func(ctx context.Context, image string, step agk.Step) ([]byte, error)

	// Include answers the tree of the repository a workflow include names at the commit its
	// ref resolves to, and that commit, whole, having checked the pusher's workflow:read on
	// it. Left out, a workflow include is refused.
	Include func(ctx context.Context, ref graph.WorkflowRef) (fs.FS, string, error)

	// Secrets answers the names of the secrets the namespace declares. Left out, the names a
	// workflow writes are not held to the namespace's.
	Secrets func(ctx context.Context) ([]string, error)

	// SecretUse answers whether the pusher holds secret:use, asked only of a version naming a
	// secret: "whoever writes a secret's name into a workflow answers for its value going into
	// a container". Left out, it is not asked.
	SecretUse func(ctx context.Context, named []string) (bool, error)
}

// ErrNotHeld is what a resolver answers for something its store does not hold.
var ErrNotHeld = errors.New("version: not held")

// Checked is a tree Check accepted: the workflow as it resolved, the graph where manifests were
// reached, and the version it would be made of.
type Checked struct {
	Workflow *graph.Workflow
	Graph    *graph.Graph
	Commit   string

	// Version is what a version holds to be rebuilt with nothing in reach: the entry point and
	// every file resolution read, the manifest of every image a brick step runs by the reference
	// the step wrote, and the digest each tag was pinned to. Where it was made and by whom is
	// the caller's.
	Version db.Version
}

// Resolved is the graph written down as wire.schema.json's resolvedGraph, what a hook records
// for the commit and what the API answers for it.
func (c *Checked) Resolved() ([]byte, error) {
	if c.Graph == nil {
		return nil, fmt.Errorf("version: no manifest was reached, so no graph was built to write down")
	}
	return c.Graph.Resolved(c.Commit)
}

// SecretsNotUsable is a version naming secrets its pusher does not hold secret:use for, which is
// a permission the pusher lacks rather than a rule the file breaks.
type SecretsNotUsable struct {
	Named []string
}

func (e *SecretsNotUsable) Error() string {
	noun := "the secret"
	if len(e.Named) > 1 {
		noun = "the secrets"
	}
	return fmt.Sprintf("this version names %s %s, and a version naming a secret is accepted only from someone holding secret:use: whoever writes a secret's name into a workflow answers for its value going into a container", noun, strings.Join(e.Named, ", "))
}

// reaching is Include as graph.Remote, under the context of the check.
type reaching struct {
	ctx     context.Context
	include func(context.Context, graph.WorkflowRef) (fs.FS, string, error)
}

func (r reaching) Include(ref graph.WorkflowRef) (fs.FS, string, error) {
	return r.include(r.ctx, ref)
}

// underItsName holds the file's metadata to where the push is made.
func underItsName(wf *graph.Workflow, c Checking) error {
	if c.Repository != "" && wf.Metadata.Name != c.Repository {
		return &graph.Refusal{Rule: RuleMetadataNameNotRepository, At: wf.MetadataAt("name"), Detail: fmt.Sprintf(
			"metadata.name is %s and the repository is %s: the name is the identity runs, grants and the git remote are addressed by, and the repository already holds one", wf.Metadata.Name, c.Repository)}
	}
	if c.Namespace != "" && wf.Metadata.Namespace != "" && wf.Metadata.Namespace != c.Namespace {
		return &graph.Refusal{Rule: RuleMetadataNamespaceNotRepository, At: wf.MetadataAt("namespace"), Detail: fmt.Sprintf(
			"metadata.namespace is %s and the repository belongs to %s: a workflow reaches the secrets, the quotas and the runner pools of the namespace that owns it, and the file cannot move it to another", wf.Metadata.Namespace, c.Namespace)}
	}
	return nil
}

// pinned puts the digest each tag was pinned to in the place of the tag, so that the graph a run
// is decided from names every image by digest, and answers the tags it pinned.
//
// "Images by digest in production: a tag is a mutable pointer, and a commit must determine what
// ran." A task carries the image its step names, and the wire's imageRef is name@sha256:<hex>, so
// a tag left in place would become a message no runner may take. An image the workflow names by
// digest is kept as written. A hidden block is never executed, so an image it names needs no pin
// until a step extends it.
func pinned(ctx context.Context, wf *graph.Workflow, c Checking) (map[string]string, error) {
	var pins map[string]string
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		switch {
		case st.Image == "" || agk.ImageByDigest(st.Image):
			continue
		case strings.Contains(st.Image, "@"):
			return nil, fmt.Errorf("%s: step %s names %s, and a digest is written sha256: and sixty-four lowercase hexadecimal characters", wf.StepAt(name, "image"), name, st.Image)
		case c.Pin == nil:
			continue
		}
		digest, held := pins[st.Image]
		if !held {
			var err error
			digest, err = c.Pin(ctx, st.Image, name)
			if errors.Is(err, ErrNotHeld) {
				return nil, &graph.Refusal{Step: name, Rule: RuleImageNotPinned, At: wf.StepAt(name, "image"), Detail: fmt.Sprintf(
					"the step names %s by a tag, and its repository holds no digest for it: a server runs every image by the digest its tag was pinned to and reaches no registry to resolve one, so a tag is pinned by agk push, which resolves it where the image is", st.Image)}
			}
			if err != nil {
				return nil, err
			}
			repository, _, _ := strings.Cut(digest, "@")
			if !agk.ImageByDigest(digest) || repository != agk.ImageRepository(st.Image) {
				return nil, fmt.Errorf("version: %s is pinned to %q, and a tag is pinned to its own repository, %s, at a sha256 digest", st.Image, digest, agk.ImageRepository(st.Image))
			}
			if pins == nil {
				pins = map[string]string{}
			}
			pins[st.Image] = digest
		}
		st.Image = digest
		wf.Steps[name] = st
	}
	return pins, nil
}

// manifestsOf reads the manifest of every image a brick step runs, once per image, for the first
// step in name order that runs it. An image with none recorded is left out, and graph.Build
// refuses the step naming it, where its image is written.
func manifestsOf(ctx context.Context, wf *graph.Workflow, c Checking) (map[string]brick.Manifest, error) {
	parse := brick.ParseManifest
	if c.Stored {
		parse = brick.ParseStoredManifest
	}
	first := map[string]agk.Step{}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		if st := wf.Steps[name]; st.Image != "" && len(st.Script) == 0 {
			if _, ok := first[st.Image]; !ok {
				first[st.Image] = name
			}
		}
	}
	read := map[string]brick.Manifest{}
	for _, image := range graph.Images(wf) {
		body, err := c.Manifest(ctx, image, first[image])
		if errors.Is(err, ErrNotHeld) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m, err := parse(body)
		if err != nil {
			return nil, fmt.Errorf("version: the manifest of %s: %w", image, err)
		}
		read[image] = m
	}
	return read, nil
}

// secretsOf holds the secrets a workflow names to the pusher's secret:use, then to the
// namespace's declarations, in that order: "asked after secret:use, so that only a caller allowed
// to write a secret's name into a workflow learns whether the namespace declares it".
func secretsOf(ctx context.Context, wf *graph.Workflow, c Checking) error {
	named := SecretsNamed(wf)
	if len(named) == 0 {
		return nil
	}
	if c.SecretUse != nil {
		held, err := c.SecretUse(ctx, named)
		if err != nil {
			return err
		}
		if !held {
			return &SecretsNotUsable{Named: named}
		}
	}
	if c.Stored || c.Secrets == nil {
		return nil
	}
	names, err := c.Secrets(ctx)
	if err != nil {
		return err
	}
	declared := make(map[string]bool, len(names))
	for _, name := range names {
		declared[name] = true
	}
	var missing []string
	for _, secret := range named {
		if !declared[secret] {
			missing = append(missing, secret)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &graph.Refusal{Rule: RuleSecretNotDeclaredByNamespace, At: wf.SecretAt(missing[0]), Detail: undeclared(wf, missing, c.Namespace)}
}

// SecretsNamed are the secrets a workflow names, sorted: its secrets block, and every one a step
// mounts. A step may mount only what the block names, which loading the workflow holds, so the
// steps add nothing today; they are read all the same, since this is what a push is authorised
// by and a secret it missed would be one nobody answered for.
func SecretsNamed(wf *graph.Workflow) []string {
	named := slices.Clone(wf.Secrets)
	for _, st := range wf.Steps {
		named = append(named, st.Secrets...)
	}
	slices.Sort(named)
	return slices.Compact(named)
}

// undeclared says which of the secrets a workflow names its namespace does not declare, and where
// each is named: by the steps that mount it, or by the secrets block alone where no step does.
func undeclared(wf *graph.Workflow, missing []string, namespace string) string {
	var where []string
	for _, secret := range missing {
		var steps []string
		for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
			if slices.Contains(wf.Steps[name].Secrets, secret) {
				steps = append(steps, string(name))
			}
		}
		switch len(steps) {
		case 0:
			where = append(where, "the secrets block names the secret "+secret)
		case 1:
			where = append(where, "step "+steps[0]+" names the secret "+secret)
		default:
			where = append(where, "steps "+strings.Join(steps[:len(steps)-1], ", ")+" and "+steps[len(steps)-1]+" name the secret "+secret)
		}
	}
	ns := "its namespace"
	if namespace != "" {
		ns = "the namespace " + namespace
	}
	if len(missing) == 1 {
		return fmt.Sprintf("%s, which %s does not declare: a workflow can name only the secrets its own namespace declares, so that moving it elsewhere breaks the reference rather than carrying access along, and every run of this version would be refused the secret at its first redemption. Declare it with PUT /api/v1/%s/secrets/%s, or push a version that does not name it", where[0], ns, orPlaceholder(namespace), missing[0])
	}
	return fmt.Sprintf("%s, none of which %s declares: a workflow can name only the secrets its own namespace declares, so that moving it elsewhere breaks the reference rather than carrying access along, and every run of this version would be refused them at their first redemption. Declare each with PUT /api/v1/%s/secrets/{name}, or push a version that does not name them", strings.Join(where, ", and "), ns, orPlaceholder(namespace))
}

// orPlaceholder is a namespace as a route writes it, or the route's own placeholder where the
// namespace is not known.
func orPlaceholder(namespace string) string {
	if namespace == "" {
		return "{namespace}"
	}
	return namespace
}
