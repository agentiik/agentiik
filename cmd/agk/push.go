package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// agk push: "Registers the workflow in a namespace on a server."
//
// What it sends is what the server stores: the entry point, every file it includes, the manifest
// of every image it names, the digest each image it names by tag resolves to, and the tree every
// step sees under /agk/repo. The server rebuilds it before writing it, so a push that would not
// come back is refused in front of the person pushing rather than at the first run.
//
// # Why everything is read out of the commit
//
// "A version is a commit. finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently,
// because that is what a commit already is." So every byte a push sends is read out of git's
// objects for that commit, and none of it off the disk: the entry point the graph is rebuilt from,
// the files it includes and the tree a container is given are one set of bytes, and a version
// cannot say it is one tree and be another. The bytes of the working copy under the name of a
// commit whose tree differs would be exactly that, for ever, and nothing downstream could ever
// notice: the digests match what was pushed.
//
// # Why a dirty tree is refused all the same
//
// An edit that was never committed is therefore never pushed. A working copy holding one is
// refused anyway, because somebody pushing it most likely believes the edit goes with the push,
// and finding out otherwise at the first run is the surprise this saves them. --allow-dirty says
// the edit is meant to stay behind: the commit is pushed as it was committed, and nothing else.

const (
	// tokenVariable is where a script's credential comes from, and where it is not set agk
	// presents the token agk login kept (profile.go). Never a flag: an argument is in the shell
	// history, in the process list and in whatever recorded the terminal.
	tokenVariable = "AGENTIIK_TOKEN"

	// serverVariable is the installation, so that a repository does not carry one and a
	// person working against two does not edit a file between pushes.
	serverVariable = "AGENTIIK_SERVER"

	// serverDefault is what --server defaults to, wherever a verb takes it (installationOf).
	serverDefault = "Defaults to " + serverVariable + ", then the installation agk login last signed in to."
)

func push(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk push", "agk push [-f <path>] --namespace <namespace> [--server <url>] [--commit <commit>] [--branch <branch>] [--allow-dirty]")
	entry := fs.String("f", "", "The entry point to push. Defaults to "+entryPoint+" in the directory the command is run in.")
	namespace := fs.String("namespace", "", "The namespace to register the workflow in.")
	server := fs.String("server", "", "The installation to push to. "+serverDefault)
	commit := fs.String("commit", "", "The commit to push: a hash, a branch or a tag the repository holds. Defaults to HEAD, the branch checked out.")
	dirty := fs.Bool("allow-dirty", false, "Push although the working tree has uncommitted changes. The commit is pushed as it was committed either way, so this says the changes are meant to stay behind.")
	branch := fs.String("branch", "", "The branch the push leaves at the commit. Defaults to the branch --commit names, or the branch checked out.")
	if code, ok := parse(fs, args); !ok {
		return code
	}

	if *namespace == "" {
		fmt.Fprintln(e.Err, "--namespace is required: a workflow belongs to exactly one namespace")
		return exitUsage
	}
	at, ok := reach(e, *server)
	if !ok {
		return exitUsage
	}
	where := at.base

	path, err := entryOf(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	repo, err := repositoryAt(ctx, path)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	sha, err := commitOf(ctx, repo.top, *commit)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	// Before the working copy is asked about and before any of the tree is read, because a -f
	// that names nothing, and one that names an entry point anywhere but at the root, are the
	// mistakes here that neither committing nor --allow-dirty puts right. "The entry point is
	// always at the root", and a workflow kept in a directory of a larger repository is pushed
	// as a repository of its own.
	if err := repo.holds(ctx, sha, path); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if err := atTheRoot(repo, path); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if !*dirty {
		if changed, err := dirtyTree(ctx, repo.top); err != nil {
			fmt.Fprintf(e.Err, "%s\n", err)
			return exitRefused
		} else if len(changed) > 0 {
			fmt.Fprintf(e.Err, "the working tree has uncommitted changes in %s, and what is pushed is %s as it was committed, without them: commit them, or pass --allow-dirty to push %s and leave them behind\n",
				counted(len(changed), "file", "files"), short(sha), short(sha))
			for _, f := range changed {
				fmt.Fprintf(e.Err, "  %s\n", f)
			}
			return exitRefused
		}
	}

	// The ref the push moves, settled before the tree is read, since a push that names none is
	// refused whatever the tree holds.
	ref, object, err := pushRef(ctx, repo.top, *commit, *branch, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// The tree as git lists it, each entry held to the rules a version's tree is held to, and
	// each file read out of git when the validation reads it.
	tree, err := repositoryOf(ctx, repo, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// The workflow is judged here by the one validation the installation's hook judges it by,
	// reaching what this machine reaches: each tag is resolved to its digest through the local
	// daemon, and the manifests are read out of those digests, so that a tag moved on this
	// machine between the two cannot pair the manifest of one image with the digest of another.
	// A version the hook will refuse is refused here, before anything is recorded or sent.
	local := &daemon{e: e}
	defer local.close()
	checked, err := versions.Check(ctx, tree, versions.Checking{
		Commit: sha, Committed: true, Namespace: *namespace,
		Resolvers: versions.Resolvers{Pin: local.pin, Manifest: local.manifest},
	})
	if err != nil {
		refusal(e.Err, err)
		return leaving(err)
	}
	wf, images := checked.Workflow, checked.Version.Images
	name := string(wf.Metadata.Name)

	// Then, in order, what a git push needs of the installation and does not carry: the
	// repository, created where it is not, and the digest of each tag and the manifest of
	// each brick image, recorded in it, which is what its hook judges the push against.
	// The default branch of a repository created here is the branch the push moves, so that
	// its first push is to the branch a clone checks out and a run naming no ref runs; a push
	// of a tag takes the branch checked out, or main.
	defaultBranch, pushesBranch := strings.CutPrefix(ref, "refs/heads/")
	if !pushesBranch {
		if defaultBranch = branchOf(ctx, repo.top); defaultBranch == "" {
			defaultBranch = "main"
		}
	}
	for _, step := range []func() error{
		func() error { return createRepository(ctx, at, *namespace, name, defaultBranch) },
		func() error { return recordImages(ctx, at, *namespace, name, images, local.texts) },
	} {
		if err := step(); err != nil {
			fmt.Fprintf(e.Err, "%s\n", err)
			if errors.Is(err, errUnreachable) {
				return exitNoOutcome
			}
			return exitRefused
		}
	}

	// And the push: the ref as the installation holds it, the objects it lacks, and its
	// answer.
	repository := "/" + *namespace + "/" + name + ".git"
	refs, err := remoteRefs(ctx, at, repository)
	if err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return leavingRemote(err)
	}
	if refs[ref] == object {
		fmt.Fprintf(e.Out, "%s/%s %s is at %s already, on %s\n", *namespace, name, ref, short(sha), where)
		keptDigests(e, *namespace, name, sha, images)
		return exitSucceeded
	}
	tips := make([]string, 0, len(refs))
	already := false
	for _, id := range refs {
		tips = append(tips, id)
		already = already || id == sha
	}
	have, err := heldHere(ctx, repo.top, tips)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	// A move git would make only with --force is not made: see aheadOf.
	if old, held := refs[ref]; held {
		if err := aheadOf(ctx, repo.top, ref, old, object, sha); err != nil {
			refusal(e.Err, err)
			return exitRefused
		}
	}
	if below, err := shallowBelow(ctx, repo.top, sha, have); err != nil {
		refusal(e.Err, err)
		return exitRefused
	} else if below != "" {
		refusal(e.Err, fmt.Errorf("this clone is shallow, and the installation lacks the history below %s, which a version's commit reaches and this clone does not hold: git fetch --unshallow, and push again", short(below)))
		return exitRefused
	}
	// Whether the commit is a version already, pushed under another ref or under one since
	// moved on, for what keptDigests says; asked only where the workflow names a tag.
	if !already && len(images) > 0 {
		already = isVersion(ctx, at, *namespace, name, sha)
	}
	result, err := sendPack(ctx, e, at, repo.top, repository, ref, refs[ref], object, have)
	if err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return leavingRemote(err)
	}
	if !result.accepted {
		fmt.Fprintf(e.Err, "the installation refused %s at %s: %s\n", ref, short(sha), result.why)
		return exitRefused
	}

	fmt.Fprintf(e.Out, "%s/%s@%s pushed to %s as %s\n", *namespace, name, short(sha), where, ref)
	if already {
		keptDigests(e, *namespace, name, sha, images)
	}
	fmt.Fprintf(e.Out, "%s, %s, %s, %s, %s\n",
		counted(len(wf.Steps), "step", "steps"),
		counted(len(tree.files), "file", "files"),
		counted(len(checked.Version.Includes), "included file", "included files"),
		counted(len(local.texts), "manifest", "manifests"),
		counted(len(images), "tag resolved to its digest", "tags resolved to their digests"))
	return exitSucceeded
}

// keptDigests says that a commit pushed before keeps the images its first push resolved: a version
// is a commit, and its first push settled its images, so a tag moved since was recorded for the next
// commit and no run of this one names it. Said, because the lines before it say each tag was
// resolved, and somebody pushing again to pick up an image they fixed under the same tag would
// otherwise believe it was picked up.
func keptDigests(e Env, namespace, name, sha string, images map[string]string) {
	if len(images) == 0 {
		return
	}
	fmt.Fprintf(e.Err, "%s/%s@%s was already pushed, and every run of it keeps the digests its first push resolved %s to, whatever they name now: running what a tag names now takes a new commit\n",
		namespace, name, short(sha), strings.Join(slices.Sorted(maps.Keys(images)), ", "))
}

// leavingRemote is how a push the installation did not answer, or refused, leaves.
func leavingRemote(err error) int {
	if errors.Is(err, errUnreachable) {
		return exitNoOutcome
	}
	return exitRefused
}

// atTheRoot refuses an entry point anywhere but agentiik.yaml at the top of the repository: "the
// entry point is always at the root", spelled so. What agk push pushes is the repository, and a
// hook reads its root. A -f naming billing/agentiik.yaml pushed the tree of billing/ until
// v0.4.0, which is how a version pushed that way was made, and it stays runnable; a new one is
// pushed from a repository of its own, which the refusal names the command for.
func atTheRoot(repo place, path string) error {
	name := repo.prefix + filepath.Base(path)
	switch {
	case repo.prefix != "" && filepath.Base(path) == entryPoint:
		return versions.EntryPointBelowRoot(name)
	case name != entryPoint:
		return fmt.Errorf("-f names %s, and the entry point of a repository is %s at its root, spelled so: agk push pushes the repository, whose root %s is what the installation reads", name, entryPoint, entryPoint)
	}
	return nil
}

// entryOf is where the entry point is on the disk.
//
// The working copy is consulted for where and never for what, since every byte is read out of the
// commit, and the entry point need not be on the disk at all: --commit can name a commit from
// before its directory was removed. So the one mistake of -f said here is a directory, which is
// the commonest way -f is mistyped, in the words load says it in. Whether there is anything at
// the path is the commit's to say, and place.holds says it.
func entryOf(e Env, entry string) (string, error) {
	if entry == "" {
		entry = entryPoint
	}
	path := e.path(entry)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory: -f names the entry point itself, which is %s inside it", path, entryPoint)
	}
	return path, nil
}

// place is where an entry point is committed: the git repository, and the directory inside it.
type place struct {
	// top is the top of the working copy, and where every git command a push runs is run.
	// Not the entry point's own directory, which a later commit may have removed, and git
	// cannot be run in a directory that is not there.
	top string

	// prefix is the entry point's directory inside the repository, the way git writes one:
	// empty at the top, and billing/ below it. The tree a version carries is rooted there,
	// because that is the root load gives every include and every schema reference.
	prefix string
}

// repositoryAt is the repository the entry point at path is committed to.
//
// Outside a git repository there is no commit, and so nothing to push. Walking the directory
// instead would send whatever it holds under a hash nobody can check it against, which is the one
// lie this command exists to refuse, and would leave it guessing what a repository is made of: a
// virtual environment, a build, the .agk a local run leaves behind.
//
// That is said only where git says it, though. Anything else git refuses a repository over, one
// owned by somebody else, a configuration it cannot parse, a HEAD it cannot follow, is passed on
// in git's words: they name the cause and usually the remedy, and telling somebody standing in a
// repository that there is none sends them looking for the wrong thing.
//
// Git is asked about the nearest directory that exists, and the names missing below it are added
// to the prefix it answers, so that a directory gone from the working copy can still be pushed
// from a commit that holds it.
func repositoryAt(ctx context.Context, path string) (place, error) {
	dir := filepath.Dir(path)
	at, below := dir, []string(nil)
	for {
		if info, err := os.Stat(at); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(at)
		if parent == at {
			break
		}
		below = append([]string{filepath.Base(at)}, below...)
		at = parent
	}

	prefix, err := gitLine(ctx, at, "rev-parse", "--show-prefix")
	if err != nil {
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return place{}, errors.New("git is not installed, and agk push reads what it sends out of a git commit")
		case !notARepository(err):
			return place{}, fmt.Errorf("the repository at %s could not be read from git: %w", at, err)
		case len(below) > 0:
			// Neither a directory nor a repository around where it would be: this is a
			// mistyped -f, and saying it in load's words is saying so.
			return place{}, fmt.Errorf("there is no workflow at %s: one is read from %s in the directory the command is run in, or from the path -f names", path, entryPoint)
		}
		return place{}, fmt.Errorf("%s is not in a git repository, and a version is a commit: agk push reads what it sends out of one, so it runs inside the repository the workflow is committed to", dir)
	}
	top, err := gitLine(ctx, at, "rev-parse", "--show-toplevel")
	if err != nil {
		return place{}, fmt.Errorf("the repository at %s could not be read from git: %w", at, err)
	}
	if len(below) > 0 {
		prefix += strings.Join(below, "/") + "/"
	}
	return place{top: top, prefix: prefix}, nil
}

// holds refuses an entry point the commit does not hold, before any of the tree is read, and says
// which of two mistakes it is. A file on the disk that was never committed is the first, and
// committing it is the remedy. Nothing at the path at all is the second: a mistyped -f, said in
// load's words, since telling somebody to commit a file that does not exist sends them looking
// for it.
func (r place) holds(ctx context.Context, sha, path string) error {
	name := r.prefix + filepath.Base(path)
	// Resolving the path reads the commit's trees and none of its files.
	if _, err := git(ctx, r.top, "rev-parse", "--verify", "--quiet", sha+":"+name); err == nil {
		return nil
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("%s holds no %s: what is pushed is the commit, so the entry point has to be committed", short(sha), name)
	}
	return fmt.Errorf("there is no workflow at %s: one is read from %s in the directory the command is run in, or from the path -f names", path, entryPoint)
}

// repositoryOf is the tree of one commit as a container will see it, read out of git's objects.
//
// The commit rather than the working copy, for the reason this file opens with. What the commit
// tracks is also what leaves out an editor's leftovers, a build and a virtual environment: an
// ignored file is ignored because somebody said it is not part of the repository. The tree is
// the one at the entry point's directory inside the commit, which git lists by that name from the
// top of the repository.
//
// A file carries one of git's two modes, 0644 or 0755, and says which even where it is the
// ordinary one, so that nothing downstream has to guess what an absent mode meant. Two other kinds
// of entry are refused rather than carried. A symbolic link is resolved on whatever host lays the
// tree out, and nothing stops its target being outside the repository once it is there. A
// submodule is another repository, which a runner would need a credential to fetch, and a runner
// holds none.
//
// The files are counted out of the listing git makes from object headers, and a tree of more than
// a version holds is refused having read none of them. None is read here at all: the tree answered
// reads a file out of git when it is first read, so that the validation, which reads the entry
// point, what it includes and the schemas its inputs name, reads those and no others, and a tree
// of large files is not read into memory to judge a workflow of a few kilobytes.
//
// That holds in a partial clone only because git is told to fetch nothing. A clone that filtered
// blobs out fetches one the moment anything asks about it, with a request of its own, so listing
// the sizes would be a round trip per file, and the pack a push writes would fetch every one of
// them before its first byte. A blob the clone lacks is refused by name instead, with a way to
// fetch them all at once.
func repositoryOf(ctx context.Context, repo place, sha string) (*gitTree, error) {
	// sha: is the commit's root, and sha:billing the tree at billing/ inside it. Either is
	// listed with paths relative to itself, because git runs at the top, where a listing is
	// not narrowed to the directory it runs in.
	listed, err := output(fetchingNothing(gitCommand(ctx, repo.top, "ls-tree", "-r", "-z", "-l", sha+":"+strings.TrimSuffix(repo.prefix, "/"))))
	if err != nil {
		return nil, fmt.Errorf("the tree of %s could not be read from git: %w", short(sha), err)
	}

	type entry struct {
		path, mode, object string
		size               int64
	}
	var entries []entry
	var missing []string
	for _, record := range strings.Split(string(listed), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> <type> <object> <size>", a tab, and the path exactly as it was committed:
		// -z quotes nothing, and a name may begin or end with a space.
		meta, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 4 {
			return nil, fmt.Errorf("the tree of %s could not be read from git: %q is not a line of its listing", short(sha), record)
		}
		// Every rule a tree is held to, by the code the installation and a hook hold it by
		// rather than a copy of it, so that an entry they would refuse is refused here, before
		// any of the tree is read, rather than there, after all of it was read and sent.
		var mode string
		var kind fs.FileMode
		switch fields[0] {
		case "100644":
			mode = "0644"
		case "100755":
			mode = "0755"
		case "120000":
			kind = fs.ModeSymlink
		case "160000":
			kind = fs.ModeIrregular
		default:
			return nil, fmt.Errorf("%s is committed with mode %s, and a tree carries a file as 100644 or 100755 and nothing else", path, fields[0])
		}
		if err := versions.TreeEntry(path, kind); err != nil {
			return nil, err
		}
		if strings.ContainsRune(path, utf8.RuneError) {
			// Valid UTF-8, and refused by the installation all the same: U+FFFD is what JSON
			// leaves where a name was not, and one really in a name looks exactly like that.
			return nil, fmt.Errorf("%s holds U+FFFD, which the installation cannot tell apart from what JSON leaves in a name that was not UTF-8: rename the file, commit the rename, then push", path)
		}

		if fields[3] == "BAD" {
			// What ls-tree writes for a size it could not read, which with fetching turned
			// off is a blob this clone does not hold.
			missing = append(missing, path)
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("the tree of %s could not be read from git: %q is not a line of its listing", short(sha), record)
		}
		entries = append(entries, entry{path: path, mode: mode, object: fields[2], size: size})
	}
	if files := len(entries) + len(missing); files > api.TreeMaxFiles {
		return nil, fmt.Errorf("the tree of %s has %d files, and a version holds at most %d, as the installation holds it to: every task of a version is sent every file's URL, and a tree of this many is usually carrying dependencies that belong in an image", short(sha), files, api.TreeMaxFiles)
	}
	if len(missing) > 0 {
		held := missing[0]
		if len(missing) > 1 {
			held += " and " + counted(len(missing)-1, "other file", "other files")
		}
		return nil, fmt.Errorf("the tree of %s holds %s, which this clone lacks: a partial clone fetches a file it lacks with a request of its own, one file at a time, and every one of them before a push could be written. Fetch them first, with git backfill or by checking %s out, then push", short(sha), held, short(sha))
	}

	tree := &gitTree{ctx: ctx, top: repo.top, files: map[string]gitBlob{}, dirs: map[string][]fs.DirEntry{".": nil}}
	for _, f := range entries {
		mode := fs.FileMode(0o444)
		if f.mode == "0755" {
			mode = 0o555
		}
		tree.add(f.path, gitBlob{object: f.object, mode: mode, size: f.size})
	}
	return tree, nil
}

// loadCommitted is load, reading a commit rather than the disk, for agk run on an installation: the
// same graph.Check, over the tree that travels, read with graph.LoadStored, since the commit it
// names is one the installation already holds as it was pushed, which may be before a rule added
// since, and the installation starts it all the same. The command reads it only for the name the
// version was pushed under, so it is not judged as a version about to be made, which is agk
// push's and version.Check's.
func loadCommitted(tree fs.FS, base, dir, sha string) (*graph.Workflow, error) {
	// place.holds has refused a commit with nothing at the path, so what is left to say here is
	// the commit holding a directory there.
	if info, err := fs.Stat(tree, base); err == nil && info.IsDir() {
		return nil, fmt.Errorf("%s is a directory in %s: -f names the entry point itself, which is %s inside it", base, short(sha), entryPoint)
	}
	wf, err := graph.LoadStored(tree, base, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w. The tree read was %s at %s", err, dir, short(sha))
	}
	if err != nil {
		return nil, err
	}
	if err := graph.Check(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// commitOf is the commit a push names, as the whole hash git holds it under.
//
// Resolved rather than taken as typed, so that a3f9c1e, the branch it is the tip of and HEAD push
// one version rather than three, and so that a name this repository does not hold is refused
// before anything is read. A name beginning with a dash is refused before git sees it, since git
// would take it for an option of its own.
func commitOf(ctx context.Context, top, named string) (string, error) {
	switch {
	case named == "":
		named = "HEAD"
	case strings.HasPrefix(named, "-"):
		return "", fmt.Errorf("--commit %s names no commit: a commit is a hash, a branch or a tag", named)
	}
	sha, err := git(ctx, top, "rev-parse", "--verify", "--quiet", named+"^{commit}")
	switch {
	case err == nil && sha != "" && len(sha) != 40:
		// A repository made with --object-format=sha256, which is what Git 3.0 makes by
		// default. The installation records a version under the forty characters of a SHA-1
		// commit and would refuse this one, after every file of the tree had been read and
		// sent; the length of the hash says so before any of it is read.
		return "", fmt.Errorf("the repository at %s names its commits by SHA-256, %s being %d characters, and an installation records a version under the forty characters of a SHA-1 commit: push from a repository that uses SHA-1", top, short(sha), len(sha))
	case err == nil && sha != "":
		return sha, nil
	case named == "HEAD":
		return "", fmt.Errorf("the repository at %s has no commit yet, and a version is a commit: commit the workflow, then push it", top)
	}
	return "", fmt.Errorf("the repository at %s holds no commit %s", top, named)
}

// notARepository is whether git refused because it found no repository at all, which it says in
// the two sentences its discovery dies with: "not a git repository (or any of the parent
// directories)" and "not a git repository (or any parent up to mount point". Its other "not a git
// repository", which names a path, is about a .git file or a GIT_DIR pointing at something broken,
// and that is a repository git could not read rather than the absence of one.
//
// A git that speaks another language says neither, and what it said is passed on instead, which is
// still the truth in its own words.
func notARepository(err error) bool {
	return strings.Contains(err.Error(), "not a git repository (or any")
}

// dirtyTree is what the working copy holds that the commit does not: the edits somebody pushing
// may believe go with the push, and which do not.
func dirtyTree(ctx context.Context, dir string) ([]string, error) {
	out, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("the working tree could not be read from git: %w", err)
	}
	var changed []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			changed = append(changed, strings.TrimSpace(line))
		}
	}
	return changed, nil
}

// branchOf is the branch the push came from, and is empty where git cannot say: it is what the
// workflow's default branch is set to on the first push and is not worth failing over.
func branchOf(ctx context.Context, dir string) string {
	out, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "HEAD" {
		return ""
	}
	return out
}

// gitCommand is git run in dir, reading what is committed and nothing a local setting would put in
// its place.
//
// GIT_OPTIONAL_LOCKS=0 because a push only reads, and git status otherwise refreshes the index
// behind the back of whatever else has the repository open. GIT_NO_REPLACE_OBJECTS=1 because a
// replace ref lives in this clone alone: honouring one would push, under the commit's name, a tree
// no other clone of that commit holds.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}

// fetchingNothing is a git command that answers out of what this clone holds and never fetches
// what it lacks, which is what repositoryOf needs of a partial clone and says why.
// GIT_NO_LAZY_FETCH is read by git from 2.45 on. An older git ignores it and fetches as it always
// has, which is slow and still reads the right bytes.
func fetchingNothing(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	return cmd
}

// gitOutput is what git wrote, exactly as it wrote it. A -z listing is read through this rather
// than through git, since a name may begin or end with a space and trimming it is renaming it.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return output(gitCommand(ctx, dir, args...))
}

// output runs a git command and answers what it wrote, or what it said where it failed.
func output(cmd *exec.Cmd) ([]byte, error) {
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(errs.String()); said != "" {
			return nil, fmt.Errorf("%s", said)
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitLine is the one line git answered, less the newline that ends it and nothing else, for an
// answer that is a path: a directory's name may begin or end with a space as a file's may.
func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// short is a commit as a person writes it.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
