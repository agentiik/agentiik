package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A principal's collections, against a real PostgreSQL: what is kept of one, its bounds, the order
// of its members, that nobody else reads it, and what removing its owner, or renaming a namespace
// its members name, does to it.

func collections(t *testing.T, pool *Pool, fn func(context.Context, *Wide) error) error {
	t.Helper()
	return pool.Installation(t.Context(), Collections, fn)
}

// owners writes the principals the tests below own collections as.
func owners(t *testing.T, super string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for _, stmt := range []string{
		`insert into principals (id, kind) values ('alice', 'user'), ('bob', 'user')`,
		`insert into users (login, display_name) values ('alice', 'Alice'), ('bob', 'Bob')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
}

// A collection is made empty, its members written whole in the order they were added, changed in
// place, and taken out; its name and description change and its identifier does not.
func TestACollectionRoundTrips(t *testing.T) {
	pool, super := opened(t)
	owners(t, super)
	const id = "01JR8Q2W6H3V0X9K4M7N5P1T2C"

	var made Collection
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		made, err = w.CreateCollection(ctx, "alice", KindUser, id, "back-office", "Invoices and orders.")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if made.ID != id || made.Name != "back-office" || made.Description != "Invoices and orders." || len(made.Members) != 0 {
		t.Fatalf("the collection was made as %+v", made)
	}

	var now Collection
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.WriteCollectionMember(ctx, "alice", id, CollectionMember{Namespace: "finance", Workflow: "monthly-invoicing"}); err != nil {
			return err
		}
		if _, err := w.WriteCollectionMember(ctx, "alice", id, CollectionMember{Namespace: "team-ops", Workflow: "nightly", Ref: "v2", As: "nightly_report"}); err != nil {
			return err
		}
		// Written again, the first keeps its place and takes what is written now.
		var err error
		now, err = w.WriteCollectionMember(ctx, "alice", id, CollectionMember{Namespace: "finance", Workflow: "monthly-invoicing", Ref: "refs/heads/main"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []CollectionMember{
		{Namespace: "finance", Workflow: "monthly-invoicing", Ref: "refs/heads/main"},
		{Namespace: "team-ops", Workflow: "nightly", Ref: "v2", As: "nightly_report"},
	}
	if fmt.Sprint(now.Members) != fmt.Sprint(want) {
		t.Fatalf("the members are %+v, want %+v", now.Members, want)
	}

	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		renamed := "finance-desk"
		c, err := w.UpdateCollection(ctx, "alice", id, &renamed, nil)
		if err != nil {
			return err
		}
		if c.ID != id || c.Name != renamed || c.Description != "Invoices and orders." {
			return fmt.Errorf("the collection was changed to %+v", c)
		}
		c, err = w.RemoveCollectionMember(ctx, "alice", id, "finance", "monthly-invoicing")
		if err != nil {
			return err
		}
		if len(c.Members) != 1 || c.Members[0].Workflow != "nightly" {
			return fmt.Errorf("a member taken out left %+v", c.Members)
		}
		// Taking out what is not a member changes nothing and is no error.
		if _, err := w.RemoveCollectionMember(ctx, "alice", id, "finance", "monthly-invoicing"); err != nil {
			return err
		}
		return w.DeleteCollection(ctx, "alice", id)
	}); err != nil {
		t.Fatal(err)
	}
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.Collection(ctx, "alice", id)
		return err
	}); !errors.Is(err, ErrNoCollection) {
		t.Fatalf("a removed collection was read with %v", err)
	}
}

// Nobody reads, changes or removes another principal's collection: each is answered as one that
// does not exist. And two of one principal's collections cannot share a name, while two
// principals' can.
func TestACollectionIsItsOwnersAlone(t *testing.T) {
	pool, super := opened(t)
	owners(t, super)
	const id = "01JR8Q2W6H3V0X9K4M7N5P1T2C"
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.CreateCollection(ctx, "alice", KindUser, id, "back-office", ""); err != nil {
			return err
		}
		_, err := w.CreateCollection(ctx, "bob", KindUser, "01JR8T5Y7A9C1E3G5J7M9P1R3T", "back-office", "")
		return err
	}); err != nil {
		t.Fatalf("two principals could not each name a collection back-office: %v", err)
	}
	err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.CreateCollection(ctx, "alice", KindUser, "01JR8V0000000000000000000A", "back-office", "")
		return err
	})
	if !errors.Is(err, ErrCollectionNameHeld) {
		t.Fatalf("a second back-office of alice's was refused with %v", err)
	}
	for name, act := range map[string]func(context.Context, *Wide) error{
		"read": func(ctx context.Context, w *Wide) error { _, err := w.Collection(ctx, "bob", id); return err },
		"changed": func(ctx context.Context, w *Wide) error {
			_, err := w.UpdateCollection(ctx, "bob", id, nil, nil)
			return err
		},
		"removed": func(ctx context.Context, w *Wide) error { return w.DeleteCollection(ctx, "bob", id) },
		"added to": func(ctx context.Context, w *Wide) error {
			_, err := w.WriteCollectionMember(ctx, "bob", id, CollectionMember{Namespace: "finance", Workflow: "monthly-invoicing"})
			return err
		},
	} {
		if err := collections(t, pool, act); !errors.Is(err, ErrNoCollection) {
			t.Errorf("alice's collection %s by bob answered %v", name, err)
		}
	}
	// A namespace's handle reads no collection at all, whoever's.
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var n int
		if err := ns.tx.QueryRow(ctx, `select count(*) from collections`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("a namespace's handle read %d collections", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A principal holds 50 collections at most, and a collection 100 members.
func TestACollectionIsBounded(t *testing.T) {
	pool, super := opened(t)
	owners(t, super)
	ulid := func(i int) string { return fmt.Sprintf("01JR8Q2W6H3V0X9K4M7N5P%04d", i) }
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		for i := range CollectionsPerPrincipal {
			if _, err := w.CreateCollection(ctx, "alice", KindUser, ulid(i), fmt.Sprintf("c%d", i), ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.CreateCollection(ctx, "alice", KindUser, ulid(CollectionsPerPrincipal), "one-more", "")
		return err
	})
	if !errors.Is(err, ErrTooManyCollections) {
		t.Fatalf("a 51st collection was refused with %v", err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`insert into collection_members (collection, namespace, workflow, position)
		 select $1, 'finance', 'w' || i, i from generate_series(1, 100) as i`, ulid(0)); err != nil {
		t.Fatal(err)
	}
	err = collections(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.WriteCollectionMember(ctx, "alice", ulid(0), CollectionMember{Namespace: "finance", Workflow: "monthly-invoicing"})
		return err
	})
	if !errors.Is(err, ErrTooManyMembers) {
		t.Fatalf("a 101st member was refused with %v", err)
	}
	// A member written again is no new one, at the bound as anywhere.
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		_, err := w.WriteCollectionMember(ctx, "alice", ulid(0), CollectionMember{Namespace: "finance", Workflow: "w7", As: "seventh"})
		return err
	}); err != nil {
		t.Fatalf("a member written again at the bound was refused: %v", err)
	}
}

// A collection goes with its owner, and its members follow a namespace's rename.
func TestACollectionGoesWithItsOwnerAndFollowsARename(t *testing.T) {
	pool, super := opened(t)
	owners(t, super)
	const id = "01JR8Q2W6H3V0X9K4M7N5P1T2C"
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		if _, err := w.CreateCollection(ctx, "alice", KindUser, id, "back-office", ""); err != nil {
			return err
		}
		_, err := w.WriteCollectionMember(ctx, "alice", id, CollectionMember{Namespace: "team-ops", Workflow: "nightly"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `update namespaces set name = 'operations' where name = 'team-ops'`); err != nil {
		t.Fatal(err)
	}
	var c Collection
	if err := collections(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		c, err = w.Collection(ctx, "alice", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(c.Members) != 1 || c.Members[0].Namespace != "operations" {
		t.Fatalf("after the rename the members are %+v", c.Members)
	}
	if _, err := conn.Exec(t.Context(), `delete from principals where id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := conn.QueryRow(t.Context(), `select (select count(*) from collections) + (select count(*) from collection_members)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d rows of alice's collection outlived her", left)
	}
}
