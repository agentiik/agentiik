package db

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// What a workflow repository's pushes are judged against beyond their own tree: the digest each
// image tag its steps name is pinned to, and the brick manifest of each image a brick step runs.
//
// A git push carries neither, and the hook reaches no registry: agk push resolves each tag on the
// pusher's own Docker daemon and reads each brick's manifest there, and records both here before it
// pushes with git, as the tree push records those of each version it makes. The hook reads them
// through Pin and Manifest. Kept per repository rather than per namespace, so that writing a pin
// takes what registering a version of that workflow takes and reaches no other workflow: see
// migration 0050.

// ErrNoPin is an image tag the repository holds no digest for.
var ErrNoPin = errors.New("db: the repository holds no digest for that image tag")

// ErrNoManifest is an image the repository holds no brick manifest for.
var ErrNoManifest = errors.New("db: the repository holds no brick manifest for that image")

// ImagePin is one image tag of a repository and the reference by digest it is pinned to.
type ImagePin struct {
	// Reference is the image as a step writes it, by a tag, and Image the reference by digest of
	// the same repository it is pinned to, name@sha256:<hex>.
	Reference string
	Image     string

	PinnedBy string
	PinnedAt time.Time
}

// BrickManifest is the brick manifest of one image of a repository, by digest, as the file the image
// holds at /agk/brick.yaml, byte for byte.
type BrickManifest struct {
	Image    string
	Manifest []byte

	RecordedBy string
	RecordedAt time.Time
}

// PinMoved is a pin RecordImages created or moved: Was is the digest it named before, and empty
// for a pin it created.
type PinMoved struct {
	Reference string
	Image     string
	Was       string
}

// Images are what RecordImages is asked to record: pins by reference, and manifests by the
// reference by digest of their image.
type Images struct {
	Pins      map[string]string
	Manifests map[string][]byte
}

// Pin answers the reference by digest the repository's tag is pinned to, or ErrNoPin.
func (n *NS) Pin(ctx context.Context, workflow, reference string) (string, error) {
	var image string
	err := n.tx.QueryRow(ctx,
		`select image from image_pins where namespace = $1 and workflow = $2 and reference = $3`,
		n.namespace, workflow, reference).Scan(&image)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s in %s", ErrNoPin, reference, workflow)
	}
	if err != nil {
		return "", fmt.Errorf("db: the pin of %s in %s could not be read: %w", reference, workflow, err)
	}
	return image, nil
}

// Manifest answers the brick manifest the repository holds for an image by digest, a tag written
// beside the digest or not, or ErrNoManifest.
func (n *NS) Manifest(ctx context.Context, workflow, image string) ([]byte, error) {
	image = CanonicalImage(image)
	var manifest []byte
	err := n.tx.QueryRow(ctx,
		`select manifest from brick_manifests where namespace = $1 and workflow = $2 and image = $3`,
		n.namespace, workflow, image).Scan(&manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s in %s", ErrNoManifest, image, workflow)
	}
	if err != nil {
		return nil, fmt.Errorf("db: the manifest of %s in %s could not be read: %w", image, workflow, err)
	}
	return manifest, nil
}

// ImagesOf answers every pin of the repository, by reference, and every manifest, by image, in
// C's collation, byte by byte, whatever collation the database was created with. ErrNoWorkflow
// where the namespace holds no workflow of that name.
func (n *NS) ImagesOf(ctx context.Context, workflow string) ([]ImagePin, []BrickManifest, error) {
	if err := n.workflowHeld(ctx, workflow, ""); err != nil {
		return nil, nil, err
	}
	rows, err := n.tx.Query(ctx,
		`select reference, image, pinned_by, pinned_at from image_pins
		 where namespace = $1 and workflow = $2 order by reference collate "C"`,
		n.namespace, workflow)
	if err != nil {
		return nil, nil, fmt.Errorf("db: the pins of %s could not be read: %w", workflow, err)
	}
	pins, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ImagePin, error) {
		var p ImagePin
		err := row.Scan(&p.Reference, &p.Image, &p.PinnedBy, &p.PinnedAt)
		p.PinnedAt = p.PinnedAt.UTC()
		return p, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("db: the pins of %s could not be read: %w", workflow, err)
	}
	rows, err = n.tx.Query(ctx,
		`select image, manifest, recorded_by, recorded_at from brick_manifests
		 where namespace = $1 and workflow = $2 order by image collate "C"`,
		n.namespace, workflow)
	if err != nil {
		return nil, nil, fmt.Errorf("db: the manifests of %s could not be read: %w", workflow, err)
	}
	manifests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BrickManifest, error) {
		var m BrickManifest
		err := row.Scan(&m.Image, &m.Manifest, &m.RecordedBy, &m.RecordedAt)
		m.RecordedAt = m.RecordedAt.UTC()
		return m, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("db: the manifests of %s could not be read: %w", workflow, err)
	}
	return pins, manifests, nil
}

// RecordImages records pins and manifests of one repository as by at at, now where at is zero, all
// of them or none, and leaves every other pin and manifest of the repository as it was, so that two
// pushes of one workflow never drop each other's. It answers the pins it created or moved, in
// reference order: a pin named again at the digest it holds is left alone, its pinned_by and
// pinned_at included, and a manifest recorded again with the same bytes is too.
//
// A manifest recorded again with other bytes replaces the one held. The runner reads the manifest
// out of the image itself when a step runs, so what is kept here is only what a push is judged
// against, and one recorded wrong, by a client with a bug, is put right by recording it again
// rather than left to refuse every push after it. A failure keeps nothing of the set, even where the
// caller's transaction goes on and commits: the set is written under a savepoint of its own, as
// UpdateRefs writes a push's refs. ErrNoWorkflow where the namespace holds no workflow of that name.
//
// Every row is written in reference and image order, so that two sets naming the same entries lock
// them in one order and never deadlock.
func (n *NS) RecordImages(ctx context.Context, workflow, by string, at time.Time, images Images) ([]PinMoved, error) {
	if by == "" {
		return nil, fmt.Errorf("db: the images of %s recorded by nobody", workflow)
	}
	for reference, image := range images.Pins {
		if !imageTag.MatchString(reference) || len(reference) > ImageReferenceMaxBytes {
			return nil, fmt.Errorf("db: %q is not an image named by a tag, of at most %d bytes", reference, ImageReferenceMaxBytes)
		}
		if !imageDigest.MatchString(image) || len(image) > ImageReferenceMaxBytes {
			return nil, fmt.Errorf("db: %s is pinned to %q, which is no image named by a digest", reference, image)
		}
	}
	for image, manifest := range images.Manifests {
		if !imageDigest.MatchString(image) || len(image) > ImageReferenceMaxBytes {
			return nil, fmt.Errorf("db: a manifest recorded for %q, which is no image named by a digest", image)
		}
		if len(manifest) == 0 || len(manifest) > ManifestMaxBytes {
			return nil, fmt.Errorf("db: the manifest of %s is %d bytes, where one is 1 to %d", image, len(manifest), ManifestMaxBytes)
		}
	}
	// Kept by the image's canonical reference, so that a step writing a tag beside the digest
	// finds what was recorded without one, and one pin names one image however it was written.
	canonical := Images{Pins: make(map[string]string, len(images.Pins)), Manifests: make(map[string][]byte, len(images.Manifests))}
	for reference, image := range images.Pins {
		canonical.Pins[reference] = CanonicalImage(image)
	}
	for image, manifest := range images.Manifests {
		image = CanonicalImage(image)
		if _, twice := canonical.Manifests[image]; twice {
			return nil, fmt.Errorf("db: the manifest of %s is recorded twice by one set, under two spellings of its image", image)
		}
		canonical.Manifests[image] = manifest
	}
	images = canonical
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// The lock a push takes, FOR NO KEY UPDATE on the workflow's row, so that two sets recording
	// images of one repository take turns, as two pushes do, and each reads what the other left
	// rather than racing it to a row; the key share a version, a grant or a run takes on the row
	// does not wait for it. A workflow deleted meanwhile is ErrNoWorkflow here rather than a
	// foreign key's violation in the middle of the set.
	if err := n.workflowHeld(ctx, workflow, "for no key update"); err != nil {
		return nil, err
	}

	set, err := n.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: the images of %s could not be recorded: %w", workflow, err)
	}
	moved, err := recordImages(ctx, set, n.namespace, workflow, by, at, images)
	if err != nil {
		rollback, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackWithin)
		rerr := set.Rollback(rollback)
		stop()
		if rerr != nil {
			return nil, errors.Join(err, fmt.Errorf("db: the images of %s recorded before it could not be put back: %w", workflow, rerr))
		}
		return nil, err
	}
	if err := set.Commit(ctx); err != nil {
		return nil, fmt.Errorf("db: the images of %s could not be recorded: %w", workflow, err)
	}
	return moved, nil
}

func recordImages(ctx context.Context, tx pgx.Tx, namespace, workflow, by string, at time.Time, images Images) ([]PinMoved, error) {
	var moved []PinMoved
	for _, reference := range slices.Sorted(maps.Keys(images.Pins)) {
		image := images.Pins[reference]
		var was string
		err := tx.QueryRow(ctx,
			`select image from image_pins where namespace = $1 and workflow = $2 and reference = $3`,
			namespace, workflow, reference).Scan(&was)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx,
				`insert into image_pins (namespace, workflow, reference, image, pinned_by, pinned_at)
				 values ($1, $2, $3, $4, $5, $6)`,
				namespace, workflow, reference, image, by, at)
		case err != nil:
			return nil, fmt.Errorf("db: the pin of %s in %s could not be read: %w", reference, workflow, err)
		case was == image:
			continue
		default:
			_, err = tx.Exec(ctx,
				`update image_pins set image = $4, pinned_by = $5, pinned_at = $6
				 where namespace = $1 and workflow = $2 and reference = $3`,
				namespace, workflow, reference, image, by, at)
		}
		if err != nil {
			return nil, fmt.Errorf("db: %s could not be pinned in %s: %w", reference, workflow, err)
		}
		moved = append(moved, PinMoved{Reference: reference, Image: image, Was: was})
	}

	for _, image := range slices.Sorted(maps.Keys(images.Manifests)) {
		if _, err := tx.Exec(ctx,
			`insert into brick_manifests (namespace, workflow, image, manifest, recorded_by, recorded_at)
			 values ($1, $2, $3, $4, $5, $6)
			 on conflict (namespace, workflow, image) do update
			   set manifest = excluded.manifest, recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at
			 where brick_manifests.manifest <> excluded.manifest`,
			namespace, workflow, image, images.Manifests[image], by, at); err != nil {
			return nil, fmt.Errorf("db: the manifest of %s could not be recorded in %s: %w", image, workflow, err)
		}
	}
	return moved, nil
}

// The bounds and the grammar an image reference is held to here, which migration 0050 holds in SQL.
const (
	// ImageReferenceMaxBytes is the longest image reference kept, 384 bytes: the 255 characters
	// clients hold a registry's host and a repository's name to together, a colon, and a tag of at
	// most 128 characters, both from the OCI distribution specification.
	ImageReferenceMaxBytes = 384

	// ManifestMaxBytes is the largest brick manifest kept, 256 KiB: sixty times the largest of the
	// standard catalog, so that one with a large parameter schema fits and a row stays a row.
	ManifestMaxBytes = 256 << 10
)

// imageTag is an image named by a tag, or by nothing and so by latest, and imageDigest one named by
// a digest, as the wire's imageRef writes one. Both are printable ASCII, as the OCI distribution
// specification's grammar of a name and a tag is: nothing a registry names an image with is left
// out, a log line or a terminal reads nothing as something else, and a bound in bytes is the same
// bound in characters, which is how the API's schema writes it.
var (
	imageTag    = regexp.MustCompile(`^[\x21-\x3f\x41-\x7e]+$`)
	imageDigest = regexp.MustCompile(`^[\x21-\x3f\x41-\x7e]+@sha256:[0-9a-f]{64}$`)
)

// CanonicalImage is an image named by a digest as the store keys it: its repository and its digest,
// less any tag written beside the digest, since the digest is what is pulled and alpine:3.21@sha256:
// and alpine@sha256: of one digest are one image. An image named otherwise is answered as it is.
func CanonicalImage(image string) string {
	at := strings.IndexByte(image, '@')
	if at < 0 {
		return image
	}
	return agk.ImageRepository(image[:at]) + image[at:]
}

// workflowHeld answers ErrNoWorkflow where the namespace holds no workflow of that name, taking the
// lock given on its row where it does.
func (n *NS) workflowHeld(ctx context.Context, workflow, lock string) error {
	var one int
	err := n.tx.QueryRow(ctx,
		`select 1 from workflows where namespace = $1 and name = $2 `+lock,
		n.namespace, workflow).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return fmt.Errorf("db: workflow %s could not be read: %w", workflow, err)
	}
	return nil
}
