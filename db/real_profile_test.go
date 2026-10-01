package db

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// What a user says of themself and their photo, as the application's role writes and reads them.

// A profile is written whole and read with the user; an administrator's change leaves it; the
// table refuses a field past its bound; a photo is read on its own, removed once, and goes with
// its user.
func TestAUsersProfileAndPhotoAreKeptBesideThem(t *testing.T) {
	pool := identity(t)
	ctx := t.Context()
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.CreateUser(ctx, User{Login: "alice", DisplayName: "Alice"})
	})

	// A user created says nothing of themself and holds no photo.
	var alice User
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		alice, err = w.User(ctx, "alice")
		return err
	})
	if alice.Profile != (Profile{}) || !alice.AvatarUpdatedAt.IsZero() {
		t.Errorf("a user just created reads as %+v", alice)
	}

	said := Profile{GivenName: "Alice", FamilyName: "Martin", Title: "Technical lead", Location: "Lyon", Timezone: "Europe/Paris", Bio: "Writes the invoicing workflows."}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.UpdateProfile(ctx, "alice", "Alice Martin", said); err != nil {
			return err
		}
		// An administrator suspending her, as UpdateUser writes it, leaves what she said.
		if err := w.UpdateUser(ctx, User{Login: "alice", DisplayName: "Alice Martin", Suspended: true}); err != nil {
			return err
		}
		var err error
		alice, err = w.User(ctx, "alice")
		return err
	})
	if alice.DisplayName != "Alice Martin" || alice.Profile != said || !alice.Suspended {
		t.Errorf("alice reads as %+v", alice)
	}
	err := pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.UpdateProfile(ctx, "nobody", "Nobody", Profile{})
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("the profile of nobody was written as %v", err)
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.UpdateProfile(ctx, "alice", "Alice Martin", Profile{Bio: strings.Repeat("é", 281)})
	})
	if err == nil {
		t.Error("a bio of 281 characters was kept")
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.UpdateProfile(ctx, "alice", "Alice Martin", Profile{Bio: strings.Repeat("é", 280)})
	})

	// A photo, set, read alone, set again and removed once.
	if _, _, err := avatarOf(t, pool, "alice"); !errors.Is(err, ErrNoAvatar) {
		t.Errorf("alice's photo before she set one is read as %v", err)
	}
	if _, _, err := avatarOf(t, pool, "nobody"); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("nobody's photo is read as %v", err)
	}
	first := time.Now().UTC().Truncate(time.Microsecond)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		return w.SetAvatar(ctx, "alice", []byte("\x89PNG first"), first)
	})
	second := first.Add(time.Minute)
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		if err := w.SetAvatar(ctx, "alice", []byte("\x89PNG second"), second); err != nil {
			return err
		}
		var err error
		alice, err = w.User(ctx, "alice")
		return err
	})
	if picture, at, err := avatarOf(t, pool, "alice"); err != nil || !bytes.Equal(picture, []byte("\x89PNG second")) || !at.Equal(second) {
		t.Errorf("alice's photo is read as %q at %s, %v", picture, at, err)
	}
	if !alice.AvatarUpdatedAt.Equal(second) {
		t.Errorf("alice's photo was set at %s, and she reads as setting it at %s", second, alice.AvatarUpdatedAt)
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		return w.SetAvatar(ctx, "nobody", []byte("\x89PNG"), first)
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("nobody's photo was set as %v", err)
	}
	for i, want := range []bool{true, false} {
		var held bool
		wide(t, pool, func(ctx context.Context, w *Wide) error {
			var err error
			held, err = w.RemoveAvatar(ctx, "alice")
			return err
		})
		if held != want {
			t.Errorf("removing alice's photo the %d time answered %v", i+1, held)
		}
	}
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.RemoveAvatar(ctx, "nobody")
		return err
	})
	if !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("removing nobody's photo answered %v", err)
	}
	wide(t, pool, func(ctx context.Context, w *Wide) error {
		var err error
		alice, err = w.User(ctx, "alice")
		return err
	})
	if !alice.AvatarUpdatedAt.IsZero() {
		t.Errorf("alice's photo is removed, and she reads as setting it at %s", alice.AvatarUpdatedAt)
	}

	// The table holds a photo and when it was set together, whoever writes them.
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `update users set avatar = '\x89'::bytea where login = 'alice'`)
		return err
	})
	if err == nil {
		t.Error("a photo was kept with no instant it was set at")
	}
}

// avatarOf reads login's photo.
func avatarOf(t *testing.T, pool *Pool, login string) (picture []byte, at time.Time, err error) {
	t.Helper()
	err = pool.Installation(t.Context(), Identity, func(ctx context.Context, w *Wide) error {
		var err error
		picture, at, err = w.Avatar(ctx, login)
		return err
	})
	return picture, at, err
}
