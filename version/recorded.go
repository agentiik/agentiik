package version

import (
	"context"
	"errors"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
)

// Recorded is Pin and Manifest over what a namespace has recorded of the images its workflows
// name: the digest each tag was last pinned to, and the manifest of each image, which agk push and
// agk validate record through PUT /api/v1/{ns}/images from what the daemon on their machine read,
// and which the migration that made the store filled from every version stored before it.
//
// What the pre-receive hook judges a pushed commit against: "a git push reaches no registry", so a
// tag is taken at the digest the namespace pinned it to and a brick step held to the manifest the
// namespace recorded for its image, and a tag or an image the namespace holds nothing for is
// refused naming agk push, which is what records them. The rest of Resolvers is the caller's.
//
// Each question is a transaction of its own, through the namespace's door, so that a store that
// cannot answer is an error and never an image held.
func Recorded(pool *db.Pool, namespace string) Resolvers {
	return Resolvers{
		Pin: func(ctx context.Context, reference string, _ agk.Step) (string, error) {
			var pin db.ImagePin
			err := pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				pin, err = ns.Pinned(ctx, reference)
				return err
			})
			if errors.Is(err, db.ErrNotRecorded) {
				return "", ErrNotHeld
			}
			return pin.Pinned, err
		},
		Manifest: func(ctx context.Context, image string, _ agk.Step) ([]byte, error) {
			var m db.ImageManifest
			err := pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
				var err error
				m, err = ns.Manifest(ctx, image)
				return err
			})
			if errors.Is(err, db.ErrNotRecorded) {
				return nil, ErrNotHeld
			}
			return m.Document, err
		},
	}
}
