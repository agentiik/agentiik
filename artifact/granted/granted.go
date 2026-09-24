// Package granted is the byte layer a runner reaches the object store with: an artifact.Objects
// over what one redemption of a task's grant answered, and over nothing else.
//
// A runner reaches the object store over HTTPS with a "presigned URL or signed POST policy, no
// standing credential". It reads each object through the presigned GET the redemption minted for
// it, and writes every object its task makes with the task's one upload policy: a
// multipart/form-data POST of the policy's fields as they were given, then key, the policy's prefix
// and the object's SHA-256, then file, last. It asks the store for nothing it was not handed a URL
// for.
//
// It is a package of its own rather than a file of package artifact because it speaks HTTP, and
// artifact may not: the evaluator imports artifact and is importable with no server and no socket
// behind it, which graph/boundary_test.go holds.
package granted

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/artifact"
)

// Objects is the store as one task sees it: Open reads what its redemption named, Put writes under
// its upload policy, and Has knows nothing.
type Objects struct {
	get    map[string]string
	policy artifact.Policy
	client *http.Client
}

// Options are what one redemption answered, and what to reach the store through.
type Options struct {
	// Get is the presigned GET of every object the task may read, under the key artifact.Key
	// builds for it in the task's namespace: its input envelopes, the artifacts they name and
	// the files of its tree. The API resolves what a task may read, so a key missing here is
	// refused without a request rather than asked for.
	Get map[string]string

	// Uploads is the task's upload policy, which every object the task makes is posted with.
	Uploads artifact.Policy

	// Client is what every request goes through. Without one, requests go through a client
	// that follows no redirect: a presigned URL is the whole of its own authorisation, and a
	// client following a redirect sends the URL it came from on to the next host, signature
	// and all, in the Referer header.
	Client *http.Client
}

// ErrNotGranted is a key the redemption named no URL for.
//
// It is not fs.ErrNotExist, and deliberately. Absence is what a store says of a key it does not
// hold, and a caller reads it as an artifact that is gone. A key with no URL is one the runner was
// never given, which says nothing about whether the store holds it, and a runner asking for one is
// reading past what the API resolved for its task.
var ErrNotGranted = errors.New("granted: the task's redemption named no URL for this object")

// New builds the Objects of one task.
//
// A policy is required. Every task writes at least the envelopes of its ports, so a runner
// without somewhere to write would run the container and lose what it made.
func New(o Options) (*Objects, error) {
	switch {
	case o.Uploads.URL == "":
		return nil, errors.New("granted: no upload policy, and every task writes the envelopes of its ports")
	case o.Uploads.KeyPrefix == "":
		return nil, errors.New("granted: an upload policy with no key prefix, and every key it writes is that prefix and a digest")
	}
	if u, err := url.Parse(o.Uploads.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("granted: the upload policy is posted to %q, which is not an HTTP URL", o.Uploads.URL)
	}
	// key and file are the two parts the runner writes itself, and a policy carrying either
	// would post a form naming it twice, with the store left to choose which one it meant.
	for _, name := range []string{"key", "file"} {
		if _, carried := o.Uploads.Fields[name]; carried {
			return nil, fmt.Errorf("granted: the upload policy carries a field named %s, which is the runner's to write", name)
		}
	}

	client := o.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	policy := o.Uploads
	policy.Fields = maps.Clone(o.Uploads.Fields)
	return &Objects{get: maps.Clone(o.Get), policy: policy, client: client}, nil
}

// Has answers false, for every key and with no request.
//
// A runner has nothing to ask with. Its URLs read the objects its task was given and its policy
// writes without reading, so it cannot know what the store holds. Store.Put then posts every
// object it is handed, and the store, which hashes what arrives, holds identical bytes once
// however many times they are sent: what a replay stores is unchanged, and only what it sends
// is not.
func (o *Objects) Has(ctx context.Context, _ string) (bool, error) {
	return false, ctx.Err()
}

// Open fetches an object through the presigned GET the redemption named for its key.
//
// The bytes are handed back as they arrive. Store.Open and artifact.GetEnvelope hold them to their
// digest, and checking here as well would read an artifact twice to say what the reader already
// says.
func (o *Objects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	raw, granted := o.get[key]
	if !granted {
		return nil, fmt.Errorf("granted: object %s: %w", key, ErrNotGranted)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("granted: object %s: the URL the redemption named for it is not one: %w", key, unsigned(err))
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("granted: object %s: %w", key, unsigned(err))
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, fmt.Errorf("granted: object %s: the store answered %d: %w", key, resp.StatusCode, fs.ErrNotExist)
	case http.StatusForbidden:
		return nil, fmt.Errorf("granted: object %s: the store answered %d: %w", key, resp.StatusCode, artifact.ErrNotSigned)
	}
	return nil, fmt.Errorf("granted: object %s: the store answered %d", key, resp.StatusCode)
}

// Put posts one object under the task's upload policy: the policy's fields, in order of their
// names, then key, then the file, last, which is how a store honouring a POST policy takes one.
//
// A key the policy does not cover is refused without a request: its prefix and 64 lowercase
// hexadecimal characters, and nothing else. What each answer means is the built-in store's, as
// the task message sets it out. 201 is the object stored, or held already and stored again. 403
// is a policy that expired or was not signed for the key, and 400 is bytes that do not hash to
// their key, which Store.Put hashed itself: both are this side's failure and never the brick's.
// 413 is artifact_max_bytes, which Store.Put has already held the bytes to, so the store holds a
// lower limit than the runner does. Any other answer, a 5xx among them, is the store's trouble.
func (o *Objects) Put(ctx context.Context, key string, r io.Reader) error {
	digest, under := strings.CutPrefix(key, o.policy.KeyPrefix)
	if !under || !isDigest(digest) {
		return fmt.Errorf("granted: object %s: the upload policy writes %s followed by 64 lowercase hexadecimal characters, and nothing else: %w", key, o.policy.KeyPrefix, artifact.ErrNotSigned)
	}

	// The form is laid out around the file rather than copied through a pipe: everything before
	// the file and the boundary after it are a few hundred bytes, and the file, which runs to
	// artifact_max_bytes, goes out between the two as it is read. A bytes.Buffer never fails a
	// write, so neither does a form written into one.
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	for _, name := range slices.Sorted(maps.Keys(o.policy.Fields)) {
		w.WriteField(name, o.policy.Fields[name])
	}
	w.WriteField("key", key)
	w.CreateFormFile("file", digest)
	head := form.Len()
	w.Close()
	body := io.MultiReader(bytes.NewReader(form.Bytes()[:head]), r, bytes.NewReader(form.Bytes()[head:]))

	// A store honouring a POST policy is told how long the form is before it reads it: MinIO
	// answers a form sent chunked with a 400 before it looks at the policy, which would read here
	// as bytes that do not hash to their key. net/http cannot know the length of a MultiReader,
	// so it is worked out from the one part whose length is not already known. Both readers a
	// runner posts can say it, the file Store.Put stages an artifact in and the bytes of an
	// envelope, and only a reader that cannot goes out chunked, which the built-in store takes.
	size, known, err := remaining(r)
	if err != nil {
		return fmt.Errorf("granted: object %s: %w", key, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.policy.URL, body)
	if err != nil {
		return fmt.Errorf("granted: object %s: %w", key, unsigned(err))
	}
	if known {
		req.ContentLength = int64(form.Len()) + size
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("granted: object %s: %w", key, unsigned(err))
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("granted: object %s: the store answered %d: %w", key, resp.StatusCode, artifact.ErrNotSigned)
	case http.StatusBadRequest:
		return fmt.Errorf("granted: object %s: the store answered %d: %w", key, resp.StatusCode, artifact.ErrWrongDigest)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("granted: object %s: the store answered %d: %w", key, resp.StatusCode, artifact.ErrTooLarge)
	}
	return fmt.Errorf("granted: object %s: the store answered %d", key, resp.StatusCode)
}

// remaining is how many bytes are left to read from r, when r can say without being read, and
// leaves r where it found it. A file and bytes in memory can say; a pipe is a file too, and one
// that cannot be asked where it is is a reader of no known length rather than a failure.
func remaining(r io.Reader) (size int64, known bool, err error) {
	s, ok := r.(io.Seeker)
	if !ok {
		return 0, false, nil
	}
	at, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, false, nil
	}
	end, err := s.Seek(0, io.SeekEnd)
	if _, back := s.Seek(at, io.SeekStart); back != nil {
		return 0, false, back
	}
	if err != nil || end < at {
		return 0, false, nil
	}
	return end - at, true, nil
}

// unsigned leaves the URL out of an error from net/http, which names the one it failed on in full.
// A presigned URL is the authorisation to read what it names, and an error goes on to the task's
// log, which run:read reaches where the object needs run:read_data.
func unsigned(err error) error {
	var named *url.Error
	if errors.As(err, &named) {
		return named.Err
	}
	return err
}

// isDigest is 64 lowercase hexadecimal characters, which is the one way a key writes a SHA-256 and
// the one the built-in store takes.
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
