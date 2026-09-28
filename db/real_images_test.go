package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What a namespace knows of the images its workflows name, against a real PostgreSQL: the digest
// each tag was pinned to and the manifest of each image, who recorded each and when, kept to its
// namespace, and filled at the upgrade from every version recorded before.

const (
	invoiceTag   = "ghcr.io/acme/agk-invoice:1.4.0"
	invoiceFirst = "ghcr.io/acme/agk-invoice@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	invoiceMoved = "ghcr.io/acme/agk-invoice@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	reportImage  = "ghcr.io/acme/agk-report@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	alpineTag    = "alpine:3.21"
	alpinePinned = "alpine@sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

func recordImages(t *testing.T, pool *Pool, namespace string, pins map[string]string, manifests map[string][]byte, by string, at time.Time) error {
	t.Helper()
	return pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		return ns.RecordImages(ctx, pins, manifests, by, at)
	})
}

func pinOf(t *testing.T, pool *Pool, namespace, reference string) (ImagePin, error) {
	t.Helper()
	var p ImagePin
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		p, err = ns.Pinned(ctx, reference)
		return err
	})
	return p, err
}

func manifestOf(t *testing.T, pool *Pool, namespace, image string) (ImageManifest, error) {
	t.Helper()
	var m ImageManifest
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		m, err = ns.Manifest(ctx, image)
		return err
	})
	return m, err
}

// A tag is pinned by whoever first pins it to a digest, and pinning it again to that digest changes
// nothing: who pinned it and when say when it last moved and who moved it. Moved to another digest,
// it names the new one, and whoever moved it.
func TestATagIsPinnedByWhoeverLastMovedIt(t *testing.T) {
	pool, _ := opened(t)
	first := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		pinned, by string
		at         time.Time
	}{
		{invoiceFirst, "alice", first},
		{invoiceFirst, "bob", first.Add(time.Hour)},
	} {
		if err := recordImages(t, pool, "finance", map[string]string{invoiceTag: r.pinned}, nil, r.by, r.at); err != nil {
			t.Fatal(err)
		}
	}
	p, err := pinOf(t, pool, "finance", invoiceTag)
	if err != nil {
		t.Fatal(err)
	}
	if p.Pinned != invoiceFirst || p.PinnedBy != "alice" || !p.PinnedAt.Equal(first) {
		t.Errorf("the tag pinned again to the digest it names reads %+v, and alice pinned it there at %s", p, first)
	}

	moved := first.Add(2 * time.Hour)
	if err := recordImages(t, pool, "finance", map[string]string{invoiceTag: invoiceMoved}, nil, "bob", moved); err != nil {
		t.Fatal(err)
	}
	if p, err = pinOf(t, pool, "finance", invoiceTag); err != nil {
		t.Fatal(err)
	}
	if p.Pinned != invoiceMoved || p.PinnedBy != "bob" || !p.PinnedAt.Equal(moved) {
		t.Errorf("the tag moved reads %+v, and bob moved it to %s at %s", p, invoiceMoved, moved)
	}

	if _, err := pinOf(t, pool, "finance", alpineTag); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("a tag nobody pinned is answered %v", err)
	}
}

// A manifest is kept by the image it was read out of, at its repository and digest, so that a tag
// written beside the digest names the same image; recorded again as it is, it is left as it was,
// and with other bytes it takes them and who sent them. Two names of one image sent with two
// manifests are refused, and nothing of the record is kept.
func TestAManifestIsKeptByTheImageItWasReadOutOf(t *testing.T) {
	pool, _ := opened(t)
	at := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	tagged := "ghcr.io/acme/agk-report:2.0@" + reportImage[strings.Index(reportImage, "@")+1:]
	if err := recordImages(t, pool, "finance", nil, map[string][]byte{tagged: []byte(`{"kind":"Brick"}`)}, "alice", at); err != nil {
		t.Fatal(err)
	}
	for _, asked := range []string{reportImage, tagged, "ghcr.io/acme/agk-report:3.0@" + reportImage[strings.Index(reportImage, "@")+1:]} {
		m, err := manifestOf(t, pool, "finance", asked)
		if err != nil {
			t.Fatalf("the manifest of %s: %v", asked, err)
		}
		if m.Image != reportImage || string(m.Document) != `{"kind":"Brick"}` || m.RecordedBy != "alice" || !m.RecordedAt.Equal(at) {
			t.Errorf("the manifest of %s reads %+v", asked, m)
		}
	}

	if err := recordImages(t, pool, "finance", nil, map[string][]byte{reportImage: []byte(`{"kind":"Brick"}`)}, "bob", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if m, _ := manifestOf(t, pool, "finance", reportImage); m.RecordedBy != "alice" {
		t.Errorf("the same manifest recorded again reads as recorded by %s", m.RecordedBy)
	}
	if err := recordImages(t, pool, "finance", nil, map[string][]byte{reportImage: []byte(`{"kind": "Brick"}`)}, "bob", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if m, _ := manifestOf(t, pool, "finance", reportImage); m.RecordedBy != "bob" || string(m.Document) != `{"kind": "Brick"}` {
		t.Errorf("a manifest recorded with other bytes reads %+v", m)
	}

	err := recordImages(t, pool, "finance", map[string]string{alpineTag: alpinePinned}, map[string][]byte{
		invoiceFirst: []byte(`{"a":1}`),
		"ghcr.io/acme/agk-invoice:1.4.0@" + invoiceFirst[strings.Index(invoiceFirst, "@")+1:]: []byte(`{"a":2}`),
	}, "carol", at)
	if err == nil || !strings.Contains(err.Error(), "two manifests") {
		t.Fatalf("two manifests of one image were recorded, answering %v", err)
	}
	if _, err := pinOf(t, pool, "finance", alpineTag); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("a record refused kept its pin: %v", err)
	}
	if _, err := manifestOf(t, pool, "finance", invoiceFirst); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("a record refused kept a manifest: %v", err)
	}
}

// What one namespace pinned a tag to is no reason for another's workflows to run it: each reads its
// own pins and manifests, and never the other's.
func TestOneNamespacesImagesAreNotAnothers(t *testing.T) {
	pool, _ := opened(t)
	if err := recordImages(t, pool, "finance", map[string]string{invoiceTag: invoiceFirst}, map[string][]byte{invoiceFirst: []byte(`{}`)}, "alice", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pinOf(t, pool, "team-ops", invoiceTag); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("team-ops reads finance's pin: %v", err)
	}
	if _, err := manifestOf(t, pool, "team-ops", invoiceFirst); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("team-ops reads finance's manifest: %v", err)
	}
	if err := recordImages(t, pool, "team-ops", map[string]string{invoiceTag: invoiceMoved}, nil, "bob", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for namespace, want := range map[string]string{"finance": invoiceFirst, "team-ops": invoiceMoved} {
		if p, err := pinOf(t, pool, namespace, invoiceTag); err != nil || p.Pinned != want {
			t.Errorf("%s reads its pin as %+v, %v, want %s", namespace, p, err, want)
		}
	}
}

// The tables hold what they keep to what it is, whoever writes to them: a tag pinned to a digest of
// its own repository and to nothing else, a tag that is a tag, an image kept at its repository and
// digest alone, a reference no longer than a registry serves, and a manifest that is something.
func TestTheTablesKeepOnlyWhatTheyHold(t *testing.T) {
	_, super := opened(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	// A tag of 110 characters past a repository of 408, which a registry serves under neither
	// bound, pinned to a digest of 480 bytes, which fits.
	repository := "ghcr.io/" + strings.Repeat("a", 400)
	long := repository + ":" + strings.Repeat("t", 110)
	for _, c := range []struct {
		why, sql string
		args     []any
	}{
		{"a tag pinned to another repository",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, 'alice')`, []any{alpineTag, invoiceFirst}},
		{"a tag pinned to a tag",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, 'alice')`, []any{invoiceTag, invoiceTag}},
		{"a digest pinned as though it were a tag",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $1, 'alice')`, []any{invoiceFirst}},
		{"a reference past 512 bytes",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, 'alice')`,
			[]any{long, repository + invoiceFirst[strings.Index(invoiceFirst, "@"):]}},
		{"a tag holding a space",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', 'alpine :3.21', 'alpine @sha256:` + strings.Repeat("4", 64) + `', 'alice')`, nil},
		{"a pin by nobody",
			`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, '')`, []any{alpineTag, alpinePinned}},
		{"an image named with a tag beside its digest",
			`insert into brick_manifests (namespace, image, manifest, recorded_by) values ('finance', $1, '\x7b7d', 'alice')`,
			[]any{"ghcr.io/acme/agk-report:2.0" + reportImage[strings.Index(reportImage, "@"):]}},
		{"an image named by a tag",
			`insert into brick_manifests (namespace, image, manifest, recorded_by) values ('finance', $1, '\x7b7d', 'alice')`, []any{invoiceTag}},
		{"an empty manifest",
			`insert into brick_manifests (namespace, image, manifest, recorded_by) values ('finance', $1, '', 'alice')`, []any{reportImage}},
	} {
		_, err := conn.Exec(t.Context(), c.sql, c.args...)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Errorf("%s was answered %v, and the table refuses it", c.why, err)
		}
	}
	// And what they keep, a port in the registry's host included.
	for _, s := range []struct{ sql, ref, pinned string }{
		{`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, 'alice')`,
			"registry.example:5000/acme/brick:1.4", "registry.example:5000/acme/brick@sha256:" + strings.Repeat("5", 64)},
		{`insert into image_pins (namespace, reference, pinned, pinned_by) values ('finance', $1, $2, 'alice')`,
			"registry.example:5000/acme/brick", "registry.example:5000/acme/brick@sha256:" + strings.Repeat("5", 64)},
	} {
		if _, err := conn.Exec(t.Context(), s.sql, s.ref, s.pinned); err != nil {
			t.Errorf("%s pinned to %s was refused: %v", s.ref, s.pinned, err)
		}
	}
	if _, err := conn.Exec(t.Context(), `insert into brick_manifests (namespace, image, manifest, recorded_by)
		values ('finance', 'registry.example:5000/acme/brick@sha256:`+strings.Repeat("5", 64)+`', '\x7b7d', 'alice')`); err != nil {
		t.Errorf("an image of a registry with a port was refused: %v", err)
	}
}

// beforeImages is the last migration before the namespace kept its images.
const beforeImages = "0048_sessions_opened_by_a_credential.sql"

// An installation upgraded knows the images of every version it held, so that a workflow pushed as
// trees before is known at its first git push, with nothing asked of whoever runs it: each tag at
// the digest the newest version naming it pinned, each manifest from the newest version holding one
// and kept by the image it was read out of, each recorded by that version's author when it was
// pushed, in the version's own namespace. What a version holds that is no image, a manifest kept by
// a tag no digest was recorded for, and a bundle of another shape, is left out rather than refusing
// the upgrade, and no version is changed.
func TestAnUpgradeKnowsTheImagesOfEveryVersionItHeld(t *testing.T) {
	super, role := migratedAt(t, beforeImages)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	bundle := func(images map[string]string, manifests map[string][]byte) string {
		b, err := json.Marshal(stored{Entry: "agentiik.yaml", Document: []byte("kind: Workflow\n"), Images: images, Manifests: manifests})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	january := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	february := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	march := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	reportTagged := "ghcr.io/acme/agk-report:2.0" + reportImage[strings.Index(reportImage, "@"):]
	versions := []struct {
		namespace, workflow, commit, graph, author string
		at                                         time.Time
	}{
		{"finance", "monthly-invoicing", "a3f9c1e", bundle(
			map[string]string{invoiceTag: invoiceFirst, alpineTag: alpinePinned},
			map[string][]byte{invoiceTag: []byte(`{"v":1}`)}), "alice", january},
		{"finance", "monthly-invoicing", "b4a0d2f", bundle(
			map[string]string{invoiceTag: invoiceMoved},
			map[string][]byte{invoiceTag: []byte(`{"v":2}`)}), "bob", february},
		{"finance", "reporting", "c5b1e3a", bundle(nil, map[string][]byte{reportTagged: []byte(`{"v":3}`)}), "carol", march},
		{"team-ops", "nightly", "d6c2f4b", bundle(
			map[string]string{invoiceTag: invoiceFirst},
			map[string][]byte{invoiceTag: []byte(`{"v":1}`)}), "dave", march},
		// A version from before digests were kept, whose manifest is kept by a tag nothing pins.
		{"finance", "reporting", "e7d3a5c", bundle(nil, map[string][]byte{"ghcr.io/acme/agk-old:1": []byte(`{"v":0}`)}), "alice", january},
		// And bundles no push wrote: none at all, images of another shape, a tag pinned to another
		// repository, a digest that is not one, and a manifest that is not base64.
		{"finance", "reporting", "f8e4b6d", `{}`, "alice", january},
		{"finance", "reporting", "a9f5c7e", `{"images": ["alpine:3.21"], "manifests": "no"}`, "alice", january},
		{"finance", "reporting", "b0a6d8f", `{"images": {"alpine:3.19": "` + invoiceFirst + `", "busybox:1": "busybox@sha256:abc"}}`, "alice", march},
		{"finance", "reporting", "c1b7e9a", `{"manifests": {"` + reportImage + `": "not base64!"}}`, "alice", march},
	}
	if _, err := conn.Exec(ctx, `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `insert into workflows (namespace, name) values
		('finance', 'monthly-invoicing'), ('finance', 'reporting'), ('team-ops', 'nightly')`); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if _, err := conn.Exec(ctx, `insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
			values ($1, $2, $3, $4, $5, $6)`, v.namespace, v.workflow, v.commit, v.graph, v.author, v.at); err != nil {
			t.Fatalf("filling the database as the release before would have: %s", err)
		}
	}
	held := func() string {
		var all string
		if err := conn.QueryRow(ctx, `select string_agg(namespace || workflow || commit || graph::text || author || created_at::text, '|' order by commit)
			from workflow_versions`).Scan(&all); err != nil {
			t.Fatal(err)
		}
		return all
	}
	before := held()

	if _, err := Provision(ctx, conn, role, "test"); err != nil {
		t.Fatalf("the upgrade was refused: %s", err)
	}
	if held() != before {
		t.Error("the upgrade changed a version")
	}

	pool, err := Open(ctx, withCredentials(super, role, "test"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, want := range []ImagePin{
		{Reference: invoiceTag, Pinned: invoiceMoved, PinnedBy: "bob", PinnedAt: february},
		{Reference: alpineTag, Pinned: alpinePinned, PinnedBy: "alice", PinnedAt: january},
	} {
		got, err := pinOf(t, pool, "finance", want.Reference)
		if err != nil || got.Pinned != want.Pinned || got.PinnedBy != want.PinnedBy || !got.PinnedAt.Equal(want.PinnedAt) {
			t.Errorf("finance pins %s as %+v, %v, want %+v", want.Reference, got, err, want)
		}
	}
	if got, err := pinOf(t, pool, "team-ops", invoiceTag); err != nil || got.Pinned != invoiceFirst || got.PinnedBy != "dave" {
		t.Errorf("team-ops pins %s as %+v, %v, and its own version pinned it to %s", invoiceTag, got, err, invoiceFirst)
	}
	for _, want := range []ImageManifest{
		{Image: invoiceFirst, Document: []byte(`{"v":1}`), RecordedBy: "alice", RecordedAt: january},
		{Image: invoiceMoved, Document: []byte(`{"v":2}`), RecordedBy: "bob", RecordedAt: february},
		{Image: reportImage, Document: []byte(`{"v":3}`), RecordedBy: "carol", RecordedAt: march},
	} {
		got, err := manifestOf(t, pool, "finance", want.Image)
		if err != nil || got.Image != want.Image || string(got.Document) != string(want.Document) || got.RecordedBy != want.RecordedBy || !got.RecordedAt.Equal(want.RecordedAt) {
			t.Errorf("finance holds the manifest of %s as %+v, %v, want %+v", want.Image, got, err, want)
		}
	}
	if got, err := manifestOf(t, pool, "team-ops", invoiceFirst); err != nil || got.RecordedBy != "dave" {
		t.Errorf("team-ops holds the manifest of %s as %+v, %v", invoiceFirst, got, err)
	}

	var pins, manifests int
	if err := conn.QueryRow(ctx, `select (select count(*) from image_pins), (select count(*) from brick_manifests)`).Scan(&pins, &manifests); err != nil {
		t.Fatal(err)
	}
	if pins != 3 || manifests != 4 {
		t.Errorf("the upgrade recorded %d pins and %d manifests, want 3 and 4: what no version pinned, or pinned elsewhere, is no image", pins, manifests)
	}
}
