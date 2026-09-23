package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentiik/agentiik/api"
	versions "github.com/agentiik/agentiik/version"
)

// agk push: "Registers the workflow in a namespace on a server."
//
// What it sends is what the server stores: the entry point, every file it includes, and the
// manifest of every image it names. The server rebuilds it before writing it, so a push that
// would not come back is refused in front of the person pushing rather than at the first run.
//
// # Why the tree has to be clean
//
// "A version is a commit. finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently,
// because that is what a commit already is." Pushing the bytes in the working copy under the name
// of a commit whose tree differs is a version that says it is one thing and is another, for ever,
// and nothing downstream can ever notice: the digests match what was pushed. So a dirty tree is
// refused, and --allow-dirty exists for somebody who knows what they are doing and says so.

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
	dirty := fs.Bool("allow-dirty", false, "Push although the working tree differs from the commit. A version is a commit, so this makes one that says it is a tree it is not.")
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

	wf, tree, dir, err := load(e, *entry)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}
	if _, err := declaredInputs(wf, tree); err != nil {
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
			fmt.Fprintf(e.Err, "the working tree differs from %s in %s, so what this would push is not what that commit names: commit it, or pass --allow-dirty and know that the version will say it is a tree it is not\n",
				short(sha), counted(len(changed), "file", "files"))
			for _, f := range changed {
				fmt.Fprintf(e.Err, "  %s\n", f)
			}
			return exitRefused
		}
	}

	// The manifests are read the way validate reads them, because a version the server
	// cannot build is a version it will refuse, and finding that out here is cheaper.
	read, code := readManifests(ctx, e, references(wf))
	if code != exitSucceeded {
		return code
	}

	// load answers the tree and the directory it was rooted at; the entry point inside it is
	// what Capture is given, because a version names a path in a tree rather than on a disk.
	base := entryPoint
	if *entry != "" {
		base = filepath.Base(*entry)
	}
	captured, err := versions.Capture(tree, base, read)
	if err != nil {
		refusal(e.Err, err)
		return exitRefused
	}

	// The whole tree, because "every step of every run sees it, mounted read-only at
	// /agk/repo", and the installation holds no clone of the repository to read it out of.
	files, err := repositoryOf(ctx, dir)
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

// repositoryOf is the repository as a container will see it.
//
// What git tracks, rather than what is on the disk: a version is a commit, and a commit holds
// tracked files. Reading the directory instead would put whatever an editor, a build or a virtual
// environment left behind into every run of every version, and an ignored file is ignored because
// somebody said it is not part of the repository.
func repositoryOf(ctx context.Context, dir string) (map[string]api.PushFile, error) {
	files := map[string]api.PushFile{}
	var total int64

	add := func(rel string) error {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		if info.IsDir() {
			// A submodule, or a directory git lists for some other reason. The tree
			// is files.
			return nil
		}
		content, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		total += int64(len(content))
		if total > api.TreeMaxBytes {
			return fmt.Errorf("this repository is above the %d bytes a tree may be: a workflow repository is an entry point, its fragments and its scripts, and something this size belongs in an image or in an artifact", api.TreeMaxBytes)
		}
		f := api.PushFile{Content: content}
		if info.Mode().Perm()&0o111 != 0 {
			f.Mode = "0755"
		}
		files[filepath.ToSlash(rel)] = f
		return nil
	}

	listed, err := git(ctx, dir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("the files of the repository could not be listed from git: %w", err)
	}
	for _, rel := range strings.Split(listed, "\x00") {
		if rel == "" {
			continue
		}
		if err := add(rel); err != nil {
			return nil, err
		}
	}
	return files, nil
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

// dirtyTree is what differs between the working copy and the commit, which is what makes a push
// of that commit a lie.
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

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(errs.String()); said != "" {
			return "", fmt.Errorf("%s", said)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// short is a commit as a person writes it.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
