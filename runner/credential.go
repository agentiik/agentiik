package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/internal/token"
)

// Keeping the runner credential alive.
//
// "The key proves the machine: a stolen credential without it cannot be renewed." The credential
// join was answered is accepted until its rotate_by, and the agent renews it at two thirds of its
// window through POST /api/v1/runners/rotate, signing with the host's key, so that a host that
// stays up never reaches the instant, and one dark past it joins again. The renewed credential is
// written to /var/lib/agentiik/credential, which serve prefers to the one in runner.env: the unit's
// ProtectSystem=strict leaves /etc/agentiik read-only to the agent.

// CredentialPath is where the agent keeps the credential it renewed to, and its window.
const CredentialPath = "/var/lib/agentiik/credential"

// rotatePath is the route, as the router spells it.
const rotatePath = "/api/v1/runners/rotate"

// ErrKeyGone is a host whose key is not there, or is not a key: it cannot renew its credential,
// and "a host whose key is gone is a new runner", which joins again.
//
// It completes a sentence saying what became of the key, which is why it carries no prefix.
var ErrKeyGone = errors.New("a host whose key is gone is a new runner, which joins again with agk-runner join --replace")

// privateMaxBytes is the most the key and the credential file are read to. Each is a few hundred
// bytes as the agent and join write them, and a file of more is not one of theirs.
const privateMaxBytes = 64 << 10

// LoadKey reads the host's private key, which join wrote to path.
//
// A key that is not there, or that is not an Ed25519 key, is ErrKeyGone: nothing the agent can do
// brings it back, and a restart would only find it gone again. One that is there and cannot be
// read, or is readable by an account other than the agent's, is a host to set right, and is
// refused as such.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	text, err := readPrivate(path, "the host's key, which every renewal of this runner's credential is signed with")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("runner: %s, the key this runner joined with, is not there, so its credential can never be renewed, and %w", path, ErrKeyGone)
	case err != nil:
		return nil, err
	}
	block, rest := pem.Decode(text)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("runner: %s, the key this runner joined with, is not one PEM block of PKCS #8, which is how join writes it, so its credential can never be renewed, and %w", path, ErrKeyGone)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("runner: %s, the key this runner joined with, is not a key (%v), so its credential can never be renewed, and %w", path, err, ErrKeyGone)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("runner: %s, the key this runner joined with, is not an Ed25519 key, which is the one kind a renewal is signed with, so its credential can never be renewed, and %w", path, ErrKeyGone)
	}
	return key, nil
}

// Held is the runner credential the agent holds, and its window where the agent was told it.
type Held struct {
	Credential Secret

	// RotatedAt is when the agent was given the credential, by its own clock, and RotateBy
	// when the API stops taking it. Both are zero for the credential join wrote to runner.env,
	// whose window the agent was never told, and which it renews at once.
	RotatedAt, RotateBy time.Time
}

// renewAt is when the credential is renewed: two thirds of the way through its window, which
// leaves a third of it, ten days of the thirty a join rotation is by default, for an API that is
// down or a clock that is off to be put right before the host has to join again.
func (h Held) renewAt() time.Time {
	if h.RotateBy.IsZero() {
		return time.Time{}
	}
	return h.RotatedAt.Add(2 * h.RotateBy.Sub(h.RotatedAt) / 3)
}

// heldFile is /var/lib/agentiik/credential as the agent writes it: the runner it belongs to, the
// credential, and its window.
type heldFile struct {
	Runner     string `json:"runner"`
	Credential string `json:"credential"`
	RotatedAt  string `json:"rotated_at"`
	RotateBy   string `json:"rotate_by"`
}

// ReadHeld is the credential serve starts with: the one in path where the agent renewed to one,
// and the one runner.env holds, c.Credential, where it has not.
//
// The file names the runner it belongs to, and one naming another than runner.env is refused: it
// was left by an identity this host no longer has, since join takes it away, and a runner that
// took either of the two would be guessing which identity is this host's.
func ReadHeld(path string, c Config) (Held, error) {
	const what = "this runner's renewed credential"
	text, err := readPrivate(path, what)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Held{Credential: c.Credential}, nil
	case err != nil:
		return Held{}, err
	}
	var f heldFile
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Held{}, fmt.Errorf("runner: %s, %s, is not what the agent writes there: remove it, and the credential in %s is used, if it has not been renewed past", path, what, EnvPath)
	}
	rotatedAt, aerr := time.Parse(time.RFC3339Nano, f.RotatedAt)
	rotateBy, berr := time.Parse(time.RFC3339Nano, f.RotateBy)
	switch {
	case f.Runner != c.Runner:
		return Held{}, fmt.Errorf("runner: %s holds the credential of runner %.64q, and %s names runner %s: it was left by an identity this host no longer has, and join takes it away. Remove it, since this runner's own credential is the one in %s", path, f.Runner, EnvPath, c.Runner, EnvPath)
	case credentialFault(f.Credential) != "":
		return Held{}, fmt.Errorf("runner: %s holds a credential that %s: remove it, and the credential in %s is used, if it has not been renewed past", path, credentialFault(f.Credential), EnvPath)
	case aerr != nil || berr != nil || !rotateBy.After(rotatedAt):
		return Held{}, fmt.Errorf("runner: %s says the window of its credential in instants that are not two RFC 3339 instants, the second after the first: remove it, and the credential in %s is used, if it has not been renewed past", path, EnvPath)
	}
	return Held{Credential: Secret(f.Credential), RotatedAt: rotatedAt, RotateBy: rotateBy}, nil
}

// credentialFault says what is wrong with a runner credential as written, or nothing. It is never
// repeated, whatever is wrong with it.
func credentialFault(v string) string {
	kind, ok := token.KindOf(v)
	switch {
	case !ok || kind != token.Runner:
		return "is not a runner credential, which is written agkrunner_ followed by its secret"
	case strings.ContainsFunc(v, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-')
	}):
		return "holds a character a runner credential is never written with, which is base64url after its prefix"
	}
	return ""
}

// readPrivate reads a file that is the agent's alone, held to what runner.env is held to: a file
// and not a link, mode 0600 and owned by the account the agent runs as, read from the descriptor
// that was checked. A file that is not there is fs.ErrNotExist, for the caller to say what that
// means.
func readPrivate(path, what string) ([]byte, error) {
	checked, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("runner: %s, %s, cannot be read: %s", path, what, reasonOf(err))
	case checked.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("runner: %s, %s, is a symbolic link, and it is read from the file the agent's account owns, whose mode and owner are checked, and not from wherever a link points", path, what)
	case !checked.Mode().IsRegular():
		return nil, fmt.Errorf("runner: %s, %s, is not a file", path, what)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("runner: %s, %s, cannot be read: %s", path, what, reasonOf(err))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("runner: %s, %s, cannot be read: %s", path, what, reasonOf(err))
	}
	owner, owned := ownerOf(info)
	switch {
	case !os.SameFile(checked, info) || !info.Mode().IsRegular():
		return nil, fmt.Errorf("runner: %s, %s, was replaced while it was being read", path, what)
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("runner: %s, %s, has mode %#o, and it is readable by the agent's account alone: chmod 600 it, because what anybody on the host can read, anybody on the host has", path, what, info.Mode().Perm())
	case owned && owner != geteuid():
		return nil, fmt.Errorf("runner: %s, %s, is owned by account %d and the agent runs as account %d: chown it to that account, which is the one join gives it to", path, what, owner, geteuid())
	}
	text, err := io.ReadAll(io.LimitReader(f, privateMaxBytes+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("runner: %s, %s, cannot be read: %s", path, what, reasonOf(err))
	case len(text) > privateMaxBytes:
		return nil, fmt.Errorf("runner: %s, %s, is more than %d bytes, and what is written there is a few hundred", path, what, privateMaxBytes)
	}
	return text, nil
}

// rotationRequest is $defs/runnerRotation/request of the wire.
type rotationRequest struct {
	Runner    string `json:"runner"`
	At        string `json:"at"`
	Signature string `json:"signature"`
}

// rotationAnswer is $defs/runnerRotation/response.
type rotationAnswer struct {
	Credential Secret `json:"credential"`
	RotateBy   string `json:"rotate_by"`
}

// rotationSigned is the message a rotation's signature is over, as api.RotationSigned writes it: a
// line saying what it is for, then the runner and the request time, each exactly as the request
// writes them.
func rotationSigned(runner, at string) []byte {
	return []byte("agentiik runner rotation\n" + runner + "\n" + at)
}

// Rotator renews one runner's credential ahead of its rotate_by, for as long as the agent runs.
type Rotator struct {
	// Client is what the rotation is asked through, and what carries the new credential once
	// it is kept.
	Client *Client

	// Runner is who this runner is, and Key the host's private key, which join wrote.
	Runner string
	Key    ed25519.PrivateKey

	// Path is where the renewed credential is kept, CredentialPath.
	Path string

	// Revoked answers whether the last heartbeat said this runner was revoked, which a
	// rotation is then refused for with 403 until the grace ends. Nil is never.
	Revoked func() bool

	// Log is where the agent writes a line.
	Log func(string)

	// Now is the clock the request time is signed with and the window read against. Nil is the
	// time of day.
	Now func() time.Time

	// settle is how long a 403 is given for the heartbeat to say the runner is revoked, zero
	// being two heartbeat intervals, which a test shortens.
	settle time.Duration

	mu   sync.Mutex
	held Held
}

// NewRotator is the rotator of a runner that holds held, which its client already carries.
func NewRotator(c *Client, runner string, key ed25519.PrivateKey, path string, held Held) *Rotator {
	return &Rotator{Client: c, Runner: runner, Key: key, Path: path, held: held}
}

// Held is the credential this rotator last kept, or the one it started with.
func (r *Rotator) Held() Held {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

func (r *Rotator) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rotator) say(s string) {
	if r.Log != nil {
		r.Log(s)
	}
}

// Run renews the credential at two thirds of its window until ctx ends, and at once where its
// window is not known, which is the credential join wrote. It answers nil once ctx ends.
//
// A renewal that is not answered, or refused in a way that may pass (a clock off the API's by more
// than it allows, a request time not after the last one's), is asked again, from a second doubling
// to thirty, since the third of the window left is there for exactly that. One answered 401 ends
// Run with an error saying to join again, as a heartbeat answered 401 does: the credential opens
// nothing. One answered 403 is a revoked runner, which renews nothing and ends Run with nil, or a
// signature the API does not take from this runner, whose key is then not the one it joined with,
// which ends Run saying to join again: nothing this host can sign renews it.
func (r *Rotator) Run(ctx context.Context) error {
	wait := retryFirst
	for {
		if !sleep(ctx, r.Held().renewAt().Sub(r.now())) {
			return nil
		}
		err := r.Rotate(ctx)
		switch {
		case err == nil:
			wait = retryFirst
			continue
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrCredentialRefused):
			return fmt.Errorf("runner: %s: %w", joinAgain, err)
		case errors.Is(err, ErrForbidden) && r.revoked(ctx):
			r.say("this runner is revoked, so its credential is not renewed, and it is accepted until the grace the heartbeat names ends")
			return nil
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrForbidden):
			return fmt.Errorf("runner: the API did not take a renewal of this runner's credential signed with the host's key, which is then not the key runner %s joined with, and %w: %w", r.Runner, ErrKeyGone, err)
		}
		until := "until its rotate_by"
		if by := r.Held().RotateBy; !by.IsZero() {
			until = "until " + by.UTC().Format(time.RFC3339)
		}
		r.say(fmt.Sprintf("this runner's credential could not be renewed, and is renewed again in %s; the one it holds is accepted %s: %s", wait, until, err))
		if !sleep(ctx, wait) {
			return nil
		}
		wait = min(2*wait, retryMost)
	}
}

// revoked answers whether a rotation refused 403 was refused for a revocation, rather than for a
// signature the API does not take.
//
// The API refuses a revoked runner's rotation at once, and the agent learns of the revocation at
// its next heartbeat, so a rotation made in between is refused before Revoked says so. The answer
// is waited for, two heartbeat intervals at most, before the refusal is taken for a key that is not
// the one the runner joined with: that ends the agent, and a revoked runner's grace is there for
// the work it holds to be answered.
func (r *Rotator) revoked(ctx context.Context) bool {
	if r.Revoked == nil {
		return false
	}
	settle := r.settle
	if settle <= 0 {
		settle = 2 * HeartbeatInterval
	}
	for until := r.now().Add(settle); ; {
		if r.Revoked() {
			return true
		}
		if !r.now().Before(until) || !sleep(ctx, min(retryFirst, until.Sub(r.now()))) {
			return false
		}
	}
}

// Rotate renews the credential once: it signs the request, keeps the new credential on the disk,
// and only then has the client carry it.
//
// In that order because the API stops taking the old credential the first time it sees the new
// one. A new credential carried and not kept would be lost with the agent, which would come back
// holding one the API no longer takes; one kept and not carried is never used, and the old one it
// was answered for goes on working until its own rotate_by, so the next rotation is asked with it
// and the credential nobody used is dropped. The file is created before the API is asked anything,
// so that a directory the agent cannot write in costs no rotation.
func (r *Rotator) Rotate(ctx context.Context) error {
	file, err := stage(r.Path, nil)
	if err != nil {
		return err
	}
	defer file.abandon()

	// Signed afresh on every try, since each rotation signs a later time than the last, and a
	// request sent again as it was would be refused as a copy.
	at := r.now().UTC().Format(time.RFC3339Nano)
	var answer rotationAnswer
	if err := r.Client.Do(ctx, http.MethodPost, rotatePath, rotationRequest{
		Runner: r.Runner, At: at,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(r.Key, rotationSigned(r.Runner, at))),
	}, &answer); err != nil {
		return err
	}
	rotatedAt := r.now()
	rotateBy, err := time.Parse(time.RFC3339Nano, answer.RotateBy)
	switch {
	case credentialFault(string(answer.Credential)) != "":
		return fmt.Errorf("runner: POST %s: the credential answered %s", rotatePath, credentialFault(string(answer.Credential)))
	case err != nil:
		return fmt.Errorf("runner: POST %s: the credential is accepted until %.64q, which is not an RFC 3339 instant", rotatePath, answer.RotateBy)
	case !rotateBy.After(rotatedAt):
		return fmt.Errorf("runner: POST %s: the credential answered stopped being accepted at %s, before it arrived", rotatePath, answer.RotateBy)
	}

	held := Held{Credential: answer.Credential, RotatedAt: rotatedAt, RotateBy: rotateBy}
	text, err := json.Marshal(heldFile{
		Runner: r.Runner, Credential: string(held.Credential),
		RotatedAt: rotatedAt.UTC().Format(time.RFC3339Nano), RotateBy: rotateBy.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("runner: %s cannot be written: %w", r.Path, err)
	}
	if err := file.write(append(text, '\n')); err != nil {
		return err
	}
	if err := file.commit(true); err != nil {
		// A rename that happened before the directory could be synced has put the new
		// credential in place, and a restart would start from it: from here on it is the
		// one carried, or the next rotation, asked with the old one, would have the API drop
		// the credential on the disk.
		kept, rerr := ReadHeld(r.Path, Config{Runner: r.Runner})
		if rerr != nil || kept.Credential != held.Credential {
			return err
		}
		r.say(err.Error() + ", and the renewed credential is carried all the same, since it is in place")
	}
	r.Client.use(held.Credential)
	r.mu.Lock()
	r.held = held
	r.mu.Unlock()
	r.say(fmt.Sprintf("this runner's credential was renewed, and the new one, kept in %s, is accepted until %s and renewed from %s",
		r.Path, rotateBy.UTC().Format(time.RFC3339), held.renewAt().UTC().Format(time.RFC3339)))
	return nil
}
