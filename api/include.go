package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing/fstest"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// A workflow include, resolved where a version is judged.
//
// "A workflow include must carry ref, a tag or a commit, and needs workflow:read on that
// repository. The run records the commit it resolved to. Unpinned, another repository could change
// what this commit does." And it "reads the other repository's root agentiik.yaml, written as a
// fragment: a library repository, which its own hook validates as a fragment and nothing runs".
//
// So what an include reads is a version of a library, as that library's hook accepted it: a ref of
// a library names a version as every ref does, since its hook judged every commit a ref of it was
// moved to, and the files resolution reads are the ones its version kept of it, its root file and
// what that includes of its own tree. The pack is never walked: a commit no ref of the library ever
// named, one between two tips, was never judged as a fragment, and an include of it would be the
// first to read it as one.

// abbreviatedCommit is a commit as git abbreviates one, which an include is refused for rather
// than resolved: see includedCommit.
var abbreviatedCommit = regexp.MustCompile(`^[0-9a-f]{7,39}$`)

// includeOf resolves the workflow includes of what the request r carries: the library's version at
// the commit the ref names, under r's caller's workflow:read on it.
//
// A workflow the caller may not read and one that does not exist are refused in one sentence, so
// that an include is no way of learning which workflows another namespace holds.
func (s *Server) includeOf(r *http.Request) func(context.Context, graph.WorkflowRef) (fs.FS, string, error) {
	return func(ctx context.Context, ref graph.WorkflowRef) (fs.FS, string, error) {
		over := Target{Namespace: ref.Namespace, Workflow: ref.Name}
		absent := fmt.Errorf("%s/%s is no workflow you may read: a workflow include needs workflow:read on the repository it names, so that an include cannot widen what its author may see", ref.Namespace, ref.Name)
		reads, err := HoldsToInclude(r)(ctx, over)
		if err != nil {
			return nil, "", &pushFault{"the include could not be authorised", err}
		}
		if !reads {
			return nil, "", absent
		}
		var v db.Version
		var refused string
		err = s.pool.In(ctx, ref.Namespace, func(ctx context.Context, ns *db.NS) error {
			if _, err := ns.WorkflowRecord(ctx, ref.Name); err != nil {
				return err
			}
			var commit string
			var err error
			if commit, refused, err = includedCommit(ctx, ns, ref); err != nil || refused != "" {
				return err
			}
			v, err = ns.Version(ctx, ref.Name, commit)
			return err
		})
		switch {
		case errors.Is(err, db.ErrNoWorkflow):
			return nil, "", absent
		case err != nil:
			return nil, "", &pushFault{"the workflow an include names could not be read", err}
		case refused != "":
			return nil, "", errors.New(refused)
		case !v.Library:
			return nil, "", fmt.Errorf("%s/%s@%s is a workflow, and a workflow include reads a library: a repository whose root agentiik.yaml is written as a fragment, with no apiVersion, kind or metadata, which other workflows include and nothing runs", ref.Namespace, ref.Name, v.Commit[:12])
		}
		tree := fstest.MapFS{v.Entry: &fstest.MapFile{Data: v.Document, Mode: 0o444}}
		for path, body := range v.Includes {
			tree[path] = &fstest.MapFile{Data: body, Mode: 0o444}
		}
		return tree, v.Commit, nil
	}
}

// includedCommit is the commit a workflow include's ref names in the library's repository: a tag,
// by its name or in full, or a commit of one of its versions, written whole. A branch is refused,
// since it moves, and so is a ref naming nothing. It answers why a ref is refused apart from a
// failure to read, which is the installation's.
//
// A commit abbreviated is refused too, as the tree push refuses one: an abbreviation is a name
// another commit of the library can come to share, and the file would then name two, which the
// next installation judging it could not tell apart. A tag is looked for first, as git looks for
// one, so a tag named like a commit's first characters is the tag.
func includedCommit(ctx context.Context, ns *db.NS, ref graph.WorkflowRef) (commit, refused string, err error) {
	named := ref.Namespace + "/" + ref.Name
	repository, err := ns.Repository(ctx, ref.Name)
	if err != nil {
		return "", "", err
	}
	tag, branch := "refs/tags/"+ref.Ref, "refs/heads/"+ref.Ref
	switch {
	case strings.HasPrefix(ref.Ref, "refs/tags/"):
		tag, branch = ref.Ref, ""
	case strings.HasPrefix(ref.Ref, "refs/"):
		tag, branch = "", ref.Ref
	}
	isBranch := false
	for _, r := range repository.Refs {
		switch {
		case r.Name == tag && r.Commit != "":
			return r.Commit, "", nil
		case r.Name == branch && r.Commit != "":
			isBranch = true
		}
	}
	switch {
	case wholeCommit.MatchString(ref.Ref):
		if _, err := ns.Version(ctx, ref.Name, ref.Ref); err == nil {
			return ref.Ref, "", nil
		} else if !errors.Is(err, db.ErrNoVersion) {
			return "", "", err
		}
	case abbreviatedCommit.MatchString(ref.Ref) && !isBranch:
		return "", fmt.Sprintf("%s is a commit abbreviated, or no tag of %s, and a workflow include names a commit whole: an abbreviation is a name another commit of the library can come to share, and this file would then name two", ref.Ref, named), nil
	}
	if isBranch {
		return "", fmt.Sprintf("%s names a branch of %s, and a workflow include is pinned to a tag or a commit: a branch moves, and another repository could change what this commit does without this commit changing", ref.Ref, named), nil
	}
	return "", fmt.Sprintf("%s names no tag of %s and no commit a ref of it named: a workflow include reads a library at a commit its hook judged, which is every commit a branch or a tag of it was pushed to", ref.Ref, named), nil
}
