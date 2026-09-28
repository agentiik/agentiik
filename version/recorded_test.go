package version_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// What a namespace has recorded of its images, as a hook reaches it: a tag at the digest the
// namespace pinned it to, a brick step held to the manifest the namespace recorded for its image,
// and what the namespace holds nothing for refused naming agk push. Against a real PostgreSQL.

// recordedFor is a namespace's store resolvers, over a database holding finance and team-ops, with
// finance's pins and manifests recorded as given.
func recordedFor(t *testing.T, pins map[string]string, manifests map[string][]byte) (*db.Pool, version.Checking) {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.RecordImages(ctx, pins, manifests, "alice", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	c := everything()
	c.Resolvers = version.Recorded(pool, "finance")
	c.Secrets = everything().Secrets
	c.SecretUse = everything().SecretUse
	return pool, c
}

// A tag is taken at the digest the namespace pinned it to, and the step held to the manifest the
// namespace recorded for that image, a tag written beside the digest or not.
func TestAPushIsJudgedAgainstWhatItsNamespaceRecorded(t *testing.T) {
	_, c := recordedFor(t,
		map[string]string{"ghcr.io/acme/agk-invoice:1.4.0": pinnedInvoice},
		map[string][]byte{"ghcr.io/acme/agk-invoice:1.4.0@" + pinnedInvoice[strings.Index(pinnedInvoice, "@")+1:]: []byte(manifest)})
	checked, err := version.Check(t.Context(), aRepository(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := checked.Version.Images["ghcr.io/acme/agk-invoice:1.4.0"]; got != pinnedInvoice {
		t.Errorf("the tag was taken at %s, and the namespace pinned it to %s", got, pinnedInvoice)
	}
	if st, _ := checked.Graph.Step("invoice"); st.Image != pinnedInvoice {
		t.Errorf("the step runs %s", st.Image)
	}
	recorded, err := brick.ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if string(checked.Version.Manifests["ghcr.io/acme/agk-invoice:1.4.0"]) != string(recorded.Document()) {
		t.Error("the version holds a manifest other than the one the namespace recorded")
	}
}

// A tag the namespace pinned to nothing, and an image it recorded no manifest of, are refused
// where the step's image is written, naming agk push; and what another namespace recorded is not
// this one's.
func TestWhatTheNamespaceHoldsNothingForIsRefusedNamingAgkPush(t *testing.T) {
	pool, c := recordedFor(t, map[string]string{"ghcr.io/acme/agk-invoice:1.4.0": pinnedInvoice}, nil)
	_, err := version.Check(t.Context(), aRepository(), c)
	r := refusedBy(t, err, graph.RuleManifestMissing)
	if !strings.Contains(r.Detail, "agk push") || r.At.File != "fragments/bricks.yaml" {
		t.Errorf("an image with no manifest recorded is refused at %s: %s", r.At, r.Detail)
	}

	if err := pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *db.NS) error {
		return ns.RecordImages(ctx, map[string]string{"ghcr.io/acme/agk-invoice:1.4.0": pinnedInvoice},
			map[string][]byte{pinnedInvoice: []byte(manifest)}, "bob", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := version.Check(t.Context(), aRepository(), c); !errors.Is(err, graph.ErrRefused) {
		t.Errorf("finance's push was judged against team-ops' manifest: %v", err)
	}

	tree := aRepository()
	tree["fragments/bricks.yaml"].Data = []byte(".invoicing:\n  image: ghcr.io/acme/agk-invoice:1.5.0\n")
	_, err = version.Check(t.Context(), tree, c)
	r = refusedBy(t, err, version.RuleImageNotPinned)
	if !strings.Contains(r.Detail, "agk push") || r.At.File != "fragments/bricks.yaml" {
		t.Errorf("a tag with no pin recorded is refused at %s: %s", r.At, r.Detail)
	}
}

// A store that cannot answer is the installation's failure and never an image held or not held.
func TestAStoreThatCannotAnswerIsNotAnAnswer(t *testing.T) {
	pool, c := recordedFor(t, nil, nil)
	pool.Close()
	_, err := version.Check(t.Context(), aRepository(), c)
	if err == nil || errors.Is(err, graph.ErrRefused) {
		t.Errorf("a store that could not be read was answered %v", err)
	}
}
