package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/brick"
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
	// tokenVariable is where the credential comes from. Never a flag: an argument is in the
	// shell history, in the process list and in whatever recorded the terminal.
	tokenVariable = "AGENTIIK_TOKEN"

	// serverVariable is the installation, so that a repository does not carry one and a
	// person working against two does not edit a file between pushes.
	serverVariable = "AGENTIIK_SERVER"
)

func push(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk push", "agk push [-f <path>] --namespace <namespace> [--server <url>] [--commit <commit>] [--allow-dirty]")
	entry := fs.String("f", "", "The entry point to push. Defaults to "+entryPoint+" in the directory the command is run in.")
	namespace := fs.String("namespace", "", "The namespace to register the workflow in.")
	server := fs.String("server", "", "The installation to push to. Defaults to "+serverVariable+".")
	commit := fs.String("commit", "", "The commit to push: a hash, a branch or a tag the repository holds. Defaults to HEAD.")
	dirty := fs.Bool("allow-dirty", false, "Push although the working tree has uncommitted changes. The commit is pushed as it was committed either way, so this says the changes are meant to stay behind.")
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
	where, token := at.base, at.token

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
	// that names nothing is the one mistake here that neither committing nor --allow-dirty
	// puts right.
	if err := repo.holds(ctx, sha, path); err != nil {
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

	// The whole tree, because "every step of every run sees it, mounted read-only at
	// /agk/repo", and the installation holds no clone of the repository to read it out of.
	files, err := repositoryOf(ctx, repo, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// And the workflow is read out of those same bytes rather than off the disk, so that the
	// closure the version is rebuilt from and the tree a container is given cannot disagree
	// about any file both of them hold.
	tree := committed(files)
	base := filepath.Base(path)
	wf, err := loadCommitted(tree, base, filepath.Dir(path), sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if _, err := declaredInputs(wf, tree); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// Every tag is resolved to its digest, and the manifests are read the way validate reads
	// them, out of those digests: a version the server cannot build is a version it will
	// refuse, and finding that out here is cheaper.
	images, read, code := pinned(ctx, e, wf)
	if code != exitSucceeded {
		return code
	}

	// The entry point is named by its path inside the tree, because a version names a path in
	// a commit rather than on a disk.
	captured, err := versions.Capture(tree, base, read)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	body := api.Push{
		Entry: captured.Entry, Document: captured.Document,
		Includes: captured.Includes, Manifests: captured.Manifests,
		Images: images,
		Tree:   files,
		Branch: branchOf(ctx, repo.top),
	}
	name := string(wf.Metadata.Name)
	url := fmt.Sprintf("%s/api/v1/%s/workflows/%s/versions/%s",
		strings.TrimRight(where, "/"), *namespace, name, sha)
	pushed, err := put(ctx, url, token, body)
	if errors.Is(err, errAnswerUnread) {
		// Recorded, and what it records is what cannot be said, which is no outcome
		// rather than a refusal.
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitNoOutcome
	}
	if err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitRefused
	}

	fmt.Fprintf(e.Out, "%s/%s@%s pushed to %s\n", *namespace, name, short(sha), where)
	fmt.Fprintf(e.Out, "%s, %s, %s, %s, %s\n",
		counted(len(wf.Steps), "step", "steps"),
		counted(len(files), "file", "files"),
		counted(len(captured.Includes), "included file", "included files"),
		counted(len(captured.Manifests), "manifest", "manifests"),
		counted(len(images), "tag resolved to its digest", "tags resolved to their digests"))

	// A commit pushed before is the version its first push recorded, digests included, so a
	// tag that has moved since was resolved here to something no run of it will name. Said,
	// because the lines above say it was resolved, and somebody pushing again to pick up an
	// image they fixed under the same tag would otherwise believe it was picked up. A
	// version recorded naming a tag itself, before digests were kept, holds none for it.
	for _, tag := range slices.Sorted(maps.Keys(images)) {
		held, recorded := pushed.Images[tag]
		if held == images[tag] {
			continue
		}
		if !recorded {
			held = "written"
		}
		fmt.Fprintf(e.Err, "%s/%s@%s was already pushed, with %s as %s, and every run of it keeps that rather than %s: the first push of a commit settles its images, so running what the tag names now takes a new commit\n",
			*namespace, name, short(sha), tag, held, images[tag])
	}
	return exitSucceeded
}

// pinned is what the images of a version are pushed as: the digest each image the workflow names
// by tag was resolved to, and the manifest of every image a step is held to, by the reference as
// the workflow writes it.
//
// A tag is resolved here, on the machine that built or pulled the image, because "a tag is a
// mutable pointer, and a commit must determine what ran": the version records the digest once, so
// every run of it runs the same bytes, and the installation, which reaches no registry, never has
// one to resolve. A script step's base image is resolved too, since a runner pulls it by digest
// like any other. An image the workflow already names by digest is sent as written and nothing is
// asked about it, so a workflow of script steps pinned by hand pushes from a machine with no
// Docker at all.
//
// Each digest is resolved before the manifest is read, and the manifest is read out of it, so
// that a tag moved on this machine between the two cannot pair the manifest of one image with the
// digest of another.
func pinned(ctx context.Context, e Env, wf *graph.Workflow) (map[string]string, map[string]brick.Manifest, int) {
	tagged, err := byTag(wf)
	if err != nil {
		refusal(e.Err, err)
		return nil, nil, exitRefused
	}
	referenced := references(wf)
	if len(tagged) == 0 && len(referenced) == 0 {
		return nil, map[string]brick.Manifest{}, exitSucceeded
	}
	d, code := imageReader(e)
	if code != exitSucceeded {
		return nil, nil, code
	}
	defer d.Close()

	var images map[string]string
	for _, r := range tagged {
		pin, err := d.Pin(ctx, r.Step, r.Image)
		if err != nil {
			refusal(e.Err, err)
			return nil, nil, leaving(err)
		}
		if images == nil {
			images = map[string]string{}
		}
		images[r.Image] = pin
		fmt.Fprintf(e.Out, "%s resolved to %s\n", r.Image, pin)
	}

	byDigest := make([]reference, 0, len(referenced))
	for _, r := range referenced {
		if pin, held := images[r.Image]; held {
			r.Image = pin
		}
		byDigest = append(byDigest, r)
	}
	read, code := manifestsThrough(ctx, e, d, byDigest)
	if code != exitSucceeded {
		return nil, nil, code
	}
	manifests := make(map[string]brick.Manifest, len(referenced))
	for i, r := range referenced {
		manifests[r.Image] = read[byDigest[i].Image]
	}
	return images, manifests, exitSucceeded
}

// byTag are the images the workflow names by tag, each with the first step in name order that
// names it, script steps included. A reference that writes a digest the wire does not carry is
// refused, since a runner is handed nothing else.
func byTag(wf *graph.Workflow) ([]reference, error) {
	var tagged []reference
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		image := wf.Steps[name].Image
		switch {
		case image == "" || seen[image] || agk.ImageByDigest(image):
			continue
		case strings.Contains(image, "@"):
			return nil, fmt.Errorf("step %s names %s, and a digest is written sha256: and sixty-four lowercase hexadecimal characters, which is what a runner is handed", name, image)
		}
		seen[image] = true
		tagged = append(tagged, reference{Image: image, Step: name})
	}
	return tagged, nil
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
// Every size is added up before any content is read, out of the listing git makes from object
// headers, so that a tree above the limit is refused having read none of its files. The limit
// belongs to how the tree travels rather than to what a repository may be: until the installation
// serves the repository over git smart HTTP, which is v0.4.0, all of it rides inside one JSON
// request.
//
// That holds in a partial clone only because git is told to fetch nothing. A clone that filtered
// blobs out fetches one the moment anything asks about it, with a request of its own, so listing
// the sizes would be a round trip per file, every one of them made before the first size could be
// added up, and a tree far above the limit downloaded whole in order to be refused. A blob the
// clone lacks is refused by name instead, with a way to fetch them all at once.
func repositoryOf(ctx context.Context, repo place, sha string) (map[string]api.PushFile, error) {
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
	var total int64
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
		if !utf8.ValidString(path) {
			return nil, fmt.Errorf("%q is not a UTF-8 name, and a push carries every name as JSON text, where it would arrive as some other name", path)
		}
		if strings.ContainsRune(path, utf8.RuneError) {
			// Valid UTF-8, and refused by the installation all the same: U+FFFD is what JSON
			// leaves where a name was not, and one really in a name looks exactly like that.
			return nil, fmt.Errorf("%s holds U+FFFD, which the installation cannot tell apart from what JSON leaves in a name that was not UTF-8: rename the file, commit the rename, then push", path)
		}
		// And every other rule the installation holds a name to, by its own code rather than
		// a copy of it, so that a name it would refuse is refused here, before any of the
		// tree is read, rather than there, after all of it was read and sent.
		if err := api.CheckTreePath(path); err != nil {
			return nil, fmt.Errorf("the installation would refuse a name in the tree of %s: %w", short(sha), err)
		}

		var mode string
		switch fields[0] {
		case "100644":
			mode = "0644"
		case "100755":
			mode = "0755"
		case "120000":
			return nil, fmt.Errorf("%s is a symbolic link, and a tree carries none: its target would be resolved on whatever host lays the tree out, where it could point outside the repository. Commit the file it points to in its place", path)
		case "160000":
			return nil, fmt.Errorf("%s is a submodule, and a tree carries none: it is another repository, which a runner would need a credential to fetch, and a runner holds none. Commit its files into this repository, or put them in an image", path)
		default:
			return nil, fmt.Errorf("%s is committed with mode %s, and a tree carries a file as 100644 or 100755 and nothing else", path, fields[0])
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
		// Counted as the server counts it, the path with the bytes, so that what passes here is
		// never refused there after every file has been read and sent.
		total += int64(len(path)) + size
		entries = append(entries, entry{path: path, mode: mode, object: fields[2], size: size})
	}
	if files := len(entries) + len(missing); files > api.TreeMaxFiles {
		return nil, fmt.Errorf("the tree of %s has %d files, and a push carries at most %d until the installation hosts the repository itself: a tree of this many is usually carrying dependencies that belong in an image", short(sha), files, api.TreeMaxFiles)
	}
	if total > api.TreeMaxBytes {
		// Where blobs are missing, the sizes added up are a floor, and already too much.
		size := strconv.FormatInt(total, 10) + " bytes"
		if len(missing) > 0 {
			size = "at least " + size
		}
		return nil, fmt.Errorf("the tree of %s is %s with its paths, and a push carries a tree of at most %d bytes until the installation hosts the repository itself: something this size belongs in an image or in an artifact", short(sha), size, api.TreeMaxBytes)
	}
	if len(missing) > 0 {
		held := missing[0]
		if len(missing) > 1 {
			held += " and " + counted(len(missing)-1, "other file", "other files")
		}
		return nil, fmt.Errorf("the tree of %s holds %s, which this clone lacks: a partial clone fetches a file it lacks with a request of its own, one file at a time, and every one of them before the size of the tree can be checked. Fetch them first, with git backfill or by checking %s out, then push", short(sha), held, short(sha))
	}

	sizes := make(map[string]int64, len(entries))
	for _, f := range entries {
		sizes[f.object] = f.size
	}
	contents, err := contentsOf(ctx, repo.top, sizes)
	if err != nil {
		return nil, fmt.Errorf("the tree of %s could not be read from git: %w", short(sha), err)
	}

	files := make(map[string]api.PushFile, len(entries))
	for _, f := range entries {
		files[f.path] = api.PushFile{Content: contents[f.object], Mode: f.mode}
	}
	return files, nil
}

// contentsOf reads the bytes of every object named, of the size the listing gave it, through one
// git process rather than one per file: a tree of three hundred scripts is otherwise three hundred
// processes.
//
// An object is asked for by its hash and never as commit:path, because the batch protocol is one
// name per line and a path may hold a newline.
func contentsOf(ctx context.Context, dir string, sizes map[string]int64) (map[string][]byte, error) {
	if len(sizes) == 0 {
		return map[string][]byte{}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	objects := make([]string, 0, len(sizes))
	var asked strings.Builder
	for object := range sizes {
		objects = append(objects, object)
		asked.WriteString(object + "\n")
	}

	cmd := fetchingNothing(gitCommand(ctx, dir, "cat-file", "--batch"))
	cmd.Stdin = strings.NewReader(asked.String())
	var errs bytes.Buffer
	cmd.Stderr = &errs
	answers, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	out := make(map[string][]byte, len(objects))
	r := bufio.NewReader(answers)
	var failed error
	for _, object := range objects {
		content, err := oneObject(r, object, sizes[object])
		if err != nil {
			failed = err
			break
		}
		out[object] = content
	}
	if failed != nil {
		// Whatever git had left to say is not going to be read, and a process blocked
		// writing it would never exit for Wait to see.
		cancel()
	}
	err = cmd.Wait()
	said := strings.TrimSpace(errs.String())
	switch {
	case failed != nil && said != "" && (errors.Is(failed, io.EOF) || errors.Is(failed, io.ErrUnexpectedEOF)):
		// An answer that stops short is git dying, over a corrupt pack or an object gone
		// from under it, and why is what it said on the way out rather than where its
		// answer broke off.
		return nil, errors.New(said)
	case failed != nil:
		return nil, failed
	case err != nil:
		if said != "" {
			return nil, errors.New(said)
		}
		return nil, err
	}
	return out, nil
}

// oneObject reads one answer of git cat-file --batch: a header naming the object, its type and its
// size, the content, and a newline.
func oneObject(r *bufio.Reader, object string, size int64) ([]byte, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("object %s: %w", object, err)
	}
	if want := object + " blob " + strconv.FormatInt(size, 10) + "\n"; header != want {
		return nil, fmt.Errorf("git answered %q for object %s, where the listing gave a blob of %d bytes", strings.TrimSpace(header), object, size)
	}
	content := make([]byte, size+1)
	if _, err := io.ReadFull(r, content); err != nil {
		return nil, fmt.Errorf("object %s: %w", object, err)
	}
	if content[size] != '\n' {
		return nil, fmt.Errorf("git answered more than the %d bytes object %s holds", size, object)
	}
	return content[:size], nil
}

// committed is the tree as an fs.FS, which is what the loader, the schema compiler and Capture
// read: the bytes that travel as the tree, and no others.
func committed(files map[string]api.PushFile) fstest.MapFS {
	tree := make(fstest.MapFS, len(files))
	for path, f := range files {
		tree[path] = &fstest.MapFile{Data: f.Content, Mode: 0o444}
	}
	return tree
}

// loadCommitted is load, reading the commit rather than the disk: the same graph.Load and the same
// graph.Check, over the tree that travels.
func loadCommitted(tree fs.FS, base, dir, sha string) (*graph.Workflow, error) {
	// place.holds has refused a commit with nothing at the path, so what is left to say here is
	// the commit holding a directory there.
	if info, err := fs.Stat(tree, base); err == nil && info.IsDir() {
		return nil, fmt.Errorf("%s is a directory in %s: -f names the entry point itself, which is %s inside it", base, short(sha), entryPoint)
	}
	wf, err := graph.Load(tree, base, nil)
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

// errAnswerUnread is a version the installation recorded and whose answer could not be read, so
// what it records, which is not always what was pushed, cannot be said.
var errAnswerUnread = errors.New("the installation recorded the version, and its answer saying which image digests it records could not be read")

// put sends the version and reads whatever the server says about it: what the version records,
// or why it was refused.
func put(ctx context.Context, url, token string, body api.Push) (api.Pushed, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return api.Pushed{}, fmt.Errorf("the version could not be written: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(encoded))
	if err != nil {
		return api.Pushed{}, fmt.Errorf("%s: %w", url, err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")

	answer, err := client(2 * time.Minute).Do(r)
	if err != nil {
		return api.Pushed{}, fmt.Errorf("%s could not be reached: %w", url, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode == http.StatusOK {
		var pushed api.Pushed
		if err := json.NewDecoder(answer.Body).Decode(&pushed); err != nil {
			return api.Pushed{}, fmt.Errorf("%w: %v", errAnswerUnread, err)
		}
		return pushed, nil
	}

	said := refusedBy(answer)
	switch answer.StatusCode {
	case http.StatusUnauthorized:
		return api.Pushed{}, fmt.Errorf("the installation did not accept the credential in %s", tokenVariable)
	case http.StatusNotFound:
		// The same answer an inaccessible workflow gets, which is the point: there is
		// nothing here to tell the two apart with, and saying so is more honest than
		// guessing.
		return api.Pushed{}, fmt.Errorf("no such namespace or workflow, or not yours")
	}
	return api.Pushed{}, fmt.Errorf("the installation refused the version: %s", said.said)
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
