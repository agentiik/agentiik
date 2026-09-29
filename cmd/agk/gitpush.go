package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/repo"
)

// agk push speaks git's smart HTTP itself, with the client every other command talks to the
// installation through, rather than running git push.
//
// git push would need the credential handed to it: on its command line, where every user of the
// machine reads it in the process list, or through a credential helper this command would have to
// configure. It would need to trust the installation's certificate as this command does, where
// Homebrew's git reads OpenSSL's bundle rather than the keychain Go reads. And it follows a
// redirect with the credential, which this command's client never does. git still reads the local
// repository and writes the pack; only the two requests are this command's.

// pushRef is the ref a push moves, in full, and the object it leaves it at: a branch at the commit,
// and a tag at what it names, an annotated tag's own object included.
//
// Named by --branch where it is given; otherwise by --commit where it names a branch or a tag;
// otherwise the branch checked out. A hash, or a detached HEAD, names no ref, and git itself asks
// for one then: pushing a commit leaves some branch or tag at it, and choosing one for the pusher
// could move the branch production runs.
//
// The name is read as git reads it, which is how commitOf read the commit sha out of it, so that the
// ref moved is the one whose commit was judged: a name both a branch and a tag hold is refused, as
// the installation refuses it, rather than read one way here and the other way there.
func pushRef(ctx context.Context, top, named, branch, sha string) (string, string, error) {
	if branch != "" {
		if err := checkBranchName(branch); err != nil {
			return "", "", err
		}
		return "refs/heads/" + branch, sha, nil
	}
	if named == "" || named == "HEAD" || named == "@" {
		checked, err := git(ctx, top, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || !strings.HasPrefix(checked, "refs/heads/") {
			return "", "", fmt.Errorf("HEAD is detached, and a push leaves a branch at the commit: name it with --branch")
		}
		return checked, sha, nil
	}
	if !strings.HasPrefix(named, "refs/") {
		_, errBranch := git(ctx, top, "rev-parse", "--verify", "--quiet", "refs/heads/"+named)
		_, errTag := git(ctx, top, "rev-parse", "--verify", "--quiet", "refs/tags/"+named)
		if errBranch == nil && errTag == nil {
			return "", "", fmt.Errorf("--commit %s is both a branch and a tag: name the one meant in full, refs/heads/%s or refs/tags/%s", named, named, named)
		}
	}
	full, _ := git(ctx, top, "rev-parse", "--verify", "--quiet", "--symbolic-full-name", named)
	if !strings.HasPrefix(full, "refs/heads/") && !strings.HasPrefix(full, "refs/tags/") {
		return "", "", fmt.Errorf("--commit %s names a commit and no branch or tag of this repository, and a push leaves a branch at the commit: name it with --branch", named)
	}
	object, err := git(ctx, top, "rev-parse", "--verify", "--quiet", full)
	if err != nil || object == "" {
		return "", "", fmt.Errorf("%s could not be read from git", full)
	}
	if peeled, err := git(ctx, top, "rev-parse", "--verify", "--quiet", full+"^{commit}"); err != nil || peeled != sha {
		return "", "", fmt.Errorf("%s names %s, where --commit %s was read as %s", full, short(peeled), named, short(sha))
	}
	return full, object, nil
}

// aheadOf refuses to move ref from old, where the installation holds it, to object unless the move
// is one git makes without --force: a branch moved to a commit that has old in its history. A tag
// is never moved, since a tag others fetched names one commit for good.
//
// The installation moves either all the same for whoever holds grant:manage on the workflow, which
// is what makes such a push forced rather than refused. agk push makes none, so that a push from a
// clone that is behind, or from a branch rewritten here, is told so rather than rewriting what others
// fetched: git push --force is how a forced push is made, deliberately.
func aheadOf(ctx context.Context, top, ref, old, object, sha string) error {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("%s is at %s on the installation, and a tag names one commit for good: push the commit under a new tag", ref, short(old))
	}
	held, err := heldHere(ctx, top, []string{old})
	if err != nil {
		return err
	}
	if len(held) == 0 {
		return fmt.Errorf("%s is at %s on the installation, which this repository does not hold: fetch it, merge or rebase, and push again", ref, short(old))
	}
	if err := gitCommand(ctx, top, "merge-base", "--is-ancestor", old, sha).Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("%s is at %s on the installation, and %s is not ahead of it: merge or rebase onto it, and push again; moving a branch back or aside is a forced push, which agk push does not make", ref, short(old), short(sha))
		}
		return fmt.Errorf("whether %s is ahead of %s could not be read from git: %w", short(sha), short(old), err)
	}
	return nil
}

// shallowBelow is a commit the push would send whose parents this clone does not hold, where it is
// shallow and the installation lacks the history below it: git fetch --unshallow is then the only
// way to push it, since the installation holds every object a commit it keeps reaches. Empty where
// the clone is whole, or the history it lacks is history the installation's refs already reach.
func shallowBelow(ctx context.Context, top, sha string, have []string) (string, error) {
	if is, err := git(ctx, top, "rev-parse", "--is-shallow-repository"); err != nil || is != "true" {
		return "", err
	}
	file, err := git(ctx, top, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(top, file)
	}
	listed, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("the shallow commits of this clone could not be read: %w", err)
	}
	boundary := map[string]bool{}
	for _, id := range strings.Fields(string(listed)) {
		boundary[id] = true
	}
	revs := sha + "\n"
	for _, id := range have {
		revs += "^" + id + "\n"
	}
	sent, err := output(stdinOf(gitCommand(ctx, top, "rev-list", "--stdin"), revs))
	if err != nil {
		return "", fmt.Errorf("what the push sends could not be read from git: %w", err)
	}
	for _, id := range strings.Fields(string(sent)) {
		if boundary[id] {
			return id, nil
		}
	}
	return "", nil
}

// checkBranchName refuses a branch git would refuse, as git check-ref-format does.
func checkBranchName(branch string) error {
	cmd := exec.Command("git", "check-ref-format", "--branch", branch)
	if strings.HasPrefix(branch, "-") || cmd.Run() != nil {
		return fmt.Errorf("--branch %s is no branch git could name", branch)
	}
	return nil
}

// createRepository creates the workflow's repository, empty, where the namespace does not hold it:
// a git push never creates one. Its default branch is the one given, the branch the push came
// from, since that is what a clone of it will check out. An installation answering that it holds
// one already, or that the caller may not create one in the namespace, which an editor of this
// one workflow may not, is let through: the push says whether it may push.
func createRepository(ctx context.Context, at remote, namespace, name, branch string) error {
	body, err := json.Marshal(api.WorkflowCreate{Name: name, DefaultBranch: branch})
	if err != nil {
		return err
	}
	req, err := at.request(ctx, http.MethodPost, "/api/v1/"+namespace+"/workflows", bytes.NewReader(body))
	if err != nil {
		return err
	}
	err = at.do(req, http.StatusCreated, nil)
	switch statusOf(err) {
	case http.StatusConflict, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized:
		return errors.New(at.refusedCredential())
	}
	return err
}

// recordImages records in the repository the digest each tag was resolved to here, and the brick
// manifest of each image a brick step runs, as the images hold them: a git push carries neither,
// and the installation reaches no registry to find them.
func recordImages(ctx context.Context, at remote, namespace, name string, pins map[string]string, manifests map[string][]byte) error {
	if len(pins) == 0 && len(manifests) == 0 {
		return nil
	}
	recording := api.RecordImages{Pins: pins, Manifests: map[string]string{}}
	for image, text := range manifests {
		recording.Manifests[image] = string(text)
	}
	body, err := json.Marshal(recording)
	if err != nil {
		return err
	}
	req, err := at.request(ctx, http.MethodPost, "/api/v1/"+namespace+"/workflows/"+name+"/images", bytes.NewReader(body))
	if err != nil {
		return err
	}
	err = at.do(req, http.StatusOK, nil)
	switch statusOf(err) {
	case 0:
		return err
	case http.StatusUnauthorized:
		return errors.New(at.refusedCredential())
	case http.StatusNotFound:
		return errors.New("no such namespace or workflow, or not yours: recording the images a push is judged against takes workflow:write on it")
	}
	return fmt.Errorf("the installation refused the images the push names: %w", err)
}

// isVersion says whether the installation holds commit as a version of the workflow already, which
// the tree at that commit answers. An answer that is not a yes is taken as a no: it decides only
// whether a sentence is said.
func isVersion(ctx context.Context, at remote, namespace, name, commit string) bool {
	req, err := at.request(ctx, http.MethodGet, "/api/v1/"+namespace+"/workflows/"+name+"/tree/"+commit, nil)
	if err != nil {
		return false
	}
	return at.do(req, http.StatusOK, nil) == nil
}

// gitAnswer is one answer of the repository's git routes: its status, and what it says, which is
// text a person reads where it refuses.
func gitAnswer(at remote, req *http.Request, want string) (*http.Response, error) {
	answer, err := client(0).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %v", errUnreachable, at.base, err)
	}
	if answer.StatusCode == http.StatusOK && answer.Header.Get("Content-Type") == want {
		return answer, nil
	}
	defer answer.Body.Close()
	said, _ := io.ReadAll(io.LimitReader(answer.Body, 4<<10))
	if answer.StatusCode >= 500 {
		// No verdict: a gateway in front of the installation, or the installation failing
		// on its side; and a push it answered so may have moved its ref all the same.
		if req.Method == http.MethodPost {
			return nil, fmt.Errorf("%w: %s answered %s, and whether the push landed is unknown: agk push again reads where the ref stands", errUnreachable, at.base, answer.Status)
		}
		return nil, fmt.Errorf("%w: %s answered %s, and nothing was pushed", errUnreachable, at.base, answer.Status)
	}
	switch answer.StatusCode {
	case http.StatusUnauthorized:
		return nil, errors.New(at.refusedCredential())
	case http.StatusNotFound:
		return nil, errors.New("no such namespace or workflow, or not yours")
	case http.StatusOK:
		return nil, fmt.Errorf("%s answered %s rather than git's %s: is it an installation of v0.4.0 or later?", req.URL, answer.Header.Get("Content-Type"), want)
	}
	if text := strings.TrimSpace(string(said)); text != "" && !strings.HasPrefix(text, "{") {
		return nil, fmt.Errorf("the installation refused the push: %s", text)
	}
	return nil, fmt.Errorf("the installation refused the push: %s", refusedBy(&http.Response{StatusCode: answer.StatusCode, Status: answer.Status, Body: io.NopCloser(bytes.NewReader(said))}).said)
}

// remoteRefs are the refs the repository advertises for a push, by name, each the object it names.
func remoteRefs(ctx context.Context, at remote, repository string) (map[string]string, error) {
	req, err := at.request(ctx, http.MethodGet, repository+"/info/refs?service=git-receive-pack", nil)
	if err != nil {
		return nil, err
	}
	answer, err := gitAnswer(at, req, "application/x-git-receive-pack-advertisement")
	if err != nil {
		return nil, err
	}
	defer answer.Body.Close()
	pkts := repo.NewPktReader(io.LimitReader(answer.Body, 64<<20))
	refs := map[string]string{}
	flushes := 0
	for flushes < 2 {
		kind, data, err := pkts.Next()
		if err != nil {
			return nil, fmt.Errorf("the repository's refs could not be read: %w", err)
		}
		if kind == repo.PktFlush {
			flushes++
			continue
		}
		line := strings.TrimSuffix(string(data), "\n")
		if flushes == 0 {
			continue // "# service=git-receive-pack"
		}
		line, _, _ = strings.Cut(line, "\x00")
		id, name, found := strings.Cut(line, " ")
		if !found || name == "capabilities^{}" {
			continue
		}
		refs[name] = id
	}
	return refs, nil
}

// heldHere are the objects of ids this repository holds, which the pack leaves out: the
// installation holds them, and a thin pack may make deltas against them.
func heldHere(ctx context.Context, top string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cmd := gitCommand(ctx, top, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	out, err := output(cmd)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, rest, _ := strings.Cut(line, " ")
		if rest != "missing" && id != "" {
			held = append(held, id)
		}
	}
	return held, nil
}

// pushed is how the installation answered a push: accepted, or refused and why.
type pushed struct {
	accepted bool
	why      string
}

// sendPack pushes new to ref of the repository, moving it from old, with the objects new reaches
// that the installation's refs do not, and prints what the installation tells the pusher as git
// prints it, after "remote:".
func sendPack(ctx context.Context, e Env, at remote, top, repository, ref, old, next string, have []string) (pushed, error) {
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	var head []byte
	head, _ = repo.AppendPkt(head, []byte(old+" "+next+" "+ref+"\x00report-status side-band-64k agent=agk\n"))
	head = append(head, "0000"...)

	// The pack of what the installation lacks, thin, deltas against what it holds allowed.
	revs := next + "\n"
	for _, id := range have {
		revs += "^" + id + "\n"
	}
	// Counted first, since a push of what the installation holds already, a branch left at a
	// commit it has, sends no pack at all, as git sends none.
	counted, err := output(stdinOf(gitCommand(ctx, top, "rev-list", "--count", "--objects", "--stdin"), revs))
	if err != nil {
		return pushed{}, fmt.Errorf("what the push sends could not be read from git: %w", err)
	}
	var pack io.Reader = bytes.NewReader(nil)
	var packing *exec.Cmd
	var packed io.Closer
	var packErrs bytes.Buffer
	if strings.TrimSpace(string(counted)) != "0" {
		packing = stdinOf(gitCommand(ctx, top, "pack-objects", "--stdout", "--revs", "--thin", "-q"), revs)
		out, err := packing.StdoutPipe()
		if err != nil {
			return pushed{}, err
		}
		packing.Stderr = &packErrs
		if err := packing.Start(); err != nil {
			return pushed{}, fmt.Errorf("the pack could not be written: %w", err)
		}
		pack, packed = out, out
	}

	req, err := at.request(ctx, http.MethodPost, repository+"/git-receive-pack", io.MultiReader(bytes.NewReader(head), pack))
	if err != nil {
		return pushed{}, err
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Accept", "application/x-git-receive-pack-result")
	answer, err := gitAnswer(at, req, "application/x-git-receive-pack-result")
	// What git says of a pack it could not write: a pack it failed on here, an object it could
	// not read, is its own failure and said first; a pack it was cut off writing, the pipe closed
	// because the installation stopped reading, is the installation's to explain.
	var packErr error
	cutOff := false
	if packing != nil {
		// Whatever the answer, git is waited for, the pipe closed first so that a git still
		// writing a pack nobody reads ends rather than waits.
		packed.Close()
		if werr := packing.Wait(); werr != nil {
			packErr = fmt.Errorf("the pack could not be written: %s", strings.TrimSpace(packErrs.String()+" "+werr.Error()))
			cutOff = brokenPipe(werr, packErrs.String())
		}
	}
	if packErr != nil && !cutOff {
		if answer != nil {
			answer.Body.Close()
		}
		return pushed{}, packErr
	}
	if err != nil {
		// What the installation answered comes first: a pack it stopped reading, one past its
		// bound, leaves git with a broken pipe that says nothing of why.
		return pushed{}, err
	}
	defer answer.Body.Close()

	band := repo.NewSidebandReader(repo.NewPktReader(answer.Body))
	band.Progress = &remoteLines{w: e.Err}
	report := repo.NewPktReader(band)
	var result pushed
	stated := false
	for {
		kind, data, err := report.Next()
		if err != nil {
			var said *repo.RemoteError
			if errors.As(err, &said) {
				return pushed{why: said.Message}, nil
			}
			if packErr != nil {
				return pushed{}, packErr
			}
			return pushed{}, fmt.Errorf("%w: its answer to the push was cut off (%v), and whether the push landed is unknown: agk push again reads where the ref stands", errUnreachable, err)
		}
		if kind == repo.PktFlush {
			break
		}
		line := strings.TrimSuffix(string(data), "\n")
		switch {
		case strings.HasPrefix(line, "unpack ") && line != "unpack ok":
			result.why, stated = strings.TrimPrefix(line, "unpack "), true
		case line == "ok "+ref:
			result, stated = pushed{accepted: true}, true
		case strings.HasPrefix(line, "ng "+ref+" "):
			result, stated = pushed{why: strings.TrimPrefix(line, "ng "+ref+" ")}, true
		}
	}
	band.Progress.(*remoteLines).flush()
	switch {
	case stated:
		// The installation's word, over a pack git could not finish: a pack it refused to
		// read on leaves git with a broken pipe that says nothing of why.
		return result, nil
	case packErr != nil:
		return pushed{}, packErr
	}
	return pushed{}, fmt.Errorf("%w: its answer to the push named no status for %s, and whether the push landed is unknown: agk push again reads where the ref stands", errUnreachable, ref)
}

// brokenPipe is whether git stopped writing because nothing read what it wrote: killed by SIGPIPE,
// or saying so where it caught the write's failure.
func brokenPipe(err error, said string) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGPIPE {
			return true
		}
	}
	return strings.Contains(said, "Broken pipe")
}

// stdinOf is cmd reading text on its standard input.
func stdinOf(cmd *exec.Cmd, text string) *exec.Cmd {
	cmd.Stdin = strings.NewReader(text)
	return cmd
}

// remoteLines writes what the installation tells the pusher, a line at a time, each after
// "remote: ", as git writes it.
type remoteLines struct {
	w    io.Writer
	part []byte
}

func (r *remoteLines) Write(b []byte) (int, error) {
	r.part = append(r.part, b...)
	for {
		i := bytes.IndexAny(r.part, "\n\r")
		if i < 0 {
			return len(b), nil
		}
		if line := strings.TrimSpace(string(r.part[:i])); line != "" {
			fmt.Fprintf(r.w, "remote: %s\n", line)
		}
		r.part = r.part[i+1:]
	}
}

func (r *remoteLines) flush() {
	if line := strings.TrimSpace(string(r.part)); line != "" {
		fmt.Fprintf(r.w, "remote: %s\n", line)
	}
	r.part = nil
}
