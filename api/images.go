package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
)

// What a workflow repository's pushes are judged against beyond their own tree: the digest each
// image tag its steps name is pinned to, and the brick manifest of each image a brick step runs.
//
// A git push carries neither, and the hook reaches no registry to find them: "a server runs every
// image by the digest its tag was pinned to", and agk push resolves a tag "to the registry digest
// the pushing daemon holds for it". So agk push records what it resolved here, with the manifest it
// read out of each brick's image, before it pushes with git, and a plain git push passes where
// every tag it names is pinned and every brick image it runs has a manifest. The tree push records
// the pins of each version it makes as well, and no manifest: what it carries of one is the document
// a version keeps, a parameter's boolean required lifted out, and not the file.
//
// Per repository, under workflow:write on it, which is what registering a version of it takes:
// a pin decides which bytes the next version runs, with that workflow's secrets, and held per
// namespace it would let an editor of one workflow choose them for another.

// ImagePin is one pin as the routes answer it.
type ImagePin struct {
	Reference string    `json:"reference"`
	Image     string    `json:"image"`
	PinnedBy  string    `json:"pinned_by"`
	PinnedAt  time.Time `json:"pinned_at"`
}

// BrickManifest is one manifest as the routes answer it: which image, the SHA-256 of the manifest as
// it is kept, and who recorded it when. The manifest itself is the file the image holds, which is
// where to read it, and a listing carrying every one a repository was ever given would grow with
// every release of every brick it runs; its digest says whether what is kept is that file, and lets
// agk push leave out a manifest the repository already holds.
type BrickManifest struct {
	Image      string    `json:"image"`
	SHA256     string    `json:"sha256"`
	RecordedBy string    `json:"recorded_by"`
	RecordedAt time.Time `json:"recorded_at"`
}

// answeredManifest is a kept manifest as the routes answer it.
func answeredManifest(m db.BrickManifest) BrickManifest {
	return BrickManifest{Image: m.Image, SHA256: m.SHA256, RecordedBy: m.RecordedBy, RecordedAt: m.RecordedAt}
}

// Images is what GET and POST /api/v1/{ns}/workflows/{name}/images answer.
type Images struct {
	Pins      []ImagePin      `json:"pins"`
	Manifests []BrickManifest `json:"manifests"`
}

// RecordImages is what POST /api/v1/{ns}/workflows/{name}/images carries: the digest each tag is
// pinned to, and the manifest of each image by digest, as its text.
type RecordImages struct {
	Pins      map[string]string `json:"pins,omitempty"`
	Manifests map[string]string `json:"manifests,omitempty"`
}

// imagesMost is how many pins, and how many manifests, one recording carries: 256, one per image a
// workflow names, and a workflow naming more images than that is not one anybody reviews. Bounded
// as the images of a push are, so that a body of a million empty entries is refused at the entry
// past the bound rather than once it is a map.
const imagesMost = 256

// imagesMaxBytes is how large a recording may be: 8 MiB, room for 256 references and the manifests
// of the images a workflow runs, which the standard catalog's are a few kibibytes each of.
const imagesMaxBytes = 8 << 20

func (ri *RecordImages) field(b *body, name string) error {
	switch name {
	case "pins":
		tooMany := fmt.Sprintf("this recording pins more than the %d image tags it may, one per image a workflow names", imagesMost)
		return b.object(imagesMost, tooMany, func(reference string) error {
			if _, held := ri.Pins[reference]; held {
				return twice("the image", reference)
			}
			var image string
			if err := text(b, &image); err != nil {
				return err
			}
			if ri.Pins == nil {
				ri.Pins = map[string]string{}
			}
			ri.Pins[reference] = image
			return nil
		})
	case "manifests":
		tooMany := fmt.Sprintf("this recording carries more than the %d brick manifests it may, one per image a workflow runs", imagesMost)
		return b.object(imagesMost, tooMany, func(image string) error {
			if _, held := ri.Manifests[image]; held {
				return twice("the manifest of", image)
			}
			var manifest string
			if err := text(b, &manifest); err != nil {
				return err
			}
			if ri.Manifests == nil {
				ri.Manifests = map[string]string{}
			}
			ri.Manifests[image] = manifest
			return nil
		})
	}
	return unknown(name)
}

// listImages answers the repository's pins and manifests.
func (s *Server) listImages(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var pins []db.ImagePin
	var manifests []db.BrickManifest
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		pins, manifests, err = ns.ImagesOf(ctx, over.Workflow, nil)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the image pins and brick manifests could not be read")
		return
	}
	out := Images{Pins: make([]ImagePin, 0, len(pins)), Manifests: make([]BrickManifest, 0, len(manifests))}
	for _, p := range pins {
		out.Pins = append(out.Pins, ImagePin(p))
	}
	for _, m := range manifests {
		out.Manifests = append(out.Manifests, answeredManifest(m))
	}
	write(w, http.StatusOK, out)
}

// recordImages records the pins and manifests a request carries, and leaves every other one of the
// repository as it was.
func (s *Server) recordImages(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	var ri RecordImages
	if err := readAtMost(r, &ri, imagesMaxBytes); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a recording is at most %d bytes, room for the pins and brick manifests of %d images, and this one is larger", imagesMaxBytes, imagesMost))
			return
		}
		fail(w, statusOf(err), err.Error())
		return
	}
	if len(ri.Pins)+len(ri.Manifests) == 0 {
		fail(w, http.StatusBadRequest, "this recording names no pin and no manifest, and records nothing")
		return
	}
	images, err := checkImages(ri)
	if err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	for _, image := range slices.Sorted(maps.Keys(images.Manifests)) {
		// Held to what a version is made under, the bound a port is written to included, so
		// that a manifest the hook would refuse a push over is refused here, where it is sent,
		// naming the image it came from. Kept as it was sent, the file the image holds, rather
		// than as the document a version keeps of it: that one lifts a parameter's boolean
		// required out, and the hook has to hold a step to the parameters it must supply.
		if _, err := brick.ParseManifest(images.Manifests[image]); err != nil {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("the brick manifest of %s: %s", image, err))
			return
		}
	}

	var recorded db.ImagesRecorded
	var pins []db.ImagePin
	var manifests []db.BrickManifest
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if recorded, err = ns.RecordImages(ctx, over.Workflow, string(who), s.now(), images); err != nil {
			return err
		}
		if err := auditImages(ctx, ns, who, over.Workflow, recorded); err != nil {
			return err
		}
		pins, manifests, err = ns.ImagesOf(ctx, over.Workflow, &images)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoWorkflow):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the image pins and brick manifests could not be recorded")
		return
	}

	out := Images{Pins: []ImagePin{}, Manifests: []BrickManifest{}}
	for _, p := range pins {
		if _, named := images.Pins[p.Reference]; named {
			out.Pins = append(out.Pins, ImagePin(p))
		}
	}
	for _, m := range manifests {
		if _, named := images.Manifests[m.Image]; named {
			out.Manifests = append(out.Manifests, answeredManifest(m))
		}
	}
	write(w, http.StatusOK, out)
}

// checkImages refuses a pin or a manifest the store could not hold or the hook could not use, and
// answers the rest as the store takes them.
func checkImages(ri RecordImages) (db.Images, error) {
	images := db.Images{Pins: map[string]string{}, Manifests: map[string][]byte{}}
	for _, reference := range slices.Sorted(maps.Keys(ri.Pins)) {
		image := ri.Pins[reference]
		if err := imageReference(reference); err != nil {
			return db.Images{}, err
		}
		if agk.ImageRepository(reference) == "" {
			return db.Images{}, fmt.Errorf("%.64q names no repository, only a tag: an image is named by its repository and the tag after it", reference)
		}
		if agk.ImageByDigest(reference) || strings.Contains(reference, "@") {
			return db.Images{}, fmt.Errorf("%.64q is pinned, and it names its image by a digest already: a pin is for an image a step names by a tag, which moves, and a digest does not", reference)
		}
		if err := imageReference(image); err != nil {
			return db.Images{}, err
		}
		if !agk.ImageByDigest(image) || agk.ImageRepository(image) == "" {
			return db.Images{}, fmt.Errorf("%s is pinned to %.64q, and a tag is pinned to its image by digest, name@sha256: and sixty-four lowercase hexadecimal characters", reference, image)
		}
		// The same check the validation makes of a pin it is answered, so that a pin the hook
		// would refuse to believe is refused where it is written.
		if agk.ImageRepository(image) != agk.ImageRepository(reference) {
			return db.Images{}, fmt.Errorf("%s is pinned to %s, of another repository: a tag is pinned to a digest of its own repository, %s, since the digest is what is pulled from where the step says the image is", reference, image, agk.ImageRepository(reference))
		}
		images.Pins[reference] = db.CanonicalImage(image)
	}
	for _, image := range slices.Sorted(maps.Keys(ri.Manifests)) {
		if err := imageReference(image); err != nil {
			return db.Images{}, err
		}
		if !agk.ImageByDigest(image) || agk.ImageRepository(image) == "" {
			return db.Images{}, fmt.Errorf("a manifest is recorded for %.64q, and a manifest is recorded for an image by its repository and its digest: a tag moves, and the manifest is the one inside the image a digest names", image)
		}
		manifest := ri.Manifests[image]
		if manifest == "" {
			return db.Images{}, fmt.Errorf("the brick manifest of %s is empty, and a brick's image holds one at /agk/brick.yaml", image)
		}
		if len(manifest) > db.ManifestMaxBytes {
			return db.Images{}, &tooLarge{reason: fmt.Sprintf("the brick manifest of %s is %d bytes, and one is at most %d: sixty times the largest of the standard catalog, so that one with a large parameter schema fits", image, len(manifest), db.ManifestMaxBytes)}
		}
		canonical := db.CanonicalImage(image)
		if _, twice := images.Manifests[canonical]; twice {
			return db.Images{}, fmt.Errorf("%s is given two manifests, under two spellings of one image: a tag written beside a digest names the image the digest names", canonical)
		}
		images.Manifests[canonical] = []byte(manifest)
	}
	return images, nil
}

// imageReference refuses a reference no registry names an image with, or one past the bound the
// store holds a reference to.
func imageReference(ref string) error {
	if len(ref) > db.ImageReferenceMaxBytes {
		return fmt.Errorf("an image reference is at most %d bytes, the 255 characters registries hold a host and a repository's name to together, a colon and a tag of at most 128, and this one is %d", db.ImageReferenceMaxBytes, len(ref))
	}
	if ref == "" || strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return fmt.Errorf("%.64q is not an image reference: a reference is printable ASCII, as a registry's grammar of a name and a tag is, with no space", ref)
	}
	return nil
}

// auditImages records each pin a recording created or moved, as image.pin, and each manifest it
// recorded or replaced, as image.manifest, in the namespace: a pin decides which bytes the next
// version of the workflow runs, with its secrets, and a manifest what that version holds of the
// brick, its ports, its parameters and where its secrets are mounted, so who changed either is what
// the log is for. What was named again as it stood changed nothing and is not recorded.
func auditImages(ctx context.Context, ns *db.NS, who Principal, workflow string, recorded db.ImagesRecorded) error {
	for _, m := range recorded.Pins {
		detail := map[string]any{"reference": m.Reference, "image": m.Image}
		if m.Was != "" {
			detail["was"] = m.Was
		}
		if err := ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.ImagePin, Target: workflow, Result: audit.Done, Detail: detail}); err != nil {
			return err
		}
	}
	for _, m := range recorded.Manifests {
		detail := map[string]any{"image": m.Image, "sha256": m.SHA256}
		if m.Was != "" {
			detail["was"] = m.Was
		}
		if err := ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.ImageManifest, Target: workflow, Result: audit.Done, Detail: detail}); err != nil {
			return err
		}
	}
	return nil
}

// versionPins are the pins a version was judged with, as its repository's store keeps them: what the
// tree push records with each new version, so that a git push of the next commit finds them.
//
// Its manifests are not recorded. What a tree push carries of a manifest is the document a version
// keeps, with a parameter's boolean required lifted out, which is not the file the image holds and
// could not hold a step to the parameters it must supply; agk push records the file itself.
//
// A pin the store could not hold is left out, a reference past its bound or holding a space: the
// version keeps it all the same, as it always has, and a git push naming that reference finds no pin
// for it, which is what it finds for any reference nobody pinned.
func versionPins(v db.Version) db.Images {
	images := db.Images{Pins: map[string]string{}}
	for reference, image := range v.Images {
		if imageReference(reference) == nil && imageReference(image) == nil && !strings.Contains(reference, "@") && agk.ImageRepository(reference) != "" &&
			agk.ImageByDigest(image) && agk.ImageRepository(image) == agk.ImageRepository(reference) {
			images.Pins[reference] = db.CanonicalImage(image)
		}
	}
	return images
}
