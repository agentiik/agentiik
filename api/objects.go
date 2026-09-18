package api

import (
	"errors"
	"io"
	"io/fs"
	"net/http"

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

// NewObjects registers the two object routes.
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
	for _, r := range []struct{ method, pattern string }{
		{"GET", "/api/v1/objects/{key...}"},
		{"PUT", "/api/v1/objects/{key...}"},
	} {
		if err := rt.Handle(r.method, r.pattern, why, s.object); err != nil {
			return nil, err
		}
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
		s.store(w, r, key)
	}
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

func (s *ObjectAPI) store(w http.ResponseWriter, r *http.Request, key string) {
	err := s.signed.Store(r.Context(), key, r.Body)
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
