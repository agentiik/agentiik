package api

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// Publishing files: POST /api/v1/{namespace}/workflows/{workflow}/commits.
//
// "Publishes files: commits them onto a branch as the caller, and moves the branch as a git push of
// that commit would." The route writes the blobs, the trees and the commit a clone would write, and
// hands them to the path a git push takes, receive, judge and accept, so that the pre-receive hook
// judges the commit, the grants it needs are a push's, the version is recorded as a push records it
// and the audit log says what it says of any push. "A route with checks of its own would be a second
// way into a repository, to keep in step with the hook's; one that is a push has nothing of its own
// to keep in step."

// commitRequest is what the route reads: "{branch, parent, message, files}".
type commitRequest struct {
	Branch  string
	Parent  string
	Message string
	files   []committedFile

	// bytes is what the files' texts and paths weigh together, held to TreeMaxBytes as a tree a push
	// carries is.
	bytes int
}

// committedFile is one path the commit writes: its new text, or removed where text is nil.
type committedFile struct {
	path string
	text *string
}

func (c *commitRequest) field(b *body, name string) error {
	switch name {
	case "branch":
		return text(b, &c.Branch)
	case "parent":
		return text(b, &c.Parent)
	case "message":
		return text(b, &c.Message)
	case "files":
		return filesOf(b, &c.files, &c.bytes)
	}
	return fmt.Errorf("the request body names %.64q, and a commit takes branch, parent, message and files", name)
}

// filesOf reads the files a commit writes or a validation lays over a tree: each path mapped to its
// text, or to null to remove it, at most TreeMaxFiles of them and TreeMaxBytes with their paths.
func filesOf(b *body, files *[]committedFile, weight *int) error {
	return b.object(TreeMaxFiles, fmt.Sprintf("files names at most %d paths, the most a version's tree holds", TreeMaxFiles), func(p string) error {
		for _, f := range *files {
			if f.path == p {
				return twice("the file", p)
			}
		}
		f := committedFile{path: p}
		if b.d.PeekKind() == jsontext.KindNull {
			if _, err := b.d.ReadToken(); err != nil {
				return malformed(err)
			}
		} else {
			var s string
			if err := text(b, &s); err != nil {
				return err
			}
			f.text = &s
			*weight += len(s)
		}
		*weight += len(p)
		if *weight > TreeMaxBytes {
			return &tooLarge{reason: fmt.Sprintf("files holds at most %d bytes with their paths, as the tree a push carries does: a file this size belongs in an image or in an artifact", TreeMaxBytes)}
		}
		*files = append(*files, f)
		return nil
	})
}

// checkFiles refuses a path no tree holds and a text that is not UTF-8, before any tree is written.
func checkFiles(files []committedFile) error {
	for _, f := range files {
		if err := version.TreePath(f.path); err != nil {
			return &commitRefused{http.StatusUnprocessableEntity, err.Error()}
		}
		if f.text != nil && !utf8.ValidString(*f.text) {
			return &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is written as text that is not UTF-8: a binary file is pushed with git", f.path)}
		}
	}
	return nil
}

// Committed is what the route answers: the commit made, the branch it moved and the commit it
// follows, empty for the first of a repository.
type Committed struct {
	Commit string `json:"commit"`
	Branch string `json:"branch"`
	Parent string `json:"parent"`
}

// commitFiles is the route.
func (s *Server) commitFiles(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var c commitRequest
	if err := readAtMost(r, &c, pushMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	// A commit workflow.commit makes is marked as arriving through MCP, in its message and in the
	// audit log: the route is the same, and what made the request is the MCP server.
	via := ""
	if _, ok := throughOf(r.Context()); ok {
		via = "mcp"
	}
	committed, err := s.commit(r, who, over, c, via)
	if err != nil {
		s.answerCommit(w, err)
		return
	}
	write(w, http.StatusCreated, committed)
}

// commitRefused is a commit the route refuses before it is a push: the request, or where the branch
// stands, which the caller can change.
type commitRefused struct {
	status int
	why    string
}

func (e *commitRefused) Error() string { return e.why }

// commit makes the commit and pushes it, through says what made it where it was not the route
// itself, workflow.commit's "mcp", which the commit's message and the audit log both carry.
func (s *Server) commit(r *http.Request, who Principal, over Target, c commitRequest, through string) (Committed, error) {
	ctx := r.Context()
	if strings.TrimSpace(c.Message) == "" {
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, "a commit carries a message, one character at least, which git log shows beside it"}
	}
	if len(c.files) == 0 {
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, "a commit writes at least one file: files maps each path to its new text, or to null to remove it"}
	}
	if err := checkFiles(c.files); err != nil {
		return Committed{}, err
	}
	if s.packs == nil || s.objects == nil {
		return Committed{}, &commitRefused{http.StatusServiceUnavailable, noPacks}
	}

	repository, err := s.repositoryOf(ctx, over)
	if errors.Is(err, db.ErrNoWorkflow) {
		return Committed{}, &commitRefused{http.StatusNotFound, notFound}
	}
	if err != nil {
		return Committed{}, err
	}
	objects, err := s.objectsOf(repository)
	if err != nil {
		return Committed{}, err
	}
	defer objects.Close()

	branch := c.Branch
	if branch == "" {
		branch = repository.DefaultBranch
	}
	ref := "refs/heads/" + branch
	if err := db.CheckRef(ref); err != nil {
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%.128s is not a branch name, on git's rules for a ref's name", branch)}
	}
	// An unborn branch is listed with no commit, the default branch of an empty repository among
	// them, so a repository holds commits where a ref points at one.
	head, born := "", false
	for _, rf := range repository.Refs {
		if rf.Name == ref {
			head = rf.Target()
		}
		born = born || rf.Target() != ""
	}
	var parent repo.ID
	if c.Parent != "" {
		if parent, err = repo.ParseID(c.Parent); err != nil {
			return Committed{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("parent is a commit's name, forty hexadecimal digits, and %.64q is not one", c.Parent)}
		}
	}
	switch {
	case head != "" && c.Parent == "":
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s points at %s: parent names the commit the files were read at, so that a commit made from a tree nobody read again undoes nothing that landed since", branch, head)}
	case head != "" && parent.String() != head:
		return Committed{}, &commitRefused{http.StatusConflict, fmt.Sprintf("%s has moved since %s: it points at %s now. Read the files again at it, and commit what they hold then", branch, c.Parent, head)}
	case head == "" && c.Parent == "" && born:
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is a new branch, and a new branch starts at a commit of the repository: parent names it", branch)}
	}

	var base repo.ID
	var parents []repo.ID
	if c.Parent != "" {
		t, data, err := repo.ReadObject(ctx, objects, parent, repo.MaxParsedBytes)
		if errors.Is(err, repo.ErrMissing) || (err == nil && t != repo.TypeCommit) {
			return Committed{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is not a commit of the repository", c.Parent)}
		}
		if err != nil {
			return Committed{}, err
		}
		pc, err := repo.ParseCommit(data)
		if err != nil {
			return Committed{}, err
		}
		base, parents = pc.Tree, []repo.ID{parent}
	}

	w := &objectsWritten{}
	changes := map[string]*string{}
	for _, f := range c.files {
		changes[f.path] = f.text
	}
	root, err := rewrite(ctx, objects, base, "", changes, w)
	if err != nil {
		return Committed{}, err
	}
	signature, err := s.signatureOf(ctx, who)
	if err != nil {
		return Committed{}, err
	}
	message := strings.TrimRight(c.Message, "\n") + "\n"
	if through != "" {
		message += "\nVia: " + through + "\n"
	}
	encoded, err := (&repo.Commit{Tree: root, Parents: parents, Author: signature, Committer: signature, Message: message}).Encode()
	if err != nil {
		return Committed{}, &commitRefused{http.StatusUnprocessableEntity, err.Error()}
	}
	id := w.add(repo.TypeCommit, encoded)

	var pack bytes.Buffer
	pw, err := repo.NewPackWriter(&pack, len(w.objects))
	if err != nil {
		return Committed{}, err
	}
	for _, o := range w.objects {
		if _, err := pw.Add(o.t, o.data); err != nil {
			return Committed{}, err
		}
	}
	if _, err := pw.Close(); err != nil {
		return Committed{}, err
	}

	old := repo.ID{}
	if head != "" {
		old = parent
	}
	p := pushed{commands: []command{{ref: ref, old: old, new: id}}, through: through}
	p.tool, _ = throughOf(ctx)
	received, err := s.receive(ctx, &pack, objects, p)
	if err != nil {
		return Committed{}, s.pushRefused(ctx, who, over, p, err)
	}
	defer received.close()
	checked, err := s.judge(ctx, r, who, over, repository, received, p)
	if err != nil {
		return Committed{}, s.pushRefused(ctx, who, over, p, err)
	}
	if err := s.accept(ctx, who, over, received, p, checked); err != nil {
		return Committed{}, s.pushRefused(ctx, who, over, p, err)
	}
	return Committed{Commit: id.String(), Branch: branch, Parent: c.Parent}, nil
}

// pushRefused records a push refused as a git push's refusal is recorded, and answers it; an error that
// is no refusal is the failure it is.
func (s *Server) pushRefused(ctx context.Context, who Principal, over Target, p pushed, err error) error {
	var why *pushRefusal
	if errors.As(err, &why) {
		s.recordRefusal(ctx, who, over, p, why)
	}
	return err
}

// answerCommit answers what commit refused, or the failure it met.
func (s *Server) answerCommit(w http.ResponseWriter, err error) {
	var refused *commitRefused
	var why *pushRefusal
	var fault *pushFault
	switch {
	case errors.As(err, &refused):
		fail(w, refused.status, refused.why)
	case errors.As(err, &why) && why.problem != nil:
		write(w, http.StatusUnprocessableEntity, struct {
			Error string `json:"error"`
			version.Problem
		}{why.short, *why.problem})
	case errors.As(err, &why):
		fail(w, why.status(), strings.TrimPrefix(why.short, "refused: "))
	case errors.As(err, &fault):
		s.report(fault.err)
		fail(w, http.StatusInternalServerError, fault.said)
	default:
		s.report(fmt.Errorf("api: a commit: %w", err))
		fail(w, http.StatusInternalServerError, "the commit could not be made")
	}
}

// signatureOf is the caller as a commit's author and committer: "a user's name and email address,
// or their login at the installation's host where they have none, and a service account's name at
// the host".
func (s *Server) signatureOf(ctx context.Context, who Principal) (repo.Signature, error) {
	host := "localhost"
	if u, err := url.Parse(s.publicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	sig := repo.Signature{Name: string(who), Email: string(who) + "@" + host, When: s.now().Unix(), Zone: "+0000"}
	if strings.Contains(string(who), "/") {
		return sig, nil
	}
	err := s.pool.Installation(ctx, db.Identity, func(ctx context.Context, wide *db.Wide) error {
		u, err := wide.User(ctx, string(who))
		if errors.Is(err, db.ErrNoPrincipal) {
			return nil
		}
		if err != nil {
			return err
		}
		sig.Name = u.DisplayName()
		if u.Email != "" {
			sig.Email = u.Email
		}
		return nil
	})
	return sig, err
}

// objectsWritten are the objects a commit adds, in the order they were written, each once.
type objectsWritten struct {
	objects []writtenObject
	seen    map[repo.ID]bool
}

type writtenObject struct {
	t    repo.Type
	data []byte
}

func (w *objectsWritten) add(t repo.Type, data []byte) repo.ID {
	id := repo.HashObject(t, data)
	if w.seen == nil {
		w.seen = map[repo.ID]bool{}
	}
	if !w.seen[id] {
		w.seen[id] = true
		w.objects = append(w.objects, writtenObject{t, data})
	}
	return id
}

// rewrite writes the tree at dir with changes applied below it, every path in changes relative to the
// root, and answers the new tree's name; a directory left empty is removed with its last file, as git
// keeps no empty directory. base is the tree as it was, zero where there was none.
func rewrite(ctx context.Context, objects repo.Lookup, base repo.ID, dir string, changes map[string]*string, w *objectsWritten) (repo.ID, error) {
	var entries []repo.TreeEntry
	if !base.IsZero() {
		_, data, err := repo.ReadObject(ctx, objects, base, repo.MaxParsedBytes)
		if err != nil {
			return repo.ID{}, err
		}
		if entries, err = repo.ParseTree(data); err != nil {
			return repo.ID{}, err
		}
	}
	// What changes here, by the name of this directory's entry each change is under.
	below := map[string]map[string]*string{}
	here := map[string]*string{}
	for p, text := range changes {
		rel, ok := strings.CutPrefix(p, dir)
		if !ok {
			continue
		}
		if name, _, deeper := strings.Cut(rel, "/"); deeper {
			if below[name] == nil {
				below[name] = map[string]*string{}
			}
			below[name][p] = text
		} else {
			here[name] = text
		}
	}
	find := func(name string) int {
		return slices.IndexFunc(entries, func(e repo.TreeEntry) bool { return e.Name == name })
	}
	for name, text := range here {
		at := find(name)
		switch {
		case at >= 0 && entries[at].Mode == repo.ModeTree:
			return repo.ID{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is a directory of the parent's tree: a commit writes and removes files, one path each", path.Join(dir, name))}
		case text == nil && at < 0:
			return repo.ID{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is removed, and the parent's tree holds no file there", path.Join(dir, name))}
		case text == nil:
			entries = slices.Delete(entries, at, at+1)
		default:
			blob := w.add(repo.TypeBlob, []byte(*text))
			mode := repo.ModeFile
			if at >= 0 {
				if entries[at].Mode == repo.ModeExecutable {
					mode = repo.ModeExecutable
				}
				entries[at] = repo.TreeEntry{Name: name, Mode: mode, ID: blob}
			} else {
				entries = append(entries, repo.TreeEntry{Name: name, Mode: mode, ID: blob})
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(below)) {
		at := find(name)
		var sub repo.ID
		if at >= 0 {
			if entries[at].Mode != repo.ModeTree {
				return repo.ID{}, &commitRefused{http.StatusUnprocessableEntity, fmt.Sprintf("%s is a file of the parent's tree, and a path below it names it as a directory", path.Join(dir, name))}
			}
			sub = entries[at].ID
		}
		id, err := rewrite(ctx, objects, sub, dir+name+"/", below[name], w)
		if err != nil {
			return repo.ID{}, err
		}
		switch {
		case id.IsZero() && at >= 0:
			entries = slices.Delete(entries, at, at+1)
		case id.IsZero():
		case at >= 0:
			entries[at].ID = id
		default:
			entries = append(entries, repo.TreeEntry{Name: name, Mode: repo.ModeTree, ID: id})
		}
	}
	if len(entries) == 0 && dir != "" {
		return repo.ID{}, nil
	}
	data, err := repo.EncodeTree(entries)
	if err != nil {
		return repo.ID{}, &commitRefused{http.StatusUnprocessableEntity, err.Error()}
	}
	return w.add(repo.TypeTree, data), nil
}
