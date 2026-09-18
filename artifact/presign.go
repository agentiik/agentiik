package artifact

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The URL that carries its own authorisation.
//
// "runner, object store, HTTPS outbound, presigned URL only, no standing credential". A runner
// holding an object-store credential would be a runner whose compromise costs every object in the
// installation rather than "the tasks in its hands and the namespaces its policy accepts", so what
// it is given is a URL that does one thing, to one object, for one run, until one instant.
//
// An object store that mints its own is used through Presigner. The built-in store has no signing
// authority of its own, so the API is it: Signed mints the URL and serves what it names, and the
// two live in one type because a minter and a checker that disagree by one byte is a store that
// refuses everything or accepts anything.

// signDomain separates these signatures from every other use of the installation's key. A
// signature that could be replayed into another context would be one that means something
// somewhere it was never meant to be read.
const signDomain = "agentiik.presign.v1"

// Presigner mints a URL that is the whole of the authorisation to do one thing to one object.
//
// The method is part of what is signed, so a URL that fetches is not a URL that stores. So is the
// run, because "scoped to one run" is what makes a leaked URL worth nothing once the run it came
// from is over.
type Presigner interface {
	Presign(ctx context.Context, method, key string, run agk.RunID, until time.Time) (string, error)
}

// Signed is the built-in presigner, and the handler that honours what it signed.
type Signed struct {
	objects Objects
	key     []byte
	base    string
	limits  agk.Limits
	now     func() time.Time
}

// SignedOptions are what the built-in presigner needs.
type SignedOptions struct {
	// Key is the installation's signing key, thirty-two bytes or more. It is not the
	// secret store's master key: one key, two purposes, is how a weakness in either
	// becomes a weakness in both.
	Key []byte

	// Base is the URL the objects route is served at, for example
	// https://agentiik.example.com/api/v1/objects. It is written down rather than worked
	// out from a request, because a URL minted from the Host header is a URL an attacker
	// chooses the host of.
	Base string

	Limits agk.Limits
	Now    func() time.Time
}

// NewSigned builds one.
func NewSigned(o Objects, opt SignedOptions) (*Signed, error) {
	switch {
	case o == nil:
		return nil, errors.New("artifact: no objects to sign for")
	case len(opt.Key) < 32:
		return nil, errors.New("artifact: a signing key of fewer than thirty-two bytes")
	case opt.Base == "":
		return nil, errors.New("artifact: no base URL, and a presigned URL built from a request's own Host header is a URL somebody else chooses the host of")
	}
	u, err := url.Parse(opt.Base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("artifact: %q is not a base URL", opt.Base)
	}
	if opt.Limits == (agk.Limits{}) {
		opt.Limits = agk.DefaultLimits()
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Signed{
		objects: o, key: append([]byte(nil), opt.Key...),
		base: strings.TrimSuffix(opt.Base, "/"), limits: opt.Limits, now: opt.Now,
	}, nil
}

// Presign answers the URL.
func (s *Signed) Presign(_ context.Context, method, key string, run agk.RunID, until time.Time) (string, error) {
	switch {
	case method != http.MethodGet && method != http.MethodPut:
		return "", fmt.Errorf("artifact: %s is not something a presigned URL does", method)
	case run == "":
		return "", errors.New("artifact: a presigned URL for no run, and one is scoped to a run")
	case until.IsZero():
		return "", errors.New("artifact: a presigned URL that never expires, which is a standing credential with extra steps")
	}
	if err := checkKey(key); err != nil {
		return "", err
	}

	expires := until.UTC().Unix()
	signature := s.sign(method, key, string(run), expires)
	return fmt.Sprintf("%s/%s?run=%s&expires=%d&signature=%s",
		s.base, key, url.QueryEscape(string(run)), expires, signature), nil
}

// sign is the one place the signed bytes are laid out.
//
// Length-prefixed, because concatenation is ambiguous: a run named "a" with a key ending "b" and a
// run named "ab" with a key ending in nothing would hash the same, and a signature that two
// different requests share is a signature that authorises the wrong one.
func (s *Signed) sign(method, key, run string, expires int64) string {
	mac := hmac.New(sha256.New, s.key)
	for _, part := range []string{signDomain, method, key, run} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		mac.Write(length[:])
		mac.Write([]byte(part))
	}
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(expires))
	mac.Write(stamp[:])
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrNotSigned is a request carrying no signature of ours, one that expired, or one for something
// other than what it asks for. One error for all of them, because a refusal that says which is a
// refusal somebody tunes against.
var ErrNotSigned = errors.New("artifact: this request is not signed for what it asks")

// Check answers the run a request is signed for, and refuses everything else.
func (s *Signed) Check(r *http.Request, key string) (agk.RunID, error) {
	if err := checkKey(key); err != nil {
		return "", ErrNotSigned
	}
	q := r.URL.Query()
	run, signature := q.Get("run"), q.Get("signature")
	expires, err := strconv.ParseInt(q.Get("expires"), 10, 64)
	if err != nil || run == "" || signature == "" {
		return "", ErrNotSigned
	}
	if !hmac.Equal([]byte(signature), []byte(s.sign(r.Method, key, run, expires))) {
		return "", ErrNotSigned
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return "", ErrNotSigned
	}
	return agk.RunID(run), nil
}

// Serve honours a signed request, and refuses an unsigned one without saying what is there.
//
// The key comes from the caller rather than from the path, because the route that mounts this
// knows its own prefix and a handler that stripped one by hand is a handler that can strip the
// wrong number of segments.
func (s *Signed) Serve(w http.ResponseWriter, r *http.Request, key string) {
	if _, err := s.Check(r, key); err != nil {
		// One answer for a signature that is wrong, one that expired, and one that was
		// minted for something else. A refusal that said which is a refusal somebody
		// tunes a forgery against, and the caller here is a machine following a URL it
		// was handed: every one of them means the same thing to it.
		refuse(w, http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, key)
	case http.MethodPut:
		s.put(w, r, key)
	default:
		refuse(w, http.StatusMethodNotAllowed)
	}
}

func (s *Signed) get(w http.ResponseWriter, r *http.Request, key string) {
	rc, err := s.objects.Open(r.Context(), key)
	if errors.Is(err, fs.ErrNotExist) {
		refuse(w, http.StatusNotFound)
		return
	}
	if err != nil {
		refuse(w, http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	// An object is bytes and its media type lives on the envelope entry that names it, not
	// here: a store that guessed one would be a store deciding how a browser treats content
	// it was handed a digest for.
	w.Header().Set("Content-Type", defaultMediaType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, rc)
}

func (s *Signed) put(w http.ResponseWriter, r *http.Request, key string) {
	want, ok := digestOf(key)
	if !ok {
		refuse(w, http.StatusForbidden)
		return
	}

	// The key is the digest of the content, so the bytes are checked against it as they
	// arrive. Without this a URL for one object stores any bytes at all under a digest
	// somebody else's envelope already names, which is the one write content addressing
	// must not allow.
	body := io.Reader(r.Body)
	if s.limits.ArtifactMaxBytes > 0 {
		body = io.LimitReader(body, s.limits.ArtifactMaxBytes+1)
	}
	checked := &digestChecked{r: body, want: want, sum: sha256.New(), limit: s.limits.ArtifactMaxBytes}

	err := s.objects.Put(r.Context(), key, checked)
	switch {
	case errors.Is(err, errWrongDigest):
		refuse(w, http.StatusBadRequest)
		return
	case errors.Is(err, errTooLarge):
		refuse(w, http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		refuse(w, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

var (
	errWrongDigest = errors.New("artifact: the bytes are not the object this URL names")
	errTooLarge    = errors.New("artifact: above artifact_max_bytes")
)

// digestChecked fails the write rather than the check: Put is all or nothing, so a reader that
// errors at the last byte leaves nothing behind for anyone to fetch.
type digestChecked struct {
	r     io.Reader
	want  string
	sum   hash.Hash
	read  int64
	limit int64
}

func (d *digestChecked) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if n > 0 {
		d.read += int64(n)
		d.sum.Write(p[:n])
	}
	if d.limit > 0 && d.read > d.limit {
		return n, errTooLarge
	}
	if errors.Is(err, io.EOF) {
		if hex.EncodeToString(d.sum.Sum(nil)) != d.want {
			return n, errWrongDigest
		}
	}
	return n, err
}

// digestOf reads the digest out of a key built by Key.
func digestOf(key string) (string, bool) {
	namespace, rest, ok := strings.Cut(key, "/sha256/")
	if !ok || namespace == "" || len(rest) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return "", false
	}
	return rest, true
}

// refuse answers with a status and nothing else. There is no body worth writing: the caller is a
// runner following a URL it was handed, and every failure here means the same thing to it.
func refuse(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
