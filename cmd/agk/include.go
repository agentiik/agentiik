package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// A workflow include, as agk push resolves it before anything is sent.
//
// "agk validate and agk push make the same checks where they reach the stores the hook reads", and
// a workflow include reads a store only an installation holds: another repository's library, at the
// commit its ref names, under the pusher's workflow:read on it. agk push reaches the installation it
// pushes to, so it reads the library there, through the tree route, with the credential the push
// presents, and judges the commit by what the hook will read. agk validate, agk graph and agk run
// --local reach no installation, and refuse a workflow include naming it.

// includeCommit is a commit written whole, which the tree route is asked for where no tag of that
// name is, and abbreviatedCommit one abbreviated, which an include is refused for as the hook
// refuses it.
var (
	includeCommit     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	abbreviatedCommit = regexp.MustCompile(`^[0-9a-f]{7,39}$`)
)

// include resolves a workflow include through the installation: its tag, or the commit of a version
// it names, and the files of that version, each read the first time resolution reads it.
//
// A branch is refused as the hook refuses it, since it moves; the installation answers a workflow
// the pusher may not read as one that is not there, and so does this.
func (at remote) include(ctx context.Context, ref graph.WorkflowRef) (fs.FS, string, error) {
	named := ref.Namespace + "/" + ref.Name
	route := "/api/v1/" + url.PathEscape(ref.Namespace) + "/workflows/" + url.PathEscape(ref.Name) + "/tree/"
	if strings.HasPrefix(ref.Ref, "refs/") && !strings.HasPrefix(ref.Ref, "refs/tags/") {
		return nil, "", fmt.Errorf("%s names a ref of %s that is no tag, and a workflow include is pinned to a tag or a commit: a branch moves, and another repository could change what this commit does without this commit changing", ref.Ref, named)
	}
	tag := ref.Ref
	if !strings.HasPrefix(tag, "refs/tags/") {
		tag = "refs/tags/" + tag
	}
	var tree api.Tree
	err := at.tree(ctx, route+escapedRef(tag), &tree)
	switch {
	case statusOf(err) == http.StatusNotFound && includeCommit.MatchString(ref.Ref):
		err = at.tree(ctx, route+ref.Ref, &tree)
	case statusOf(err) == http.StatusNotFound && abbreviatedCommit.MatchString(ref.Ref):
		return nil, "", fmt.Errorf("%s is a commit abbreviated, or no tag of %s, and a workflow include names a commit whole: an abbreviation is a name another commit of the library can come to share, and this file would then name two", ref.Ref, named)
	}
	switch {
	case statusOf(err) == http.StatusNotFound:
		return nil, "", fmt.Errorf("%s names no tag of %s and no commit it keeps as a version, or %s is no workflow you may read: a workflow include reads a library at a commit its hook judged, under workflow:read on it", ref.Ref, named, named)
	case statusOf(err) == http.StatusUnauthorized:
		return nil, "", errors.New(at.refusedCredential())
	case err != nil:
		return nil, "", fmt.Errorf("the library %s could not be read at %s: %w", named, ref.Ref, err)
	}
	files := make(map[string]int64, len(tree.Entries))
	for _, e := range tree.Entries {
		files[e.Path] = e.Size
	}
	return &libraryTree{at: at, ctx: ctx, route: route + tree.Commit, files: files, read: map[string][]byte{}}, tree.Commit, nil
}

// tree asks the tree route one question.
func (at remote) tree(ctx context.Context, path string, out *api.Tree) error {
	req, err := at.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return at.do(req, http.StatusOK, out)
}

// escapedRef is a ref as a path of the tree route writes it, each segment escaped and the slashes
// between them kept.
func escapedRef(ref string) string {
	segments := strings.Split(ref, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// libraryTree is a library's version as resolution reads it: the files its listing names, each
// fetched the first time it is read, and the ones it does not name absent without a request.
type libraryTree struct {
	at    remote
	ctx   context.Context
	route string
	files map[string]int64

	mu   sync.Mutex
	read map[string][]byte
}

func (t *libraryTree) Open(name string) (fs.File, error) {
	body, err := t.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: body, Mode: 0o444}}.Open(name)
}

// ReadFile fetches one file of the version, refusing one that says it weighs more than what the
// validation reads out of every tree together before a byte of it is sent.
func (t *libraryTree) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	t.mu.Lock()
	body, held := t.read[name]
	t.mu.Unlock()
	if held {
		return body, nil
	}
	size, named := t.files[name]
	switch {
	case !named:
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	case size > versions.ReadMaxBytes:
		return nil, fmt.Errorf("%s weighs %d bytes, past the %d the validation reads out of every tree together", name, size, versions.ReadMaxBytes)
	}
	req, err := t.at.request(t.ctx, http.MethodGet, t.route+"?path="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}
	answer, err := client(answerTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %v", errUnreachable, t.at.base, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		return nil, refusedBy(answer)
	}
	body, err = io.ReadAll(io.LimitReader(answer.Body, size+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s could not be read: %v", errUnreachable, name, err)
	}
	if int64(len(body)) != size {
		return nil, fmt.Errorf("%s arrived as %d bytes where the listing says %d", name, len(body), size)
	}
	t.mu.Lock()
	t.read[name] = body
	t.mu.Unlock()
	return body, nil
}
