package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
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
//
// Somewhere to write is the one part the runner could not be told by name, because the key of an
// output is the digest of bytes that do not exist until the container has exited. So it is told at
// the first redemption, beside everything else, as one policy for the task's namespace, and a task
// never comes back at the end to ask where its outputs go, which would read its secrets a second
// time for nothing. The holder may still redeem again, when the answer to its first redemption
// never reached it, or when a message it redeemed comes round to it after a restart because its
// acknowledgement never arrived, and it is answered as the first time was, with fresh URLs.

// Secrets is where a secret value comes from.
//
// One method, because that is the whole of what the API does with a store: read one value, for one
// namespace, at the last moment. There is no write here and no listing, which is not an omission:
// "No role reads a secret value through the API. Rotation is a write, never a read-then-write."
//
// secret.Providers fills it, reading the namespace's declaration of the name and then the store
// the declaration names. It is wired in by secret.Attach rather than built here, for the reason
// Values is: this package is linked by the command line, and what reads a store is not.
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

// Redemption is what a runner presents: the grant, and the task it claims the grant is for.
type Redemption struct {
	Grant string `json:"grant"`

	// TaskID is the task's row, which the grant also names inside its own text, and
	// IdempotencyKey is which attempt and which shard is asking. Both are compared with what
	// the grant was issued for, and "the API refuses a redemption where the two disagree
	// rather than believing either alone".
	TaskID         string     `json:"task_id"`
	IdempotencyKey agk.TaskID `json:"idempotency_key"`
}

func (ask *Redemption) field(b *body, name string) error {
	switch name {
	case "grant":
		return text(b, &ask.Grant)
	case "task_id":
		return text(b, &ask.TaskID)
	case "idempotency_key":
		return text(b, &ask.IdempotencyKey)
	}
	return unknown(name)
}

func (s *RunnerAPI) redeem(w http.ResponseWriter, r *http.Request, runner Runner) {
	if s.objects == nil || s.urls == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached")
		return
	}
	var ask Redemption
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	// Both are required rather than checked when present, because a comparison with nothing
	// is no comparison: a request that left one out would be believed on the other alone.
	switch {
	case ask.TaskID == "":
		fail(w, http.StatusBadRequest, "a redemption names the task it is for, and this one has no task_id")
		return
	case ask.IdempotencyKey == "":
		fail(w, http.StatusBadRequest, "a redemption names the attempt that is asking, and this one has no idempotency_key")
		return
	}
	// The row is held against the task the grant names inside its own text before anything
	// is read, so that a body disagreeing with its grant has one refusal whatever the task is
	// doing. Compared once the task was read, a wrong task_id would be told the work is
	// somebody else's where a wrong idempotency_key is told nothing, and the two halves of
	// one rule would answer differently.
	if row, named := token.TaskOf(ask.Grant); !named || row != ask.TaskID {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that grant cannot be redeemed")
		return
	}

	// Checked, answered, and only then bound, each in that order for a reason.
	//
	// The grant is checked first, and the version its scope names read in the same
	// transaction, so that a grant that would not redeem reads nothing else.
	var got db.Redeemed
	var tree []db.TreeFile
	err := s.pool.Installation(r.Context(), db.Redemption, func(ctx context.Context, wide *db.Wide) error {
		var err error
		got, err = wide.Redeemable(ctx, ask.Grant, ask.IdempotencyKey, runner.ID, s.now())
		if err != nil {
			return err
		}
		// And again with the row the grant was found by, so that should the two ever
		// come apart the request is refused as a grant that opens nothing.
		if got.Row != ask.TaskID {
			return db.ErrNoGrant
		}
		if got.Scope.Workflow == "" || got.Scope.Commit == "" {
			return errNoCommit
		}
		tree, err = wide.Tree(ctx, got.Namespace, got.Scope.Workflow, got.Scope.Commit)
		return err
	})
	if err != nil {
		refuseRedemption(w, err)
		return
	}

	// Then the answer, the secret values last: "the runner obtains the value at the last moment",
	// and the last moment is once everything else the task is given is ready, so that a redemption
	// refused for anything else never reads one. It comes before the image is pulled, though,
	// which is a cost accepted with the order a runner takes a task in: it redeems before it
	// acknowledges the message, so that the redemption decides who runs the task, and pulls only
	// after. They are read outside any transaction. A store reads a declaration, and the built-in
	// one its value, on connections of its own, and does not always answer from this database at
	// all: a transaction held open around the read would hold the task's row for as long as the
	// store takes, and hold a connection while waiting for another, which enough redemptions at
	// once turn into every connection held and none to be had.
	answer, err := s.whatTheGrantIsFor(r.Context(), got, tree)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// And the task is bound once there is an answer to give. A redemption that cannot answer
	// refuses and binds nothing, as a missing tree always did: a runner told there is no tree, or
	// no secret, has not taken a task it cannot run. It reports that no container ran, which ends
	// the dispatch and binds it in one write (db.Wide.BindUnredeemed), and acknowledges the
	// message only once the report is published (bus.Taken.Refused). So a runner that dies between
	// the refusal and its report has acknowledged nothing, and the next runner of the pool is
	// handed the message once the consumer's AckWait has passed, to be refused the same way, or to
	// find the dispatch over. Bound here, a runner that died there would have left a task bound to
	// it and run by nobody, for the heartbeat's sweep to find lost. Redeem checks again under the
	// row's lock, so a task another runner bound in between is refused here and the values read
	// for it go nowhere.
	err = s.pool.Installation(r.Context(), db.Redemption, func(ctx context.Context, wide *db.Wide) error {
		bound, err := wide.Redeem(ctx, ask.Grant, ask.IdempotencyKey, runner.ID, s.now())
		if err != nil {
			return err
		}
		if bound.Row != ask.TaskID {
			return db.ErrNoGrant
		}
		return nil
	})
	if err != nil {
		refuseRedemption(w, err)
		return
	}
	// Not cached anywhere, by anything: what is in it is every value the task was given.
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, answer)
}

// refuseRedemption answers a redemption the grant, the task or the version would not allow, the
// same way whether the check refused it or the binding did.
func refuseRedemption(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNoGrant):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that grant cannot be redeemed")
	case errors.Is(err, db.ErrTaskHeld):
		// Not a refusal of the credential: the grant was real and the task is another runner's, or
		// over, a lost dispatch among them. A runner that gets this acknowledges the message and
		// starts nothing (bus.Taken.Refused): the holder answers for the task, the heartbeat's
		// sweep for a holder gone quiet, and a lost task's requeue goes out as a message of its
		// own. Asking again gets the same answer, and a message put back goes to the next runner
		// of the pool to be refused in its turn.
		fail(w, http.StatusConflict, "that task is not this runner's to work on")
	case errors.Is(err, db.ErrRunnerNotTaking):
		// "A redemption by a draining or revoked runner gets 403, binds nothing, and the runner
		// puts the message back with Again": the runner's standing and not the task's, which is
		// nobody's yet and should go to another runner of the pool. It is refused before any
		// secret is read. A task the runner already holds is not refused, since finishing what
		// it holds is what a drain and a grace leave it to do.
		fail(w, http.StatusForbidden, "this runner is draining or revoked and takes no new task: put the message back for another runner of the pool")
	case errors.Is(err, errNoCommit):
		// This and the two below are the installation's rather than the runner's: the
		// grant was real and what it was written with cannot be answered. Each says so
		// rather than handing over an empty /agk/repo, which would start a step on a
		// directory that looks like a repository and is not one.
		fail(w, http.StatusInternalServerError, "this task was dispatched without the commit its run pinned, so there is no repository to give it")
	case errors.Is(err, db.ErrNoTree):
		fail(w, http.StatusInternalServerError, "the version this task runs was recorded without its tree, so there is nothing to lay out at /agk/repo")
	case errors.Is(err, db.ErrNoVersion):
		fail(w, http.StatusInternalServerError, "the version this task runs is not recorded, so there is nothing to lay out at /agk/repo")
	default:
		fail(w, http.StatusInternalServerError, "the grant could not be redeemed")
	}
}

// Input is one input port: where its envelope is fetched from, and every artifact that envelope
// names, given together so that the runner never has to come back for what it finds by reading.
//
// Each port carries the artifacts of its own envelope, the same bytes under each port that names
// them, because the runner pairs an entry with the file entry it read by the URI, and the URI is
// the envelope's own.
type Input struct {
	Port      agk.Port   `json:"port"`
	Envelope  Envelope   `json:"envelope"`
	Artifacts []Artifact `json:"artifacts"`
}

// Envelope is where one port's batch is fetched from, and the digest it has to hash to, written
// as the task message writes it so that the runner can hold one against the other.
type Envelope struct {
	URL    string `json:"url"`
	Digest string `json:"digest"`
}

// Artifact is one file an envelope names, by the logical URI the envelope writes it with, and the
// URL that fetches its bytes. The URI is the name; the URL beside it is a credential.
type Artifact struct {
	URI    agk.URI `json:"uri"`
	SHA256 string  `json:"sha256"`
	URL    string  `json:"url"`
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

// Secret is a name, the path the value goes at, and the value, which exists in this answer and
// nowhere else on the way to the machine that will mount it.
type Secret struct {
	Name     string `json:"name"`
	Mount    string `json:"mount"`
	Encoding string `json:"encoding"`
	Value    string `json:"value"`
}

// Uploads is where the runner writes everything the task makes, its output envelopes and its
// artifacts alike: a form posted to URL with Fields as they are given, a key that is KeyPrefix
// followed by the digest of the bytes, and the file.
//
// It is one signed policy for the task and not a URL per object, because no digest exists until
// the container has exited. The policy is bounded instead by the namespace's prefix, the run and
// the grant's expiry, and the built-in store holds each object to the digest its key names.
type Uploads struct {
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	KeyPrefix string            `json:"key_prefix"`
}

// The two ways a value is written, said on every entry rather than left to a default: "a reader
// that had to guess would guess wrong exactly once, on the day a value stops being text".
const (
	EncodingUTF8   = "utf-8"
	EncodingBase64 = "base64"
)

// Grant is the whole of what one grant is for.
type Grant struct {
	// TaskID is the row the grant was issued for, echoed back, because a runner holding
	// several tasks has to know which container a document of secret values belongs to.
	TaskID    string `json:"task_id"`
	ExpiresAt string `json:"expires_at"`

	Inputs  []Input  `json:"inputs"`
	Secrets []Secret `json:"secrets"`

	// Tree is the whole of the commit's tree, which is what a step that says nothing about
	// files is given. Narrowing it by a step's files is the controller's to add, and until it
	// does every task of a version is handed the same list.
	Tree []TreeEntry `json:"tree"`

	Uploads Uploads `json:"uploads"`
}

// errNoCommit is a scope naming no version, which is a scope written by a controller from before
// a scope named one.
var errNoCommit = errors.New("api: the grant's scope names no commit")

// whatTheGrantIsFor turns the names the controller wrote into values, and refuses to go beyond
// them. Every URL here is minted for one object, the policy for the namespace's prefix, and each
// ends with the grant, so nothing the runner holds outlives the task it was given for.
func (s *RunnerAPI) whatTheGrantIsFor(ctx context.Context, got db.Redeemed, tree []db.TreeFile) (Grant, error) {
	out := Grant{
		TaskID:    got.Row,
		ExpiresAt: got.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Inputs:    []Input{}, Secrets: []Secret{}, Tree: []TreeEntry{},
	}

	// One URL per object, whoever names it: two files with the same bytes are one object,
	// and a runner that holds a digest already writes it from its own cache and fetches
	// nothing.
	minted := map[string]string{}
	fetch := func(digest string) (string, error) {
		if url, held := minted[digest]; held {
			return url, nil
		}
		url, err := s.urls.Presign(ctx, http.MethodGet, artifact.Key(got.Namespace, digest), got.Scope.Run, got.ExpiresAt)
		if err != nil {
			return "", err
		}
		minted[digest] = url
		return url, nil
	}

	// The envelopes on this task's input ports, and then the artifacts those envelopes
	// name. The API resolves them rather than letting the runner ask for a digest of its
	// own, because a runner that could name what it wanted would reach every object in the
	// namespace and "refusing anything the task does not name" would mean nothing.
	for _, in := range got.Scope.Inputs {
		url, err := fetch(in.Digest)
		if err != nil {
			return Grant{}, err
		}
		port := Input{
			Port:      in.Port,
			Envelope:  Envelope{URL: url, Digest: "sha256:" + in.Digest},
			Artifacts: []Artifact{},
		}

		envelope, err := artifact.GetEnvelope(ctx, s.objects, got.Namespace, in.Digest, s.limits)
		if err != nil {
			return Grant{}, err
		}
		listed := map[Artifact]bool{}
		for _, item := range envelope.Items {
			for _, f := range item.Files {
				a := Artifact{URI: f.URI, SHA256: f.SHA256}
				if f.SHA256 == "" || listed[a] {
					continue
				}
				listed[a] = true
				if a.URL, err = fetch(f.SHA256); err != nil {
					return Grant{}, err
				}
				port.Artifacts = append(port.Artifacts, a)
			}
		}
		out.Inputs = append(out.Inputs, port)
	}

	// The files of the version the scope names, each minted exactly as an artifact's URL is.
	for _, f := range tree {
		url, err := fetch(f.SHA256)
		if err != nil {
			return Grant{}, err
		}
		out.Tree = append(out.Tree, TreeEntry{Path: f.Path, Mode: f.Mode, SHA256: f.SHA256, URL: url})
	}

	// Signed for the namespace the grant was issued in, never one the runner names, and expiring
	// with the grant, so that what it may write is bounded by what it was given to run.
	policy, err := s.urls.Policy(ctx, got.Namespace, got.Scope.Run, got.ExpiresAt)
	if err != nil {
		return Grant{}, err
	}
	out.Uploads = Uploads{URL: policy.URL, Fields: policy.Fields, KeyPrefix: policy.KeyPrefix}

	// And the values last, once nothing else can refuse, each read from the store at every
	// redemption and kept by nothing on the way. That answers asking again with whatever the
	// store holds by then, which is not settled: the documentation says asking again after a
	// lost answer gets the same answer while the grant lives, and a runner adopting its
	// container redeems again for the values its masker matches.
	for _, secret := range got.Scope.Secrets {
		value, err := s.secrets.Value(ctx, got.Namespace, secret.Name)
		if err != nil {
			// Named by the step and not given is a task that cannot run, and saying so is
			// better than mounting an empty file and failing three layers away from the
			// cause. The runner is told which secret and whether the store holds one; why
			// is the store's to say, to whoever runs the installation, since they are the
			// one who can put a key back on the ring or set a variable.
			s.report(fmt.Errorf("api: task %s was not given %s/%s: %w", got.Row, got.Namespace, secret.Name, err))
			if errors.Is(err, ErrNoSecret) {
				return Grant{}, errors.New("the secret " + secret.Name + " is not held for this namespace")
			}
			return Grant{}, errors.New("the secret " + secret.Name + " is held for this namespace and could not be read from its store")
		}
		out.Secrets = append(out.Secrets, secretOf(secret, value))
	}
	return out, nil
}

// secretOf writes one value for the wire.
//
// A secret is a file's worth of bytes and not always text: a PEM key is UTF-8 and a keystore is
// not. A JSON string cannot carry bytes that are not UTF-8, and encoding one anyway replaces each
// of them with U+FFFD, so a keystore sent as a string arrives as a different file. Text travels as
// itself and anything else as base64, and the entry says which.
func secretOf(secret db.GrantSecret, value []byte) Secret {
	out := Secret{Name: secret.Name, Mount: secret.Mount, Encoding: EncodingUTF8, Value: string(value)}
	if !utf8.Valid(value) {
		out.Encoding, out.Value = EncodingBase64, base64.StdEncoding.EncodeToString(value)
	}
	return out
}
