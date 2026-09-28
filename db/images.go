package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// What a namespace knows of the images its workflows name.
//
// "A git push reaches no registry": the pre-receive hook judges a pushed commit by the one
// validation agk push makes, and two things that validation reads out of images, the digest a tag
// names and the manifest a brick step is held to, are read here instead. agk push and agk validate
// record them from what the daemon on their machine read, through PUT /api/v1/{ns}/images, and
// migration 0050 recorded them from every version stored before it.

// ImageReferenceMaxBytes is the longest reference the namespace records a pin or a manifest under.
//
// The distribution specification holds a tag to 128 characters, and its clients a repository's
// name, its registry's host included, to 255, so no reference a registry serves is longer than 512
// bytes, and the bound keeps each one a key an index holds. image_reference in migration 0050
// holds the tables to it.
const ImageReferenceMaxBytes = 512

// ErrNotRecorded is a tag the namespace holds no pin for, or an image it holds no manifest of.
var ErrNotRecorded = errors.New("db: the namespace has recorded nothing for that image")

// ImagePin is one tag of the namespace, pinned.
type ImagePin struct {
	// Reference is the tag as a workflow writes it, and Pinned the digest it was last pinned
	// to, written name@sha256:<hex> in the tag's own repository.
	Reference string
	Pinned    string

	// PinnedBy and PinnedAt are who pinned it to that digest and when: pinning it again to the
	// same digest changes neither, so they say when the tag last moved and who moved it.
	PinnedBy string
	PinnedAt time.Time
}

// ImageManifest is the brick manifest recorded for one image.
type ImageManifest struct {
	// Image is the image, named by its repository at its digest, and Document the manifest
	// read out of it, as brick.Manifest keeps it.
	Image    string
	Document []byte

	RecordedBy string
	RecordedAt time.Time
}

// RecordImages records tags pinned to digests, by the reference each tag is written as, and the
// manifests of images, by the image each was read out of, named by digest, as by recorded them at
// at. A tag pinned already takes the new digest, and a manifest recorded already the new bytes,
// each with who sent it; one sent again as it is recorded is left as it was.
//
// An image is recorded at its repository and digest, a tag written beside the digest left out
// (agk.ImageAtDigest), and two names of one image sent with two manifests are refused rather than
// one of them chosen. The rows are written in the order of their keys, so that two records sharing
// images, which each lock the rows they write, never lock them in two orders and deadlock.
func (n *NS) RecordImages(ctx context.Context, pins map[string]string, manifests map[string][]byte, by string, at time.Time) error {
	if by == "" {
		return errors.New("db: images are recorded by nobody")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	kept := make(map[string][]byte, len(manifests))
	for _, image := range slices.Sorted(maps.Keys(manifests)) {
		key := agk.ImageAtDigest(image)
		if held, twice := kept[key]; twice && !bytes.Equal(held, manifests[image]) {
			return fmt.Errorf("db: two manifests of %s were sent, one image under two names", key)
		}
		kept[key] = manifests[image]
	}

	references := slices.Sorted(maps.Keys(pins))
	pinned := make([]string, len(references))
	for i, ref := range references {
		pinned[i] = pins[ref]
	}
	if len(references) > 0 {
		if _, err := n.tx.Exec(ctx,
			`insert into image_pins (namespace, reference, pinned, pinned_by, pinned_at)
			 select $1, reference, pinned, $4, $5
			   from unnest($2::text[], $3::text[]) as sent (reference, pinned)
			  order by reference
			 on conflict (namespace, reference) do update
			   set pinned = excluded.pinned, pinned_by = excluded.pinned_by, pinned_at = excluded.pinned_at
			   where image_pins.pinned <> excluded.pinned`,
			n.namespace, references, pinned, by, at); err != nil {
			return fmt.Errorf("db: the image pins could not be recorded: %w", err)
		}
	}

	images := slices.Sorted(maps.Keys(kept))
	documents := make([][]byte, len(images))
	for i, image := range images {
		documents[i] = kept[image]
	}
	if len(images) > 0 {
		if _, err := n.tx.Exec(ctx,
			`insert into brick_manifests (namespace, image, manifest, recorded_by, recorded_at)
			 select $1, image, manifest, $4, $5
			   from unnest($2::text[], $3::bytea[]) as sent (image, manifest)
			  order by image
			 on conflict (namespace, image) do update
			   set manifest = excluded.manifest, recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at
			   where brick_manifests.manifest <> excluded.manifest`,
			n.namespace, images, documents, by, at); err != nil {
			return fmt.Errorf("db: the brick manifests could not be recorded: %w", err)
		}
	}
	return nil
}

// Pinned is the digest the namespace last pinned a tag to, or ErrNotRecorded.
func (n *NS) Pinned(ctx context.Context, reference string) (ImagePin, error) {
	p := ImagePin{Reference: reference}
	err := n.tx.QueryRow(ctx,
		`select pinned, pinned_by, pinned_at from image_pins where namespace = $1 and reference = $2`,
		n.namespace, reference).Scan(&p.Pinned, &p.PinnedBy, &p.PinnedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImagePin{}, fmt.Errorf("%w: %s", ErrNotRecorded, reference)
	}
	if err != nil {
		return ImagePin{}, fmt.Errorf("db: the pin of %s could not be read: %w", reference, err)
	}
	p.PinnedAt = p.PinnedAt.UTC()
	return p, nil
}

// Manifest is the brick manifest the namespace recorded for an image named by digest, a tag
// written beside the digest or not, or ErrNotRecorded.
func (n *NS) Manifest(ctx context.Context, image string) (ImageManifest, error) {
	m := ImageManifest{Image: agk.ImageAtDigest(image)}
	err := n.tx.QueryRow(ctx,
		`select manifest, recorded_by, recorded_at from brick_manifests where namespace = $1 and image = $2`,
		n.namespace, m.Image).Scan(&m.Document, &m.RecordedBy, &m.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImageManifest{}, fmt.Errorf("%w: %s", ErrNotRecorded, m.Image)
	}
	if err != nil {
		return ImageManifest{}, fmt.Errorf("db: the manifest of %s could not be read: %w", m.Image, err)
	}
	m.RecordedAt = m.RecordedAt.UTC()
	return m, nil
}
