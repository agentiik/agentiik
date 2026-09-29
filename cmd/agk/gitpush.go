package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

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
func pushRef(ctx context.Context, top, named, branch, sha string) (string, string, error) {
	if branch != "" {
		if err := checkBranchName(branch); err != nil {
			return "", "", err
		}
		return "refs/heads/" + branch, sha, nil
	}
	if named == "" {
		checked, err := git(ctx, top, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || !strings.HasPrefix(checked, "refs/heads/") {
			return "", "", fmt.Errorf("HEAD is detached, and a push leaves a branch at the commit: name it with --branch")
		}
		return checked, sha, nil
	}
	for _, full := range []string{"refs/heads/" + named, "refs/tags/" + named, named} {
		if !strings.HasPrefix(full, "refs/heads/") && !strings.HasPrefix(full, "refs/tags/") {
			continue
		}
		object, err := git(ctx, top, "rev-parse", "--verify", "--quiet", full)
		if err == nil && object != "" {
			return full, object, nil
		}
	}
	return "", "", fmt.Errorf("--commit %s names a commit and no branch or tag, and a push leaves a branch at the commit: name it with --branch", named)
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
	if packing != nil {
		// Whatever the answer, git is waited for, the pipe closed first so that a git still
		// writing a pack nobody reads ends rather than waits; and a pack it failed to write
		// is said rather than the refusal of a pack cut short that the installation answers.
		packed.Close()
		werr := packing.Wait()
		if err == nil && werr != nil {
			answer.Body.Close()
			return pushed{}, fmt.Errorf("the pack could not be written: %s", strings.TrimSpace(packErrs.String()+" "+werr.Error()))
		}
	}
	if err != nil {
		return pushed{}, err
	}
	defer answer.Body.Close()

	band := repo.NewSidebandReader(repo.NewPktReader(answer.Body))
	band.Progress = &remoteLines{w: e.Err}
	report := repo.NewPktReader(band)
	result := pushed{why: "the installation answered no status for " + ref}
	for {
		kind, data, err := report.Next()
		if err != nil {
			var said *repo.RemoteError
			if errors.As(err, &said) {
				return pushed{why: said.Message}, nil
			}
			return pushed{}, fmt.Errorf("the installation's answer to the push could not be read: %w", err)
		}
		if kind == repo.PktFlush {
			break
		}
		line := strings.TrimSuffix(string(data), "\n")
		switch {
		case strings.HasPrefix(line, "unpack ") && line != "unpack ok":
			result.why = strings.TrimPrefix(line, "unpack ")
		case line == "ok "+ref:
			result = pushed{accepted: true}
		case strings.HasPrefix(line, "ng "+ref+" "):
			result = pushed{why: strings.TrimPrefix(line, "ng "+ref+" ")}
		}
	}
	band.Progress.(*remoteLines).flush()
	return result, nil
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
