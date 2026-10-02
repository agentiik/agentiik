package secret

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The built-in store: a value sealed under the installation's keyring and kept in the database.
//
// Seal and Open are the encryption; this is where what they produce is kept and read back from.
// The database holds every part of a sealed value and never the key, and the version it allocates
// is the one the value is bound to, so a ciphertext copied into another namespace's row, another
// secret's, or over a later write of its own, does not open.
//
// A value goes in on a declaration's PUT, in the transaction the declaration is written in, and
// comes out on a redemption, and those are the only two ways it moves. "No role reads a secret
// value through the API. Rotation is a write, never a read-then-write."

// Builtin is the built-in store: a Provider that opens a value, and the api.Values a declaration's
// PUT seals one into.
type Builtin struct {
	pool *db.Pool
	keys *Keyring
}

// NewBuiltin keeps values in the database, sealed under the current key of the ring and opened
// under whichever key of it sealed them.
func NewBuiltin(pool *db.Pool, keys *Keyring) (*Builtin, error) {
	switch {
	case pool == nil:
		return nil, errors.New("secret: no database, and the built-in store keeps its values there")
	case keys == nil:
		return nil, errors.New("secret: no keyring, and a value the built-in store keeps is sealed under a master key held outside the database")
	}
	return &Builtin{pool: pool, keys: keys}, nil
}

// Read opens the value the namespace keeps under the declaration's name.
func (b *Builtin) Read(ctx context.Context, namespace string, d db.Declaration) ([]byte, error) {
	if d.Provider != api.ProviderBuiltin || d.Path != "" {
		return nil, fmt.Errorf("secret: %s/%s is declared in %s, and the built-in store reads only a secret declared in it, which names no path", namespace, d.Name, d.Provider)
	}
	var s db.SealedValue
	var storage string
	err := b.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if s, err = ns.SealedValue(ctx, d.Name); err != nil {
			return err
		}
		storage, err = ns.Storage(ctx)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoValue):
		return nil, fmt.Errorf("secret: %s/%s is declared in the built-in store and no value has been written to it: %w", namespace, d.Name, api.ErrNoSecret)
	case err != nil:
		return nil, fmt.Errorf("secret: the value of %s/%s could not be read: %w", namespace, d.Name, err)
	}

	// Opened under the namespace's storage name, which it was sealed under and a rename leaves as
	// it was, so that a value sealed before a rename opens after it.
	value, err := b.keys.Open(storage, d.Name, s.Version, sealedOf(s))
	if errors.Is(err, ErrNotMine) {
		// Said apart from a value that does not open, because it is fixed apart: a key to put
		// back on the ring, rather than a row to restore or a value to write again.
		return nil, fmt.Errorf("secret: %s/%s was sealed under a master key this installation's keyring does not hold: %w", namespace, d.Name, err)
	}
	return value, err
}

// Write seals value as the one the namespace keeps under name, at the next version of that name,
// in the transaction the declaration is written in. It is sealed under the namespace's storage name,
// the name it was created with, rather than its name, which a rename changes: a value is bound to
// the namespace it was written in for that namespace's whole life.
func (b *Builtin) Write(ctx context.Context, ns *db.NS, name string, value []byte) error {
	storage, err := ns.Storage(ctx)
	if err != nil {
		return err
	}
	return ns.WriteSealed(ctx, name, func(version int) (db.SealedValue, error) {
		s, err := b.keys.Seal(storage, name, version, value)
		if err != nil {
			return db.SealedValue{}, err
		}
		return rowOf(s), nil
	})
}

// Forget removes the value the namespace keeps under name, and is not an error where it keeps
// none. The count of its writes stays, so a value written under the name later is sealed at a
// version no earlier one had.
func (b *Builtin) Forget(ctx context.Context, ns *db.NS, name string) error {
	return ns.ForgetSealed(ctx, name)
}

// rowOf and sealedOf are one sealed value in the two shapes it has: this package's, and the
// table's, which package db holds without importing this one.
func rowOf(s Sealed) db.SealedValue {
	return db.SealedValue{
		Version: s.Version, Master: s.Master,
		Salt: s.Salt, WrappedKey: s.Key, WrapNonce: s.WrapNonce,
		Ciphertext: s.Value, Nonce: s.Nonce,
	}
}

func sealedOf(s db.SealedValue) Sealed {
	return Sealed{
		Master: s.Master, Version: s.Version,
		Salt: s.Salt, Key: s.WrappedKey, WrapNonce: s.WrapNonce,
		Value: s.Ciphertext, Nonce: s.Nonce,
	}
}
