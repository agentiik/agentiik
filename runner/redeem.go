package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
)

// Redeeming a task's grant.
//
// "A runner redeems before it acknowledges the message and pulls the image." The redemption is
// the one exchange that turns the names a task message carries into what it deliberately does
// not: a URL for each input envelope and each artifact it names, a URL for each file of the
// tree, the secret values and one upload policy. The first answer binds the task to this runner,
// so what the runner does next depends on which answer it got, and the page's table sets that out
// status by status; NextAfter is that table.
//
// The types are this package's own, held to wire.schema.json's grantRedemption rather than to
// package api's, since a runner links no API: cmd/agk-runner/boundary_test.go refuses it.

// redeemPath is the route, as the page and the router spell it.
const redeemPath = "/api/v1/tasks/redeem"

// redemptionRequest is grantRedemption.request: the grant, and the task it is claimed for, named
// twice, by its row and by its key, since the API refuses a redemption where the two disagree.
type redemptionRequest struct {
	Grant          string `json:"grant"`
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Redemption is grantRedemption.response: everything the task message named and did not carry.
//
// Every URL in it is a bearer credential and every value a secret, so nothing in this package
// prints one: a refusal names a port, a path, a digest or a secret's name, and never what it
// was given for it.
type Redemption struct {
	// TaskID echoes the row the grant was issued for, since a runner holding several tasks
	// has to know which one a document of secret values belongs to.
	TaskID    string `json:"task_id"`
	ExpiresAt string `json:"expires_at"`

	Inputs  []RedeemedInput  `json:"inputs"`
	Secrets []RedeemedSecret `json:"secrets"`
	Tree    []TreeEntry      `json:"tree"`
	Uploads Uploads          `json:"uploads"`
}

// RedeemedInput is one input port: where its envelope is fetched from, and every artifact that
// envelope names.
type RedeemedInput struct {
	Port      string             `json:"port"`
	Envelope  RedeemedEnvelope   `json:"envelope"`
	Artifacts []RedeemedArtifact `json:"artifacts"`
}

// RedeemedEnvelope is where one port's envelope is fetched from, and the digest it hashes to,
// written as the task message writes it.
type RedeemedEnvelope struct {
	URL    string `json:"url"`
	Digest string `json:"digest"`
}

// RedeemedArtifact is one file an input envelope names, by the URI the envelope writes it with,
// and the URL its bytes are fetched through.
type RedeemedArtifact struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// TreeEntry is one file of the workflow repository: where it goes under /agk/repo, the mode it
// is created with, its digest and the URL it is fetched through.
//
// To is the wire's relocation, which the API does not send yet. It is read rather than refused,
// since the wire allows it, and held to the step's own files, which is what the driver binds a
// relocation from.
type TreeEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
	To     string `json:"to,omitempty"`
}

// RedeemedSecret is one value, the name the task message gave it, where it is mounted, and how
// the value is written in this document.
type RedeemedSecret struct {
	Name     string `json:"name"`
	Mount    string `json:"mount"`
	Encoding string `json:"encoding"`
	Value    string `json:"value"`
}

// Uploads is the task's one upload policy: where the form is posted, the signed fields it is
// posted with, and the prefix every key it writes starts with.
type Uploads struct {
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	KeyPrefix string            `json:"key_prefix"`
}

// The two encodings a value travels in, spelled as the wire spells them.
const (
	encodingUTF8   = "utf-8"
	encodingBase64 = "base64"
)

// ErrAnswerUnusable is a redemption that answered 200, and so bound the task to this runner, with
// a document this runner cannot run the task on: one it cannot read, or one that does not answer
// the task message it holds. Every runner of an installation carries one version, so no runner
// could do better with it, and asking again is answered the same way until the deadline.
var ErrAnswerUnusable = errors.New("runner: the redemption bound the task to this runner and answered with something the task cannot be run on")

// Redeem redeems the grant of one task message.
//
// The answer is read strictly, as every answer is, and then held to the message it answers: the
// same task, the same input ports with the same digests, the same secrets at the same mounts, a
// tree that stays inside /agk/repo and an upload policy for the task's own namespace. An answer
// that fails either is ErrAnswerUnusable. NextAfter says what the runner does with any error this
// returns.
func (c *Client) Redeem(ctx context.Context, m bus.TaskMessage) (Redemption, error) {
	var r Redemption
	err := c.Do(ctx, http.MethodPost, redeemPath, redemptionRequest{
		Grant: m.Grant, TaskID: m.TaskID, IdempotencyKey: m.IdempotencyKey,
	}, &r)
	var refused *APIError
	var syntax *json.SyntaxError
	switch {
	case err == nil:
	case errors.As(err, &refused), errors.Is(err, ErrUnavailable), ctx.Err() != nil:
		return Redemption{}, err
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// A success that is not JSON at all, is empty or stops short, is not an API of another
		// version: it is a proxy's page, or a body cut off, and the 200 may not even be
		// the API's. Nothing says the task was bound, so it is asked again, which the
		// holder is answered as the first time.
		return Redemption{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		// A document that reads as JSON and not as this runner's answer: an API of another
		// version, whose 200 said the task is bound here.
		return Redemption{}, fmt.Errorf("%w: %w", ErrAnswerUnusable, err)
	}
	if err := r.answers(m); err != nil {
		return Redemption{}, fmt.Errorf("%w: task %s: %w", ErrAnswerUnusable, m.IdempotencyKey, err)
	}
	return r, nil
}

// Next is what a runner does after a redemption, which the page's table decides from the answer.
type Next int

const (
	// RedeemRun is a 200: the task is bound to this runner. It acknowledges the message, pulls
	// the image and runs the task.
	RedeemRun Next = iota

	// RedeemPutBack is a 403: this runner may not take the task, being draining or revoked or
	// narrowed by AGK_RUNNER_NAMESPACES. Nothing is bound. It clears the key from its record
	// and puts the message back on the queue, for another runner of the pool.
	RedeemPutBack

	// RedeemLetGo is a 409: another runner's task, or one that is over. Nothing is bound, and
	// the runner acknowledges the message and starts nothing.
	RedeemLetGo

	// RedeemReport is a 422, the API having nothing to answer with, ever, or a 200 whose
	// document the task cannot be run on (ErrAnswerUnusable). The runner reports that no
	// container ran, then acknowledges.
	RedeemReport

	// RedeemAgain is no answer at all, a 401 or any other 5xx: the grant expired or opens
	// nothing, the runner's own credential was refused, or a failure that may pass. A 200 that
	// is not JSON, or stops short, is one too, since it may not be the API's at all. The
	// answer may have been lost after the binding, so the runner keeps the key, names it in
	// its heartbeat and redeems again, acknowledging nothing, until the deadline the message
	// carries has passed, when it reports the task timed_out with no container ran.
	RedeemAgain
)

// NextAfter reads what Redeem answered as the page's table does.
//
// A status the table does not name, a 400 or a 404 among them, is asked again. It says nothing
// of whose the task is, and asking again is the one answer that never lets go of a task that may
// be bound here: the deadline ends the asking, where acknowledging would take the message off the
// queue with nobody holding it.
func NextAfter(err error) Next {
	switch {
	case err == nil:
		return RedeemRun
	case errors.Is(err, ErrForbidden):
		return RedeemPutBack
	case errors.Is(err, ErrConflict):
		return RedeemLetGo
	case errors.Is(err, ErrUnprocessable), errors.Is(err, ErrAnswerUnusable):
		return RedeemReport
	}
	return RedeemAgain
}

// answers holds a redemption to the task message it answers.
//
// Each rule is a way the two documents could describe two different tasks, and a container
// started on such a pair would run one task's brick on another's inputs, or mount a value where
// nothing asked for one. None of what it refuses prints a URL or a value.
func (r Redemption) answers(m bus.TaskMessage) error {
	if r.TaskID != m.TaskID {
		return fmt.Errorf("the redemption answers for task_id %q, and the message is task_id %q", r.TaskID, m.TaskID)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.ExpiresAt); err != nil {
		return fmt.Errorf("expires_at %q is not an RFC 3339 instant", r.ExpiresAt)
	}

	// The input ports: one entry for each port the message names, with the digest the
	// message gave it, and none besides.
	named := make(map[string]string, len(m.Inputs))
	for _, in := range m.Inputs {
		named[in.Port] = in.Digest
	}
	seen := map[string]bool{}
	for _, in := range r.Inputs {
		digest, ok := named[in.Port]
		switch {
		case !ok:
			return fmt.Errorf("the redemption gives input port %q, which the message does not name", in.Port)
		case seen[in.Port]:
			return fmt.Errorf("the redemption gives input port %q twice", in.Port)
		case in.Envelope.Digest != digest:
			return fmt.Errorf("the redemption gives port %s the envelope %s, and the message names %s", in.Port, in.Envelope.Digest, digest)
		case in.Envelope.URL == "":
			return fmt.Errorf("the redemption gives port %s no URL to fetch its envelope through", in.Port)
		}
		seen[in.Port] = true
		for _, a := range in.Artifacts {
			if !isHex64(a.SHA256) || a.URL == "" || !strings.HasPrefix(a.URI, "agk://run/") {
				return fmt.Errorf("the redemption gives port %s an artifact that is not a URI, a digest and a URL", in.Port)
			}
		}
	}
	for port := range named {
		if !seen[port] {
			return fmt.Errorf("the redemption gives nothing for input port %s, which the message names", port)
		}
	}

	// The secrets: one value for each secret the message names, at the mount the message
	// gave it, and none besides.
	mounts := make(map[string]string, len(m.Secrets))
	for _, s := range m.Secrets {
		mounts[s.Name] = s.Mount
	}
	given := map[string]bool{}
	for _, s := range r.Secrets {
		mount, ok := mounts[s.Name]
		switch {
		case !ok:
			return fmt.Errorf("the redemption gives a value for secret %q, which the message does not name", s.Name)
		case given[s.Name]:
			return fmt.Errorf("the redemption gives secret %q twice", s.Name)
		case s.Mount != mount:
			return fmt.Errorf("the redemption mounts secret %s at %q, and the message at %q", s.Name, s.Mount, mount)
		case s.Encoding != encodingUTF8 && s.Encoding != encodingBase64:
			return fmt.Errorf("secret %s is written in the encoding %q, and a value travels as utf-8 or base64", s.Name, s.Encoding)
		}
		given[s.Name] = true
	}
	for name := range mounts {
		if !given[name] {
			return fmt.Errorf("the redemption gives no value for secret %s, which the message names", name)
		}
	}

	// The tree: each path inside /agk/repo, once, with a mode and a digest.
	relocated := map[string]bool{}
	for _, f := range m.Files {
		if f.To != "" {
			relocated[f.To] = true
		}
	}
	paths := make(map[string]bool, len(r.Tree))
	for _, f := range r.Tree {
		if err := treePath(f.Path); err != nil {
			return err
		}
		if paths[f.Path] {
			return fmt.Errorf("the tree names %s twice", f.Path)
		}
		paths[f.Path] = true
		if _, err := treeMode(f.Mode); err != nil {
			return fmt.Errorf("the tree gives %s %w", f.Path, err)
		}
		if !isHex64(f.SHA256) || f.URL == "" {
			return fmt.Errorf("the tree gives %s no digest and URL to fetch it by", f.Path)
		}
		// A relocation is bound by the driver from the step's own files, so one the
		// step did not write would be a relocation nothing binds.
		if f.To != "" && !relocated[f.To] {
			return fmt.Errorf("the tree relocates %s to %s, which none of the step's files names", f.Path, f.To)
		}
	}

	// The upload policy: somewhere to post, under the task's own namespace. The store
	// refuses any other prefix, but only once the container has run and made something to
	// post, which is too late to find out.
	if r.Uploads.URL == "" {
		return errors.New("the redemption gives no upload policy to post the task's outputs with")
	}
	if want := artifact.Prefix(m.Namespace); r.Uploads.KeyPrefix != want {
		return fmt.Errorf("the upload policy writes under %q, and the task's namespace is written under %q", r.Uploads.KeyPrefix, want)
	}
	return nil
}

// treeModePattern is the wire's grammar for a mode, octal with or without its leading zero.
var treeModePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// treeMode reads a mode as the wire writes it.
func treeMode(mode string) (os.FileMode, error) {
	if !treeModePattern.MatchString(mode) {
		return 0, fmt.Errorf("the mode %q, and a mode is three octal digits with or without a leading zero", mode)
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("the mode %q, which does not read as octal", mode)
	}
	return os.FileMode(m), nil
}

// treePath holds one path of the tree to what can be written under /agk/repo and nowhere else.
//
// The wire refuses an absolute path and a .. segment. This refuses as well a path that is not
// written the one way it cleans to, an empty or . segment among them, so that two entries can
// never name one file in two spellings, and the root itself.
func treePath(p string) error {
	switch {
	case p == "" || p == ".":
		return fmt.Errorf("the tree names %q, which is the root of /agk/repo and not a file in it", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("the tree names %q, which is absolute, and a tree path is relative to /agk/repo", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("the tree names %q, which carries a NUL", p)
	case path.Clean(p) != p:
		return fmt.Errorf("the tree names %q, which is not written as it cleans to", p)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return fmt.Errorf("the tree names %q, which climbs out of /agk/repo", p)
		}
	}
	return nil
}

// secretValues is the driver.Secrets of one task: the values its redemption answered, decoded
// from the encoding they travelled in, since masking is a literal match against the bytes a
// container can print, and a base64 text is not what a container holding the value prints.
//
// It is written once, before the driver is handed it, and only read after, so the driver asking
// for two values at once needs no lock.
type secretValues struct {
	values map[string][]byte
}

// decodeSecrets reads each value of a redemption as its encoding says.
//
// A value that does not decode is refused naming the secret and never quoting it: the error goes
// into the agent's log, and a value half decoded is still the value.
func decodeSecrets(r Redemption) (*secretValues, error) {
	out := &secretValues{values: make(map[string][]byte, len(r.Secrets))}
	for _, s := range r.Secrets {
		switch s.Encoding {
		case encodingUTF8:
			out.values[s.Name] = []byte(s.Value)
		case encodingBase64:
			b, err := base64.StdEncoding.Strict().DecodeString(s.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: secret %s is marked base64 and does not decode as base64", ErrAnswerUnusable, s.Name)
			}
			out.values[s.Name] = b
		default:
			return nil, fmt.Errorf("%w: secret %s is written in the encoding %q, and a value travels as utf-8 or base64", ErrAnswerUnusable, s.Name, s.Encoding)
		}
	}
	return out, nil
}

// Value answers with one value, as a copy, so that nothing the driver does with it reaches the
// one this task holds.
func (s *secretValues) Value(_ context.Context, name string) ([]byte, error) {
	v, ok := s.values[name]
	if !ok {
		return nil, fmt.Errorf("runner: the redemption of this task gave no value for secret %s", name)
	}
	return append([]byte(nil), v...), nil
}

// isHex64 is a digest as a key is built from one: sixty-four lowercase hexadecimal characters.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// hexOf is the hexadecimal of a digest written as the wire writes one.
func hexOf(digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	return hex, ok && isHex64(hex)
}
