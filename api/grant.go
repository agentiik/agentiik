package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// What a grant turns into.
//
// "The runner obtains the value at the last moment, by redeeming at the API the per-task grant the
// controller issued for that one task and that one secret. The API is the only component that
// reads the store; the controller names which secret a task may have and never sees its value."
//
// A task message "carries no business payload, no secret value and no URL that would work without
// the grant", which is what makes a copy of one worth nothing on its own. This is the other half:
// the one route that turns those names into values, and it answers from what the controller wrote
// beside the grant and from nothing else.
//
// The repository is one of those values. "A runner still never speaks git and never holds a
// credential, because the controller resolves a commit to a tree and the runner fetches
// content-addressed objects with the task's grant, exactly as it fetches an artifact." So the
// commit is in the scope the controller wrote, the files of that commit are named here with a URL
// each, and the runner can reach the tree of the one version its task runs and of no other.

// Secrets is where a secret value comes from.
//
// One method, because that is the whole of what the API does with a store: read one value, for one
// namespace, at the last moment. There is no write here and no listing, which is not an omission:
// "No role reads a secret value through the API. Rotation is a write, never a read-then-write."
type Secrets interface {
	Value(ctx context.Context, namespace, name string) ([]byte, error)
}

// ErrNoSecret is a secret the provider does not hold.
var ErrNoSecret = errors.New("api: no secret of that name in that namespace")

// NoSecrets is an installation with no provider configured, and it holds nothing.
//
// It is the default rather than something to remember to replace, for the reason DenyAll is: a
// task naming a secret then fails in front of somebody, rather than starting a container whose
// secret file is empty and whose failure is three layers away from its cause.
type NoSecrets struct{}

// Value holds nothing.
func (NoSecrets) Value(context.Context, string, string) ([]byte, error) { return nil, ErrNoSecret }

// Redemption is what a runner presents.
type Redemption struct {
	Grant string     `json:"grant"`
	Task  agk.TaskID `json:"task"`

	// Upload names the digests the runner has computed and wants somewhere to put. It is
	// empty at the start of a task, when the runner is asking what to fetch, and full at
	// the end, when it knows what it produced. A PUT URL cannot be minted in advance
	// because the key of an object is the digest of bytes that do not exist yet.
	Upload []string `json:"upload,omitempty"`
}

func (s *RunnerAPI) redeem(w http.ResponseWriter, r *http.Request, runner Runner) {
	if s.objects == nil || s.urls == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached")
		return
	}
	var ask Redemption
	if err := read(r, &ask); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var got db.Redeemed
	var tree []db.TreeFile
	err := s.pool.Installation(r.Context(), db.Redemption, func(ctx context.Context, wide *db.Wide) error {
		var err error
		got, err = wide.Redeem(ctx, ask.Grant, ask.Task, runner.ID, s.now())
		if err != nil {
			return err
		}
		// The version the scope names and no other, read in the same transaction, so a
		// refusal here also leaves the task unbound: a runner told there is no tree has
		// not taken a task it cannot run.
		if got.Scope.Workflow == "" || got.Scope.Commit == "" {
			return errNoCommit
		}
		tree, err = wide.Tree(ctx, got.Namespace, got.Scope.Workflow, got.Scope.Commit)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoGrant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that grant cannot be redeemed")
		return
	case errors.Is(err, db.ErrTaskHeld):
		// Not a refusal of the credential: the grant was real and the task is somebody
		// else's or already over. A runner that gets this stops rather than retrying.
		fail(w, http.StatusConflict, "that task is not this runner's to work on")
		return
	case errors.Is(err, errNoCommit):
		// This and the two below are the installation's rather than the runner's: the
		// grant was real and what it was written with cannot be answered. Each says so
		// rather than handing over an empty /agk/repo, which would start a step on a
		// directory that looks like a repository and is not one.
		fail(w, http.StatusInternalServerError, "this task was dispatched without the commit its run pinned, so there is no repository to give it")
		return
	case errors.Is(err, db.ErrNoTree):
		fail(w, http.StatusInternalServerError, "the version this task runs was recorded without its tree, so there is nothing to lay out at /agk/repo")
		return
	case errors.Is(err, db.ErrNoVersion):
		fail(w, http.StatusInternalServerError, "the version this task runs is not recorded, so there is nothing to lay out at /agk/repo")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the grant could not be redeemed")
		return
	}

	answer, err := s.whatTheGrantIsFor(r.Context(), got, tree, ask.Upload)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Not cached anywhere, by anything: what is in it is every value the task was given.
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, answer)
}

// Fetch is one object and the URL that fetches it.
type Fetch struct {
	Port   agk.Port `json:"port,omitempty"`
	Digest string   `json:"digest"`
	Items  int      `json:"items,omitempty"`
	URL    string   `json:"url"`
}

// TreeEntry is one file of the repository, where it goes under /agk/repo, and the URL that fetches
// it.
//
// The shape is the wire's, entry for entry. It carries no to, the relocation the long form of a
// step's files asks for, because narrowing and relocating are not served yet and every file here
// goes where its path says.
type TreeEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// Secret is a name and its value, which exists in this answer and nowhere else on the way to the
// machine that will mount it.
type Secret struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Grant is the whole of what one grant is for.
type Grant struct {
	Task      agk.TaskID `json:"task"`
	Namespace string     `json:"namespace"`
	Run       agk.RunID  `json:"run"`
	Step      agk.Step   `json:"step"`
	ExpiresAt string     `json:"expires_at"`

	Inputs    []Fetch  `json:"inputs"`
	Artifacts []Fetch  `json:"artifacts"`
	Secrets   []Secret `json:"secrets"`

	// Tree is the whole of the commit's tree, which is what a step that says nothing about
	// files is given. Narrowing it by a step's files is the controller's to add, and until it
	// does every task of a version is handed the same list.
	Tree []TreeEntry `json:"tree"`

	Uploads []Fetch `json:"uploads,omitempty"`
}

// errNoCommit is a scope naming no version, which is a scope written by a controller from before
// a scope named one.
var errNoCommit = errors.New("api: the grant's scope names no commit")

// whatTheGrantIsFor turns the names the controller wrote into values, and refuses to go beyond
// them. Every URL here is minted for one object and ends with the grant, so nothing the runner
// holds outlives the task it was given for.
func (s *RunnerAPI) whatTheGrantIsFor(ctx context.Context, got db.Redeemed, tree []db.TreeFile, upload []string) (Grant, error) {
	out := Grant{
		Task: got.Task, Namespace: got.Namespace,
		Run: got.Scope.Run, Step: got.Scope.Step,
		ExpiresAt: got.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Inputs:    []Fetch{}, Artifacts: []Fetch{}, Secrets: []Secret{}, Tree: []TreeEntry{},
	}

	// The envelopes on this task's input ports, and then the artifacts those envelopes
	// name. The API resolves them rather than letting the runner ask for a digest of its
	// own, because a runner that could name what it wanted would reach every object in the
	// namespace and "refusing anything the task does not name" would mean nothing.
	seen := map[string]bool{}
	for _, in := range got.Scope.Inputs {
		url, err := s.urls.Presign(ctx, http.MethodGet, artifact.Key(got.Namespace, in.Digest), got.Scope.Run, got.ExpiresAt)
		if err != nil {
			return Grant{}, err
		}
		out.Inputs = append(out.Inputs, Fetch{Port: in.Port, Digest: in.Digest, Items: in.Items, URL: url})

		envelope, err := artifact.GetEnvelope(ctx, s.objects, got.Namespace, in.Digest, s.limits)
		if err != nil {
			return Grant{}, err
		}
		for _, item := range envelope.Items {
			for _, f := range item.Files {
				if f.SHA256 == "" || seen[f.SHA256] {
					continue
				}
				seen[f.SHA256] = true
				url, err := s.urls.Presign(ctx, http.MethodGet, artifact.Key(got.Namespace, f.SHA256), got.Scope.Run, got.ExpiresAt)
				if err != nil {
					return Grant{}, err
				}
				out.Artifacts = append(out.Artifacts, Fetch{Digest: f.SHA256, URL: url})
			}
		}
	}

	// The files of the version the scope names, each minted exactly as an artifact's URL is,
	// and one URL per blob: two files with the same bytes are one object, and a runner that
	// holds a digest already writes it from its own cache and fetches nothing.
	minted := map[string]string{}
	for _, f := range tree {
		url, held := minted[f.SHA256]
		if !held {
			var err error
			url, err = s.urls.Presign(ctx, http.MethodGet, artifact.Key(got.Namespace, f.SHA256), got.Scope.Run, got.ExpiresAt)
			if err != nil {
				return Grant{}, err
			}
			minted[f.SHA256] = url
		}
		out.Tree = append(out.Tree, TreeEntry{Path: f.Path, Mode: f.Mode, SHA256: f.SHA256, URL: url})
	}

	for _, name := range got.Scope.Secrets {
		value, err := s.secrets.Value(ctx, got.Namespace, name)
		if err != nil {
			// Named by the step and missing from the store is a task that cannot run,
			// and saying so is better than mounting an empty file and failing three
			// layers away from the cause.
			return Grant{}, errors.New("the secret " + name + " is not held for this namespace")
		}
		out.Secrets = append(out.Secrets, Secret{Name: name, Value: string(value)})
	}

	for _, digest := range upload {
		key := artifact.Key(got.Namespace, digest)
		url, err := s.urls.Presign(ctx, http.MethodPut, key, got.Scope.Run, got.ExpiresAt)
		if err != nil {
			return Grant{}, err
		}
		out.Uploads = append(out.Uploads, Fetch{Digest: digest, URL: url})
	}
	return out, nil
}
