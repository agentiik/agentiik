package api

import (
	"errors"
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
	s.signed.Serve(w, r, r.PathValue("key"))
}
