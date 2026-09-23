package api

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/agentiik/agentiik/artifact"
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
}

// NewObjects registers the object routes: the GET and the PUT a presigned URL does, and the POST a
// policy does.
func NewObjects(rt *Router, signed *artifact.Signed) (*ObjectAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case signed == nil:
		return nil, errors.New("api: no presigner, and an object route that signed nothing would serve every object to anybody")
	}
	s := &ObjectAPI{signed: signed}

	// The one sentence that puts these outside the authorisation hook. It is long because
	// the guard demands it be, and it is the right demand: this is the fourth public route
	// in the design and the first one whose authorisation is a signature rather than a
	// principal.
	why := Public{Why: "a presigned URL is itself the authorisation: it names one method, one object, one run and one instant, and it is signed by the installation, so asking for a credential here as well would mean the runner holding a standing object-store credential, which is the thing presigning exists to remove"}
	// Outside /api/v1, because a presigned URL is not a call of the API: it stands where the
	// object store's own URL stands wherever there is a real store, and a runner follows it
	// without knowing which of the two it holds. Under /api/v1 it was also a pair of routes no
	// router could hold beside the rest: /api/v1/objects/{key...} and /api/v1/{namespace}/runs
	// both match /api/v1/objects/runs, neither is the more specific, and net/http refuses to
	// serve the two together.
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
		s.store(w, r, key, r.Body)
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
		s.store(w, r, key, part)
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

func (s *ObjectAPI) store(w http.ResponseWriter, r *http.Request, key string, body io.Reader) {
	err := s.signed.Store(r.Context(), key, body)
	switch {
	case errors.Is(err, artifact.ErrWrongDigest):
		nothing(w, http.StatusBadRequest)
	case errors.Is(err, artifact.ErrTooLarge):
		nothing(w, http.StatusRequestEntityTooLarge)
	case err != nil:
		nothing(w, http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusCreated)
	}
}

// nothing answers with a status and no body. There is nothing worth writing: the caller is a
// machine following a URL, and a body it would not read is a body that only helps somebody
// probing.
func nothing(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
