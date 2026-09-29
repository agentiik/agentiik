package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A workflow repository's image pins and brick manifests, against a real PostgreSQL: a set is
// recorded whole or not at all, a pin moves and a manifest is replaced, two sets for one repository
// take turns, and a namespace reads and writes its own repositories' alone.

var (
	pinnedTo = "ghcr.io/acme/agk-invoice@sha256:" + strings.Repeat("1", 64)
	movedTo  = "ghcr.io/acme/agk-invoice@sha256:" + strings.Repeat("2", 64)
	manifest = []byte(`{"apiVersion":"agentiik.dev/v1","kind":"Brick"}`)
)

const tagged = "ghcr.io/acme/agk-invoice:1.4.0"

func recording(t *testing.T, pool *Pool, namespace, by string, images Images) ([]PinMoved, error) {
	t.Helper()
	var moved []PinMoved
	err := pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
		var err error
		moved, err = n.RecordImages(ctx, "nightly", by, time.Time{}, images)
		return err
	})
	return moved, err
}

func imagesHeld(t *testing.T, pool *Pool, namespace string) ([]ImagePin, []BrickManifest) {
	t.Helper()
	var pins []ImagePin
	var manifests []BrickManifest
	if err := pool.In(t.Context(), namespace, func(ctx context.Context, n *NS) error {
		var err error
		pins, manifests, err = n.ImagesOf(ctx, "nightly")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pins, manifests
}

func TestAPinMovesAndAManifestIsReplaced(t *testing.T) {
	pool, _ := repositories(t)

	moved, err := recording(t, pool, "finance", "alice", Images{
		Pins: map[string]string{tagged: pinnedTo}, Manifests: map[string][]byte{pinnedTo: manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0] != (PinMoved{Reference: tagged, Image: pinnedTo}) {
		t.Errorf("a pin created is answered as %+v", moved)
	}

	// Again at the digest it holds, and with the same manifest: nothing moved, and the rows keep
	// who wrote them.
	if moved, err = recording(t, pool, "finance", "bob", Images{
		Pins: map[string]string{tagged: pinnedTo}, Manifests: map[string][]byte{pinnedTo: manifest},
	}); err != nil || len(moved) != 0 {
		t.Errorf("the same set recorded again moved %+v: %v", moved, err)
	}
	pins, manifests := imagesHeld(t, pool, "finance")
	if len(pins) != 1 || pins[0].PinnedBy != "alice" || len(manifests) != 1 || manifests[0].RecordedBy != "alice" {
		t.Errorf("the same set recorded again left %+v and %+v", pins, manifests)
	}

	if moved, err = recording(t, pool, "finance", "bob", Images{Pins: map[string]string{tagged: movedTo}}); err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0] != (PinMoved{Reference: tagged, Image: movedTo, Was: pinnedTo}) {
		t.Errorf("a pin moved is answered as %+v", moved)
	}

	// Another manifest for the image replaces the one held, and a set failing on a pin the store
	// cannot hold keeps nothing of itself, the manifest beside it included, even where the
	// transaction goes on and commits.
	another := []byte(`{"kind":"Brick"}`)
	if _, err := recording(t, pool, "finance", "carol", Images{Manifests: map[string][]byte{pinnedTo: another}}); err != nil {
		t.Fatal(err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		_, err := n.RecordImages(ctx, "nightly", "dan", time.Time{}, Images{
			Pins:      map[string]string{"ghcr.io/acme/agk-invoice:1.5.0": movedTo, "ghcr.io/acme/agk-invoice:" + strings.Repeat("9", ImageReferenceMaxBytes): movedTo},
			Manifests: map[string][]byte{pinnedTo: manifest},
		})
		if err == nil {
			t.Error("a set holding a pin past its bound was recorded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pins, manifests = imagesHeld(t, pool, "finance")
	if len(pins) != 1 || pins[0].Image != movedTo || pins[0].PinnedBy != "bob" || string(manifests[0].Manifest) != string(another) || manifests[0].RecordedBy != "carol" {
		t.Errorf("a set refused left %+v and %+v", pins, manifests)
	}

	var image string
	var document []byte
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *NS) error {
		var err error
		if image, err = n.Pin(ctx, "nightly", tagged); err != nil {
			return err
		}
		if document, err = n.Manifest(ctx, "nightly", pinnedTo); err != nil {
			return err
		}
		if _, err := n.Pin(ctx, "nightly", "ghcr.io/acme/agk-invoice:9"); !errors.Is(err, ErrNoPin) {
			t.Errorf("a tag nobody pinned is %v", err)
		}
		if _, err := n.Manifest(ctx, "nightly", movedTo); !errors.Is(err, ErrNoManifest) {
			t.Errorf("an image with no manifest is %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if image != movedTo || string(document) != string(another) {
		t.Errorf("the pin reads %s and the manifest %s", image, document)
	}
}

// A namespace reads and writes its own repositories' images alone, and a workflow it does not hold
// is ErrNoWorkflow, whether another namespace holds one of that name or not.
func TestARepositorysImagesAreItsNamespacesAlone(t *testing.T) {
	pool, _ := repositories(t)
	if _, err := recording(t, pool, "finance", "alice", Images{
		Pins: map[string]string{tagged: pinnedTo}, Manifests: map[string][]byte{pinnedTo: manifest},
	}); err != nil {
		t.Fatal(err)
	}
	if pins, manifests := imagesHeld(t, pool, "team-ops"); len(pins) != 0 || len(manifests) != 0 {
		t.Errorf("team-ops reads finance's images: %+v %+v", pins, manifests)
	}
	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, n *NS) error {
		if _, err := n.Pin(ctx, "nightly", tagged); !errors.Is(err, ErrNoPin) {
			t.Errorf("team-ops reads finance's pin: %v", err)
		}
		if _, _, err := n.ImagesOf(ctx, "absent"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("the images of a workflow nobody created are %v", err)
		}
		if _, err := n.RecordImages(ctx, "absent", "alice", time.Time{}, Images{Pins: map[string]string{tagged: pinnedTo}}); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("recording the images of a workflow nobody created is %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The grammar and the bounds are held here as well as at the route, so that nothing reaches the
// table the table would refuse with an error nobody can read.
func TestAnImageTheStoreCannotHoldIsRefusedBeforeTheTable(t *testing.T) {
	pool, super := repositories(t)
	for name, images := range map[string]Images{
		"a digest pinned":             {Pins: map[string]string{pinnedTo: pinnedTo}},
		"a tag pinned to a tag":       {Pins: map[string]string{tagged: tagged}},
		"a reference holding a space": {Pins: map[string]string{"ghcr.io/acme/a b:1": pinnedTo}},
		"a reference past its bound":  {Pins: map[string]string{"r/" + strings.Repeat("a", ImageReferenceMaxBytes) + ":1": pinnedTo}},
		"a manifest of a tag":         {Manifests: map[string][]byte{tagged: manifest}},
		"an empty manifest":           {Manifests: map[string][]byte{pinnedTo: {}}},
		"a manifest past its bound":   {Manifests: map[string][]byte{pinnedTo: make([]byte, ManifestMaxBytes+1)}},
	} {
		if _, err := recording(t, pool, "finance", "alice", images); err == nil {
			t.Errorf("%s was recorded", name)
		}
	}
	// And the table refuses them itself, to anything that would write around this package.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`insert into image_pins (namespace, workflow, reference, image, pinned_by, pinned_at)
		 values ('finance', 'nightly', 'a b:1', $1, 'alice', now())`, pinnedTo); err == nil {
		t.Error("the table holds a reference with a space")
	}
}

// Two sets recording images of one repository take turns, as two pushes do, so that each moves the
// pin from where the other left it and the log names each move once.
func TestTwoSetsForOneRepositoryTakeTurns(t *testing.T) {
	pool, _ := repositories(t)
	var wg sync.WaitGroup
	moves := make([][]PinMoved, 8)
	for i := range moves {
		wg.Go(func() {
			to := pinnedTo
			if i%2 == 1 {
				to = movedTo
			}
			var err error
			if moves[i], err = recording(t, pool, "finance", "alice", Images{Pins: map[string]string{tagged: to}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	// Each move names the digest the one before it left, so the moves chain from nothing to the
	// digest the pin holds now.
	was := map[string]int{}
	for _, m := range moves {
		for _, one := range m {
			was[one.Was]++
		}
	}
	if was[""] != 1 {
		t.Errorf("the pin was created %d times", was[""])
	}
}

// Deleting a workflow takes its images with it.
func TestAWorkflowDeletedTakesItsImages(t *testing.T) {
	pool, super := repositories(t)
	if _, err := recording(t, pool, "finance", "alice", Images{
		Pins: map[string]string{tagged: pinnedTo}, Manifests: map[string][]byte{pinnedTo: manifest},
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `delete from workflows where namespace = 'finance' and name = 'nightly'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := conn.QueryRow(t.Context(),
		`select (select count(*) from image_pins) + (select count(*) from brick_manifests)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d images outlived their workflow", left)
	}
}
