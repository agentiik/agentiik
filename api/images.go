package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
)

// What a namespace knows of the images its workflows name.
//
// "A git push reaches no registry": the pre-receive hook judges a pushed commit by the one
// validation agk push makes, and what that validation reads out of images, the digest a tag names
// and the manifest a brick step is held to, it reads from what the namespace has recorded instead.
// agk push and agk validate record it here, from what the daemon on their machine read, with PUT
// /api/v1/{ns}/images: a tag or an image the namespace holds nothing for is refused at the push,
// naming agk push. Nothing here reaches a registry either, so what is recorded is what a principal
// allowed to write the namespace's workflows says it read, as a version's own images and manifests
// have always been what agk push said: every run of a version keeps the digests its push recorded.
//
// Under workflow:write on the namespace rather than on a workflow, since the path names none and
// what is recorded is what every workflow of the namespace is judged against: a principal holding
// workflow:write on one workflow alone does not move the digest another's pushes run.
//
// Not audited. What is recorded keeps who recorded it and when beside it, as a version keeps who
// pushed it, and a push, which is what a record is part of, is not an act the audit log names.

// Images is a record of what a daemon read out of the images a workflow names.
type Images struct {
	// Pins are tags, each by the reference a workflow writes, with the digest its registry
	// serves it under, name@sha256:<hex> in the tag's own repository, which the daemon that
	// read it holds it by.
	Pins map[string]string `json:"pins,omitempty"`

	// Manifests are brick manifests, each by the image it was read out of named by digest,
	// as the JSON document brick.Manifest keeps.
	Manifests map[string][]byte `json:"manifests,omitempty"`
}

func (m *Images) field(b *body, name string) error {
	switch name {
	case "pins":
		tooMany := fmt.Sprintf("this record pins more tags than the %d a push resolves, one per image a workflow names by tag", imagesMax)
		return b.object(imagesMax, tooMany, func(ref string) error {
			if _, held := m.Pins[ref]; held {
				return twice("the tag", ref)
			}
			var pinned string
			if err := text(b, &pinned); err != nil {
				return err
			}
			if m.Pins == nil {
				m.Pins = map[string]string{}
			}
			m.Pins[ref] = pinned
			return nil
		})
	case "manifests":
		return files(b, &m.Manifests, imagesMax, "the manifest of",
			fmt.Sprintf("this record carries more manifests than the %d a push carries, one per image a workflow names", imagesMax))
	}
	return unknown(name)
}

// imagesMaxBytes and imagesMax are how large a record may be, and how many pins and how many
// manifests it carries: a push's, pushMaxBytes and TreeMaxFiles. What agk push records here is what
// the version it pushes carries beside its tree, so a record is held to what a push holds those
// to, and no push the version route takes is one whose images this route refuses.
const (
	imagesMaxBytes = pushMaxBytes
	imagesMax      = TreeMaxFiles
)

// ImageOptions are what the image routes are given.
type ImageOptions struct {
	Pool *db.Pool

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time
}

// ImageAPI serves /api/v1/{ns}/images.
type ImageAPI struct {
	pool *db.Pool
	now  func() time.Time
}

// NewImages registers the image routes on a router.
func NewImages(rt *Router, o ImageOptions) (*ImageAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and an image recorded is a row")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &ImageAPI{pool: o.Pool, now: o.Now}
	if err := rt.Handle("PUT", "/api/v1/{namespace}/images", Needs{Permission: WorkflowWrite, Scope: Namespace}, s.record); err != nil {
		return nil, err
	}
	return s, nil
}

// record keeps what a record carries, whole or not at all, and answers 204: what it records is
// what it was sent, a tag pinned already taking the new digest and a manifest recorded already the
// new bytes, and one sent as it is recorded left as it was.
func (s *ImageAPI) record(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var m Images
	if err := readAtMost(r, &m, imagesMaxBytes); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a record of images is at most %d bytes, the most a push carries, and this one is larger", imagesMaxBytes))
			return
		}
		fail(w, statusOf(err), err.Error())
		return
	}
	if status, err := checkImages(m); err != nil {
		fail(w, status, err.Error())
		return
	}
	if len(m.Pins)+len(m.Manifests) > 0 {
		err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
			return ns.RecordImages(ctx, m.Pins, m.Manifests, string(who), s.now())
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, "the images could not be recorded")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkImages refuses a record holding anything the namespace's store would not keep, before
// anything is written, and answers the status the refusal is answered with: 400 for something
// that is no reference, and 422 for a pin or a manifest that is not what it says.
func checkImages(m Images) (int, error) {
	for _, ref := range slices.Sorted(maps.Keys(m.Pins)) {
		pinned := m.Pins[ref]
		if err := imageReference(ref); err != nil {
			return http.StatusBadRequest, err
		}
		if strings.Contains(ref, "@") {
			return http.StatusBadRequest, fmt.Errorf("%s names a digest, and a pin is a tag's: an image named by digest runs that digest and is pinned to nothing", ref)
		}
		if err := imageReference(pinned); err != nil {
			return http.StatusBadRequest, err
		}
		if !agk.ImageByDigest(pinned) {
			return http.StatusBadRequest, fmt.Errorf("%s is pinned to %s, and a tag is pinned to a digest, written name@sha256: and sixty-four lowercase hexadecimal characters", ref, pinned)
		}
		if repository, _, _ := strings.Cut(pinned, "@"); repository != agk.ImageRepository(ref) {
			return http.StatusUnprocessableEntity, fmt.Errorf("%s is pinned to %s, and a tag is pinned to its own repository, %s, at a digest: that repository is what a run of the tag pulls", ref, pinned, agk.ImageRepository(ref))
		}
	}
	read := make(map[string][]byte, len(m.Manifests))
	for _, image := range slices.Sorted(maps.Keys(m.Manifests)) {
		if err := imageReference(image); err != nil {
			return http.StatusBadRequest, err
		}
		if !agk.ImageByDigest(image) {
			return http.StatusBadRequest, fmt.Errorf("a manifest is recorded by the image it was read out of, named by digest, and %s names none: a tag moves, and the manifest it names with it", image)
		}
		key := agk.ImageAtDigest(image)
		if first, twice := read[key]; twice && !bytes.Equal(first, m.Manifests[image]) {
			return http.StatusUnprocessableEntity, fmt.Errorf("this record carries two manifests of %s, one image under two names, and a digest names one image and so one manifest", key)
		}
		read[key] = m.Manifests[image]
		// Held to what a push holds a manifest to, since a push is what is judged against it:
		// one no push could be judged against is no manifest worth keeping.
		if _, err := brick.ParseManifest(m.Manifests[image]); err != nil {
			return http.StatusUnprocessableEntity, fmt.Errorf("the manifest of %s: %w", image, err)
		}
	}
	return 0, nil
}

// imageReference refuses what could not be a reference a registry serves: nothing, a space or a
// control character anywhere, or more bytes than db.ImageReferenceMaxBytes.
func imageReference(ref string) error {
	switch {
	case ref == "":
		return errors.New("an image is named by a reference, and this one is empty")
	case len(ref) > db.ImageReferenceMaxBytes:
		return fmt.Errorf("%.64q... is %d bytes, and an image reference is at most %d: the distribution specification holds a tag to 128 characters and its clients a repository's name to 255, so no reference a registry serves is longer", ref, len(ref), db.ImageReferenceMaxBytes)
	case strings.ContainsFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("%q holds a space or a control character, and no image reference does", ref)
	}
	return nil
}
