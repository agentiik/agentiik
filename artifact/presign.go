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
// Writing is the one thing a URL per object cannot do for a task, because the key of an object is
// the digest of its bytes and no digest exists until the container has exited. So a task is given
// one policy instead: a form to post, signed for every key under its namespace's prefix, for one
// run, until one instant. Deduplication never crosses a namespace, so the prefix is the narrowest
// bound that still names objects nobody has made yet.
//
// An object store that mints its own is used through Presigner. The built-in store has no signing
// authority of its own, so the API is it: Signed mints the URL and the policy, checks one back and
// moves the bytes, and the three live in one type because a minter and a checker that disagree by
// one byte is a store that refuses everything or accepts anything.
//
// What is not here is the HTTP. The evaluator imports this package and is importable with no
// server behind it, which is what keeps agk run --local the same code path as a server run, so the
// route and the status codes belong to whoever is serving.

// The methods a signature is for. Spelled out rather than taken from net/http, which this package
// may not import. A presigned URL does GET or PUT. POST is a policy's and never a URL's, so what is
// signed for a whole prefix cannot be presented as what was signed for one key.
const (
	MethodGet  = "GET"
	MethodPut  = "PUT"
	MethodPost = "POST"
)

// signDomain separates these signatures from every other use of the installation's key. A
// signature that could be replayed into another context would be one that means something
// somewhere it was never meant to be read.
const signDomain = "agentiik.presign.v1"

// Presigner mints what a runner reaches the store with, which is the whole of its authorisation.
//
// Presign answers a URL that does one thing to one object. The method is part of what is signed,
// so a URL that fetches is not a URL that stores. So is the run, because "scoped to one run" is
// what makes a leaked URL worth nothing once the run it came from is over.
//
// Policy answers the one form a task writes everything it makes with: signed for a namespace's
// prefix rather than for a key, and for one run until one instant, like a URL.
type Presigner interface {
	Presign(ctx context.Context, method, key string, run agk.RunID, until time.Time) (string, error)
	Policy(ctx context.Context, namespace string, run agk.RunID, until time.Time) (Policy, error)
}

// Policy is somewhere to write: where the form is posted, the signed fields it is posted with, and
// the prefix every key it stores starts with.
//
// A runner posts the fields as they are given, then the key, which is the prefix followed by the
// digest of what it is storing, then the file. The fields are the store's own vocabulary and
// nobody else reads them, which is what lets a store with a signing authority of its own mint them
// in its own words and the built-in one in this package's.
type Policy struct {
	URL       string
	Fields    map[string]string
	KeyPrefix string
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
	// https://agentiik.example.com/objects. It is written down rather than worked
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
	case method != MethodGet && method != MethodPut:
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

// sign is the one place the signed bytes are laid out: the method, then the key a URL names or the
// prefix a policy covers, then the run and the expiry.
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
// refusal somebody tunes a forgery against.
var ErrNotSigned = errors.New("artifact: this request is not signed for what it asks")

// Check answers the run a signed request is for, and refuses everything else.
//
// The method and the key come from the caller because only the caller knows how it routes; the
// rest comes out of the query the URL carries.
func (s *Signed) Check(method, key string, q url.Values) (agk.RunID, error) {
	if method != MethodGet && method != MethodPut {
		return "", ErrNotSigned
	}
	if err := checkKey(key); err != nil {
		return "", ErrNotSigned
	}
	return s.verify(method, key, q)
}

// Policy answers the form a task posts what it makes with.
//
// It is posted to the base URL followed by the namespace, and its fields are the ones a presigned
// URL carries in its query: run, expires and signature. What is signed is POST and the prefix,
// where a URL signs GET or PUT and one key, so the one form stores any object under the prefix and
// neither signature can stand in for the other.
func (s *Signed) Policy(_ context.Context, namespace string, run agk.RunID, until time.Time) (Policy, error) {
	switch {
	case run == "":
		return Policy{}, errors.New("artifact: a policy for no run, and one is scoped to a run")
	case until.IsZero():
		return Policy{}, errors.New("artifact: a policy that never expires, which is a standing credential with extra steps")
	}
	if err := checkNamespace(namespace); err != nil {
		return Policy{}, err
	}

	prefix := Prefix(namespace)
	expires := until.UTC().Unix()
	return Policy{
		URL: s.base + "/" + url.PathEscape(namespace),
		Fields: map[string]string{
			"run":       string(run),
			"expires":   strconv.FormatInt(expires, 10),
			"signature": s.sign(MethodPost, prefix, string(run), expires),
		},
		KeyPrefix: prefix,
	}, nil
}

// CheckPolicy answers the run a posted form is signed for, when the policy it carries allows the
// key it names, and refuses everything else.
//
// The namespace comes from where the form was posted, which is where Policy said to post it, and
// the key and the signed fields come out of the form. A policy allows its prefix followed by a
// digest, 64 lowercase hexadecimal characters, and nothing else: anything longer or shorter is a
// name no envelope could address an object by, and a digest in capitals is a second key for bytes
// that already have one.
func (s *Signed) CheckPolicy(namespace, key string, fields url.Values) (agk.RunID, error) {
	if checkNamespace(namespace) != nil {
		return "", ErrNotSigned
	}
	prefix := Prefix(namespace)
	if digest, under := strings.CutPrefix(key, prefix); !under || !isDigest(digest) {
		return "", ErrNotSigned
	}
	return s.verify(MethodPost, prefix, fields)
}

// verify is what a URL and a policy are both checked by: the signature over what the caller says
// was signed, and the instant it stops being worth anything.
func (s *Signed) verify(method, signed string, q url.Values) (agk.RunID, error) {
	run, signature := q.Get("run"), q.Get("signature")
	expires, err := strconv.ParseInt(q.Get("expires"), 10, 64)
	if err != nil || run == "" || signature == "" {
		return "", ErrNotSigned
	}
	if !hmac.Equal([]byte(signature), []byte(s.sign(method, signed, run, expires))) {
		return "", ErrNotSigned
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return "", ErrNotSigned
	}
	return agk.RunID(run), nil
}

// Fetch opens an object. The caller has already checked the signature, which is the only thing
// standing between this and every object in the store.
func (s *Signed) Fetch(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.objects.Open(ctx, key)
}

// Store writes one, and only the one the key names.
//
// The key is the digest of the content, so the bytes are checked against it as they arrive.
// Without this a URL for one object, or a policy for a whole prefix, stores any bytes at all under a
// digest somebody else's envelope already names, which is the one write content addressing must
// not allow.
func (s *Signed) Store(ctx context.Context, key string, r io.Reader) error {
	want, ok := digestOf(key)
	if !ok {
		return ErrNotSigned
	}
	if s.limits.ArtifactMaxBytes > 0 {
		r = io.LimitReader(r, s.limits.ArtifactMaxBytes+1)
	}
	return s.objects.Put(ctx, key, &digestChecked{
		r: r, want: want, sum: sha256.New(), limit: s.limits.ArtifactMaxBytes,
	})
}

// ErrWrongDigest is bytes that are not the object the key names.
var ErrWrongDigest = errors.New("artifact: the bytes are not the object this URL names")

// ErrTooLarge is an object above artifact_max_bytes.
var ErrTooLarge = errors.New("artifact: above artifact_max_bytes")

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
		return n, ErrTooLarge
	}
	if errors.Is(err, io.EOF) {
		if hex.EncodeToString(d.sum.Sum(nil)) != d.want {
			return n, ErrWrongDigest
		}
	}
	return n, err
}

// isDigest is 64 lowercase hexadecimal characters, which is the one way a key writes a SHA-256.
// hex.DecodeString also reads capitals, which is why it is not what decides.
func isDigest(s string) bool {
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
