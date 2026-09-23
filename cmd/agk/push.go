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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// agk push: "Registers the workflow in a namespace on a server."
//
// What it sends is what the server stores: the entry point, every file it includes, the manifest
// of every image it names, and the tree every step sees under /agk/repo. The server rebuilds it
// before writing it, so a push that would not come back is refused in front of the person pushing
// rather than at the first run.
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
	where := *server
	if where == "" {
		where = e.Getenv(serverVariable)
	}
	if where == "" {
		fmt.Fprintf(e.Err, "no installation to push to: pass --server or set %s\n", serverVariable)
		return exitUsage
	}
	token := e.Getenv(tokenVariable)
	if token == "" {
		fmt.Fprintf(e.Err, "no credential: set %s. It is not a flag, because an argument is in the shell history, in the process list and in whatever recorded the terminal\n", tokenVariable)
		return exitUsage
	}

	dir, base, err := entryOf(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	sha, err := commitOf(ctx, dir, *commit)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if !*dirty {
		if changed, err := dirtyTree(ctx, dir); err != nil {
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
	files, err := repositoryOf(ctx, dir, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// And the workflow is read out of those same bytes rather than off the disk, so that the
	// closure the version is rebuilt from and the tree a container is given cannot disagree
	// about any file both of them hold.
	tree := committed(files)
	wf, err := loadCommitted(tree, base, dir, sha)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if _, err := declaredInputs(wf, tree); err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// The manifests are read the way validate reads them, because a version the server
	// cannot build is a version it will refuse, and finding that out here is cheaper.
	read, code := readManifests(ctx, e, references(wf))
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
		Tree:   files,
		Branch: branchOf(ctx, dir),
	}
	name := string(wf.Metadata.Name)
	url := fmt.Sprintf("%s/api/v1/%s/workflows/%s/versions/%s",
		strings.TrimRight(where, "/"), *namespace, name, sha)
	if err := put(ctx, url, token, body); err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitRefused
	}

	fmt.Fprintf(e.Out, "%s/%s@%s pushed to %s\n", *namespace, name, short(sha), where)
	fmt.Fprintf(e.Out, "%s, %s, %s, %s\n",
		counted(len(wf.Steps), "step", "steps"),
		counted(len(files), "file", "files"),
		counted(len(captured.Includes), "included file", "included files"),
		counted(len(captured.Manifests), "manifest", "manifests"))
	return exitSucceeded
}

// entryOf is where the entry point is: the directory git is asked about, and the name inside it.
//
// The working copy is consulted for where and never for what, since every byte is read out of the
// commit. So what is said here are the two mistakes of -f that no commit could put right, in the
// words load says them in.
func entryOf(e Env, entry string) (string, string, error) {
	if entry == "" {
		entry = entryPoint
	}
	path := e.path(entry)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", "", fmt.Errorf("%s is a directory: -f names the entry point itself, which is %s inside it", path, entryPoint)
	}
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("there is no workflow at %s: one is read from %s in the directory the command is run in, or from the path -f names", path, entryPoint)
	}
	return dir, filepath.Base(path), nil
}

// repositoryOf is the tree of one commit as a container will see it, read out of git's objects.
//
// The commit rather than the working copy, for the reason this file opens with. What the commit
// tracks is also what leaves out an editor's leftovers, a build and a virtual environment: an
// ignored file is ignored because somebody said it is not part of the repository. The tree is
// rooted where the entry point is, because git lists a commit from the directory it runs in, and
// that is the root load gives every include and every schema reference.
//
// A file carries one of git's two modes, 0644 or 0755, and says which even where it is the
// ordinary one, so that nothing downstream has to guess what an absent mode meant. Two other kinds
// of entry are refused rather than carried. A symbolic link is resolved on whatever host lays the
// tree out, and nothing stops its target being outside the repository once it is there. A
// submodule is another repository, which a runner would need a credential to fetch, and a runner
// holds none.
//
// Every size is added up before any content is read, out of the listing git makes from object
// headers, so that a tree above the limit is refused having read nothing. The limit belongs to how
// the tree travels rather than to what a repository may be: until the installation serves the
// repository over git smart HTTP, which is v0.4.0, all of it rides inside one JSON request.
func repositoryOf(ctx context.Context, dir, sha string) (map[string]api.PushFile, error) {
	listed, err := gitOutput(ctx, dir, "ls-tree", "-r", "-z", "-l", sha)
	if err != nil {
		return nil, fmt.Errorf("the tree of %s could not be read from git: %w", short(sha), err)
	}

	type entry struct {
		path, mode, object string
		size               int64
	}
	var entries []entry
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

		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("the tree of %s could not be read from git: %q is not a line of its listing", short(sha), record)
		}
		total += size
		entries = append(entries, entry{path: path, mode: mode, object: fields[2], size: size})
	}
	if total > api.TreeMaxBytes {
		return nil, fmt.Errorf("the tree of %s is %d bytes, and a push carries at most %d until the installation hosts the repository itself: something this size belongs in an image or in an artifact", short(sha), total, api.TreeMaxBytes)
	}

	sizes := make(map[string]int64, len(entries))
	for _, f := range entries {
		sizes[f.object] = f.size
	}
	contents, err := contentsOf(ctx, dir, sizes)
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

	cmd := gitCommand(ctx, dir, "cat-file", "--batch")
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
	switch {
	case failed != nil:
		return nil, failed
	case err != nil:
		if said := strings.TrimSpace(errs.String()); said != "" {
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
	if info, err := fs.Stat(tree, base); err != nil || info.IsDir() {
		return nil, fmt.Errorf("%s holds no %s in %s: what is pushed is the commit, so the entry point has to be committed", short(sha), base, dir)
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

// put sends the version and reads whatever the server says about it.
func put(ctx context.Context, url, token string, body api.Push) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("the version could not be written: %w", err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")

	answer, err := (&http.Client{Timeout: 2 * time.Minute}).Do(r)
	if err != nil {
		return fmt.Errorf("%s could not be reached: %w", url, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode == http.StatusOK {
		return nil
	}

	var said struct {
		Error string `json:"error"`
	}
	json.NewDecoder(answer.Body).Decode(&said)
	if said.Error == "" {
		said.Error = answer.Status
	}
	switch answer.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("the installation did not accept the credential in %s", tokenVariable)
	case http.StatusNotFound:
		// The same answer an inaccessible workflow gets, which is the point: there is
		// nothing here to tell the two apart with, and saying so is more honest than
		// guessing.
		return fmt.Errorf("no such namespace or workflow, or not yours")
	}
	return fmt.Errorf("the installation refused the version: %s", said.Error)
}

// commitOf is the commit a push names, as the whole hash git holds it under.
//
// Resolved rather than taken as typed, so that a3f9c1e, the branch it is the tip of and HEAD push
// one version rather than three, and so that a name this repository does not hold is refused
// before anything is read. A name beginning with a dash is refused before git sees it, since git
// would take it for an option of its own.
//
// Outside a git repository there is no commit, and so nothing to push. Walking the directory
// instead would send whatever it holds under a hash nobody can check it against, which is the one
// lie this command exists to refuse, and would leave it guessing what a repository is made of: a
// virtual environment, a build, the .agk a local run leaves behind.
func commitOf(ctx context.Context, dir, named string) (string, error) {
	if _, err := git(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git is not installed, and agk push reads what it sends out of a git commit")
		}
		return "", fmt.Errorf("%s is not in a git repository, and a version is a commit: agk push reads what it sends out of one, so it runs inside the repository the workflow is committed to", dir)
	}
	switch {
	case named == "":
		named = "HEAD"
	case strings.HasPrefix(named, "-"):
		return "", fmt.Errorf("--commit %s names no commit: a commit is a hash, a branch or a tag", named)
	}
	sha, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", named+"^{commit}")
	switch {
	case err == nil && sha != "":
		return sha, nil
	case named == "HEAD":
		return "", fmt.Errorf("the repository at %s has no commit yet, and a version is a commit: commit the workflow, then push it", dir)
	}
	return "", fmt.Errorf("the repository at %s holds no commit %s", dir, named)
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

// gitOutput is what git wrote, exactly as it wrote it. A -z listing is read through this rather
// than through git, since a name may begin or end with a space and trimming it is renaming it.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, dir, args...)
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

// short is a commit as a person writes it.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
