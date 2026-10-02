package db

import (
	"context"
	"errors"
	"testing"
)

// The store removes a personal namespace as it removes a shared one, with its built-in identity
// where it has one: which namespaces may be removed, and by whom, is the caller's to say, and
// removing a user takes their empty personal namespace with them. What it holds is counted all the
// same, a personal namespace's included.
func TestTheStoreRemovesAPersonalNamespaceAsAnyOther(t *testing.T) {
	pool := identity(t)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		for _, login := range []string{"alice", "bob"} {
			if err := w.CreateUser(ctx, User{Login: login}); err != nil {
				return err
			}
		}
		return nil
	})
	administer := func(fn func(context.Context, *Wide) error) error {
		return pool.Installation(t.Context(), NamespaceAdministration, fn)
	}
	for _, login := range []string{"alice", "bob"} {
		if err := administer(func(ctx context.Context, w *Wide) error {
			_, err := w.CreateNamespace(ctx, Namespace{Name: login, Kind: NamespacePersonal, Owner: login})
			return err
		}); err != nil {
			t.Fatalf("the personal namespace of %s could not be created: %s", login, err)
		}
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		return w.CreateServiceAccount(ctx, ServiceAccount{Namespace: "bob", Name: "deploy", CreatedBy: "bob"})
	}); err != nil {
		t.Fatal(err)
	}

	if err := administer(func(ctx context.Context, w *Wide) error { return w.RemoveNamespace(ctx, "alice") }); err != nil {
		t.Fatalf("an empty personal namespace was refused removal: %s", err)
	}
	var holds *NamespaceHolds
	err := administer(func(ctx context.Context, w *Wide) error { return w.RemoveNamespace(ctx, "bob") })
	if !errors.As(err, &holds) || holds.ServiceAccounts != 1 {
		t.Errorf("a personal namespace holding a service account was answered %v", err)
	}
	if err := administer(func(ctx context.Context, w *Wide) error {
		_, err := w.NamespaceNamed(ctx, "alice")
		return err
	}); !errors.Is(err, ErrNoNamespace) {
		t.Errorf("the removed personal namespace reads as %v", err)
	}
}
