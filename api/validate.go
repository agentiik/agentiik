package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/repo"
	"github.com/agentiik/agentiik/version"
)

// Validating a draft: POST /api/v1/{namespace}/workflows/{workflow}/validate.
//
// "Judges files as the version they would make, and commits nothing." The files are laid over the
// tree of a ref as POST .../commits lays them over its parent, and the tree is judged by the hook's
// own check, version.Check, with what the hook reaches: the repository's pins and manifests, the
// libraries an include names and, for a caller holding workflow:read on the namespace, the
// namespace's secret declarations. secret:use is not asked, "since it is the committer's and is
// asked when the commit is made". Nor are the declarations asked for anybody else, since "a grant
// on one workflow shows none" of them, and a refusal naming the secrets a draft writes that the
// namespace does not declare would tell which it does: such a draft is judged as if each were
// declared, and the push holds the commit to them. It is workflow.validate, "the only authority on
// whether a draft is legal", so that a client asks the rules a push is held to rather than a copy
// of them.

// validateRequest is what the route reads: "{ref, files}".
type validateRequest struct {
	Ref   string
	files []committedFile
	bytes int
}

func (v *validateRequest) field(b *body, name string) error {
	switch name {
	case "ref":
		return text(b, &v.Ref)
	case "files":
		return filesOf(b, &v.files, &v.bytes)
	}
	return fmt.Errorf("the request body names %.64q, and a validation takes ref and files", name)
}

// Validated is what the route answers for a draft the hook would accept: what the version would
// hold.
type Validated struct {
	Valid   bool `json:"valid"`
	Steps   int  `json:"steps"`
	Inputs  int  `json:"inputs"`
	Outputs int  `json:"outputs"`
}

func (s *Server) validateFiles(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var v validateRequest
	if err := readAtMost(r, &v, pushMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	validated, err := s.validate(r, over, v)
	if err != nil {
		s.answerCommit(w, err)
		return
	}
	write(w, http.StatusOK, validated)
}

// validate lays the files over the tree of the ref and judges what that makes.
func (s *Server) validate(r *http.Request, over Target, v validateRequest) (Validated, error) {
	ctx := r.Context()
	if len(v.files) == 0 {
		return Validated{}, &commitRefused{http.StatusUnprocessableEntity, "a validation lays at least one file over the tree: files maps each path to its text, or to null to leave it out"}
	}
	if err := checkFiles(v.files); err != nil {
		return Validated{}, err
	}
	if s.packs == nil || s.objects == nil {
		return Validated{}, &commitRefused{http.StatusServiceUnavailable, noPacks}
	}
	repository, err := s.repositoryOf(ctx, over)
	if errors.Is(err, db.ErrNoWorkflow) {
		return Validated{}, &commitRefused{http.StatusNotFound, notFound}
	}
	if err != nil {
		return Validated{}, err
	}
	objects, err := s.objectsOf(repository)
	if err != nil {
		return Validated{}, err
	}
	defer objects.Close()

	// The ref the files are laid over: the one named, or the default branch's head, or nothing in a
	// repository with no commit yet.
	commit := ""
	if v.Ref != "" {
		var status int
		err = s.pool.In(ctx, over.Namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			commit, status, err = resolveRef(ctx, ns, over.Workflow, v.Ref)
			return err
		})
		if err != nil && status != http.StatusOK {
			return Validated{}, &commitRefused{status, err.Error()}
		}
		if err != nil {
			return Validated{}, err
		}
	} else {
		for _, rf := range repository.Refs {
			if rf.Name == "refs/heads/"+repository.DefaultBranch {
				commit = rf.Commit
			}
		}
	}
	var base repo.ID
	if commit != "" {
		id, err := repo.ParseID(commit)
		if err != nil {
			return Validated{}, err
		}
		_, data, err := repo.ReadObject(ctx, objects, id, repo.MaxParsedBytes)
		if err != nil {
			return Validated{}, err
		}
		c, err := repo.ParseCommit(data)
		if err != nil {
			return Validated{}, err
		}
		base = c.Tree
	}

	written := &objectsWritten{}
	root, err := rewrite(ctx, objects, base, v.files, written)
	if err != nil {
		return Validated{}, err
	}
	former, err := formerNames(ctx, s.pool, over.Namespace)
	if err != nil {
		return Validated{}, &pushFault{"the names the namespace held could not be read", err}
	}
	resolvers := s.resolvers(r, over)
	resolvers.SecretUse = nil
	// The declarations are shown to a reader of the namespace alone, as secret.list shows them, and
	// a refusal listing the names a draft writes that are not declared would show the rest to
	// anybody guessing; the push asks secret:use, a namespace's grant alone, before it checks them.
	declarations, err := Revealing(r)(ctx, Target{Namespace: over.Namespace})
	if err != nil {
		return Validated{}, &pushFault{"the request could not be authorised", err}
	}
	if !declarations {
		resolvers.Secrets = nil
	}
	tree := repo.NewTreeFS(ctx, held{written: written, under: objects}, root)
	checked, err := version.Check(ctx, tree, version.Checking{
		Committed: true, Namespace: over.Namespace, FormerNamespaces: former, Repository: over.Workflow,
		Resolvers: resolvers,
	})
	if err != nil {
		if problem, ok := version.Explain(err); ok {
			return Validated{}, &pushRefusal{short: "refused: " + problem.Rule, problem: &problem}
		}
		var fault *pushFault
		if errors.As(err, &fault) {
			return Validated{}, err
		}
		return Validated{}, &commitRefused{http.StatusUnprocessableEntity, err.Error()}
	}
	if checked.Library {
		return Validated{Valid: true}, nil
	}
	wf := checked.Workflow
	return Validated{Valid: true, Steps: len(wf.Steps), Inputs: len(wf.Inputs), Outputs: len(wf.Outputs)}, nil
}

// held is the objects a validation wrote over those the repository holds, which a tree is read out
// of without either being stored.
type held struct {
	written *objectsWritten
	under   repo.Lookup
}

func (h held) OpenObject(ctx context.Context, id repo.ID) (repo.ObjectReader, error) {
	if i, ok := h.written.at[id]; ok {
		o := h.written.objects[i]
		return &heldObject{Reader: bytes.NewReader(o.data), t: o.t, size: int64(len(o.data))}, nil
	}
	return h.under.OpenObject(ctx, id)
}

// heldObject is one object a validation wrote, read from memory.
type heldObject struct {
	*bytes.Reader
	t    repo.Type
	size int64
}

func (o *heldObject) Type() repo.Type { return o.t }
func (o *heldObject) Size() int64     { return o.size }
func (o *heldObject) Close() error    { return nil }

var _ io.ReadCloser = (*heldObject)(nil)
