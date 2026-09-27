package api

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// The object routes, which are the built-in store's whole surface.
//
// "runner, object store, HTTPS outbound, presigned URL only, no standing credential." Where an
// installation has a real object store the runner goes straight to it and these routes are not
// mounted. Where it has the built-in one there is nothing else to sign a URL, so the API is the
// object store, which is what deployment profile A already says a single node is.

// ObjectAPI serves what a presigned URL names.
type ObjectAPI struct {
	signed *artifact.Signed

	// pool is where a write is held to its namespace's max_artifact_bytes, and nil holds none to
	// it: a store with no database behind it, which only a test builds.
	pool *db.Pool
}

// NewObjects registers the object routes: the GET and the PUT a presigned URL does, and the POST a
// policy does. Every write is held to its namespace's max_artifact_bytes through pool.
func NewObjects(rt *Router, signed *artifact.Signed, pool *db.Pool) (*ObjectAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case signed == nil:
		return nil, errors.New("api: no presigner, and an object route that signed nothing would serve every object to anybody")
	}
	s := &ObjectAPI{signed: signed, pool: pool}

	// The one sentence that puts these outside the authorisation hook. It is long because
	// the guard demands it be, and it is the right demand: this is the fourth public route
	// in the design and the first one whose authorisation is a signature rather than a
	// principal.
	why := Public{Why: "a presigned URL is itself the authorisation: it names one method, one object, one run and one instant, and it is signed by the installation, so asking for a credential here as well would mean the runner holding a standing object-store credential, which is the thing presigning exists to remove"}
	// Outside /api/v1, because a presigned URL is not a call of the API: it stands where the
	// object store's own URL stands wherever there is a real store, and a runner follows it
	// without knowing which of the two it holds.
	for _, r := range []struct{ method, pattern string }{
		{"GET", "/objects/{key...}"},
		{"PUT", "/objects/{key...}"},
	} {
		if err := rt.Handle(r.method, r.pattern, why, s.object); err != nil {
			return nil, err
		}
	}

	// A form is posted to its namespace, as a real store's is posted to its bucket, and names
	// its key in its body. The namespace is read out of the path by the router, as every
	// namespace a handler is given is, and the policy then has to have been signed for it.
	policy := Public{Why: "a signed policy is itself the authorisation: it names one namespace's prefix, one run and one instant, and it is signed by the installation, so asking for a credential here as well would mean the runner holding a standing object-store credential. It travels in the form rather than in the URL because the key it stores under is the digest of bytes that did not exist when it was signed"}
	if err := rt.Handle("POST", "/objects/{namespace}", policy, s.post); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ObjectAPI) object(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	key := r.PathValue("key")
	if _, err := s.signed.Check(r.Method, key, r.URL.Query()); err != nil {
		// One answer for a signature that is wrong, one that expired, and one minted for
		// something else. A refusal that said which is a refusal somebody tunes a forgery
		// against, and the caller here is a machine following a URL it was handed: every
		// one of them means the same thing to it.
		nothing(w, http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.fetch(w, r, key)
	case http.MethodPut:
		s.store(w, r, key, r.Body, signedUntil(r.URL.Query()))
	}
}

// formMaxBytes is how much of a posted form may come before its file. What comes first is the
// fields of a policy and a key, a few hundred bytes the API minted, and it is read before anything
// about the caller is known, because the policy is inside it: a route that read an unbounded form
// before checking a signature would hold whatever anybody cared to send it.
const formMaxBytes = 64 << 10

// errPreamble is a form carrying more than formMaxBytes before its file.
var errPreamble = errors.New("api: a form carrying more than it may before its file")

// post stores one object from a form, taken the way a store honouring a POST policy takes one: the
// signed fields and the key, then the file, which is the last part and the only one whose bytes
// are stored. A field the store does not read is ignored rather than refused, because the fields
// are passed through by a runner that does not interpret them.
func (s *ObjectAPI) post(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		nothing(w, http.StatusBadRequest)
		return
	}
	body := &preamble{r: r.Body, left: formMaxBytes}
	form := multipart.NewReader(body, params["boundary"])
	fields := url.Values{}
	for {
		part, err := form.NextPart()
		if err != nil {
			// Whatever stopped the form, its end included: a form with no file has
			// nothing to store.
			nothing(w, http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			value, err := io.ReadAll(part)
			if err != nil {
				nothing(w, http.StatusBadRequest)
				return
			}
			fields.Add(part.FormName(), string(value))
			continue
		}

		key := fields.Get("key")
		if _, err := s.signed.CheckPolicy(over.Namespace, key, fields); err != nil {
			// One answer for every way a form is not signed for what it asks, as a URL
			// gets, and for the same reason.
			nothing(w, http.StatusForbidden)
			return
		}
		body.lifted = true
		s.store(w, r, key, part, signedUntil(fields))
		return
	}
}

// preamble bounds what a form carries before its file, and is lifted once the file begins, since
// Store bounds the file by artifact_max_bytes.
type preamble struct {
	r      io.Reader
	left   int64
	lifted bool
}

func (p *preamble) Read(b []byte) (int, error) {
	if p.lifted {
		return p.r.Read(b)
	}
	if p.left <= 0 {
		return 0, errPreamble
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, err := p.r.Read(b)
	p.left -= int64(n)
	return n, err
}

func (s *ObjectAPI) fetch(w http.ResponseWriter, r *http.Request, key string) {
	rc, err := s.signed.Fetch(r.Context(), key)
	if errors.Is(err, fs.ErrNotExist) {
		nothing(w, http.StatusNotFound)
		return
	}
	if err != nil {
		nothing(w, http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	// An object is bytes, and its media type lives on the envelope entry that names it. A
	// store that guessed one would be deciding how a browser treats content it was handed a
	// digest for.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, rc)
}

// store writes one object, held to its namespace's max_artifact_bytes: room is made for it before
// its bytes are read and settled once they are stored or refused, and an object that would take the
// namespace past the quota is answered 507 with nothing read. until is when the policy or the URL it
// is written with expires, which the room lapses with.
//
// The room is made at the length of the request, which is the most the object may be: a form's
// length counts its fields and its boundaries as well, a few hundred bytes more than the file, and
// a request of no stated length is given the room left, up to artifact_max_bytes. The object is held
// to that room as it arrives, and counted at its size once it is in.
func (s *ObjectAPI) store(w http.ResponseWriter, r *http.Request, key string, body io.Reader, until time.Time) {
	room, err := s.makeRoom(r.Context(), key, r.ContentLength, until)
	var none *db.NoRoom
	switch {
	case errors.As(err, &none):
		nothing(w, http.StatusInsufficientStorage)
		return
	case err != nil:
		nothing(w, http.StatusInternalServerError)
		return
	}
	counted := &within{r: body, left: -1}
	if room.Held() {
		counted.left = room.Bound()
	}
	err = s.signed.Store(r.Context(), key, counted)
	s.settle(r.Context(), room, counted, err)
	switch {
	case errors.Is(err, artifact.ErrWrongDigest):
		nothing(w, http.StatusBadRequest)
	case errors.Is(err, artifact.ErrTooLarge):
		nothing(w, http.StatusRequestEntityTooLarge)
	case errors.Is(err, artifact.ErrNoRoom):
		nothing(w, http.StatusInsufficientStorage)
	case err != nil:
		nothing(w, http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusCreated)
	}
}

// heldRoom is room made for one write, and the namespace it is settled in.
type heldRoom struct {
	db.Room
	namespace string
}

// makeRoom makes room for the object key names, of up to length bytes where length is not
// negative, in its namespace. Nothing is held where the store has no database behind it, or where
// the key is not one an object is written under, which the store then refuses by itself.
func (s *ObjectAPI) makeRoom(ctx context.Context, key string, length int64, until time.Time) (heldRoom, error) {
	namespace, digest, ok := strings.Cut(key, "/sha256/")
	if s.pool == nil || !ok || !lowerHex(digest) || until.IsZero() {
		return heldRoom{}, nil
	}
	// A length past artifact_max_bytes is an object the store refuses whatever the room, so it is
	// not given more room than that.
	most := s.signed.Limits().ArtifactMaxBytes
	if most > 0 && length > most {
		length = most
	}
	var room db.Room
	err := s.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		room, err = ns.MakeRoom(ctx, db.Upload{Digest: digest, Length: length, Most: most, Until: until})
		return err
	})
	if err != nil {
		return heldRoom{}, err
	}
	return heldRoom{Room: room, namespace: namespace}, nil
}

// lowerHex is 64 lowercase hexadecimal characters, the one way a key writes a digest, which is
// what room is counted by.
func lowerHex(digest string) bool {
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}

// settle counts a stored object at its size, or gives back the room made for one that was not
// stored. Settled whether or not the request is still there, since a writer that went once its
// bytes were in, or halfway, leaves room to count or to give back all the same; a settlement that
// fails leaves the room as it was made, which lapses with the policy.
func (s *ObjectAPI) settle(ctx context.Context, room heldRoom, counted *within, stored error) {
	if !room.Held() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	s.pool.In(ctx, room.namespace, func(ctx context.Context, ns *db.NS) error {
		if stored != nil {
			return ns.Unwritten(ctx, room.Room)
		}
		return ns.Stored(ctx, room.Room, counted.read)
	})
}

// within counts what is read of an object, and refuses it past left bytes where left is not
// negative: the room made for it, which the store then writes nothing of.
type within struct {
	r    io.Reader
	left int64
	read int64
}

func (c *within) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.left >= 0 && c.read > c.left {
		return n, artifact.ErrNoRoom
	}
	return n, err
}

// signedUntil is when a signed request stops being worth anything, read from the fields or the
// query its signature was checked over, and the zero time where there is none to read.
func signedUntil(signed url.Values) time.Time {
	expires, err := strconv.ParseInt(signed.Get("expires"), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(expires, 0).UTC()
}

// nothing answers with a status and no body. There is nothing worth writing: the caller is a
// machine following a URL, and a body it would not read is a body that only helps somebody
// probing.
func nothing(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
