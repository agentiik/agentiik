package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The built-in store's values, sealed.
//
// "Envelope encryption with AES-256-GCM data keys, wrapped by a master key held outside the
// database." What is kept here is a value as that sealed it, and never the key that opens it.
// Package secret seals a value before it is handed to this file and opens it after it leaves, and
// this package, which the controller and the command line link as well, holds bytes it has no way
// to read.
//
// The version is this file's to allocate, because the database is what can keep it unique. It is
// bound into what is sealed, so that a ciphertext opens only at the write it was sealed for, and
// two writes sharing one would be two ciphertexts either of which opens where the other belongs.

// ErrNoValue is a secret the built-in store holds no value for: never written, or forgotten.
var ErrNoValue = errors.New("db: the built-in store holds no value of that name")

// SealedValue is one value of the built-in store as the table keeps it: every part of what sealing
// it produced, and nothing that opens it.
type SealedValue struct {
	// Version is the write this is, which the ciphertext is bound to.
	Version int

	// Master names the key that wrapped the data key.
	Master string

	// Salt is what the wrapping key of this write was derived over, WrappedKey the data key
	// wrapped, and WrapNonce what wrapped it.
	Salt       []byte
	WrappedKey []byte
	WrapNonce  []byte

	// Ciphertext is the value sealed under the data key, and Nonce what sealed it.
	Ciphertext []byte
	Nonce      []byte
}

// WriteSealed replaces the value the namespace keeps under a name with the one seal answers, at
// the next version of that name.
//
// The version is allocated here and handed to seal, rather than chosen by the caller, since it is
// bound into what seal produces and has to be the one the row is written at. The row is locked
// from the moment the version is read until the transaction ends, so two writes of one name take
// their turns and each is sealed at a version of its own.
func (n *NS) WriteSealed(ctx context.Context, name string, seal func(version int) (SealedValue, error)) error {
	// A row for the name if it has none, at version zero and holding nothing, so that the lock
	// below has something to take. Two first writes would otherwise both find nothing, both seal
	// version one, and one of them would fail on the key rather than wait its turn.
	_, err := n.tx.Exec(ctx,
		`insert into secret_values (namespace, name, version) values ($1, $2, 0)
		 on conflict (namespace, name) do nothing`, n.namespace, name)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == foreignKeyViolation {
		return ErrNoNamespace
	}
	if err != nil {
		return fmt.Errorf("db: the value of %s could not be written: %w", name, err)
	}

	var at int
	if err := n.tx.QueryRow(ctx,
		`select version from secret_values where namespace = $1 and name = $2 for update`,
		n.namespace, name).Scan(&at); err != nil {
		return fmt.Errorf("db: the value of %s could not be written: %w", name, err)
	}

	next := at + 1
	s, err := seal(next)
	if err != nil {
		return err
	}
	if s.Version != next {
		return fmt.Errorf("db: the value of %s was sealed at version %d and its next write is %d", name, s.Version, next)
	}
	if _, err := n.tx.Exec(ctx,
		`update secret_values
		   set version = $3, master = $4, salt = $5, wrapped_key = $6, wrap_nonce = $7,
		       ciphertext = $8, nonce = $9
		 where namespace = $1 and name = $2`,
		n.namespace, name, s.Version, s.Master, s.Salt, s.WrappedKey, s.WrapNonce, s.Ciphertext, s.Nonce); err != nil {
		return fmt.Errorf("db: the value of %s could not be written: %w", name, err)
	}
	return nil
}

// SealedValue is the value the namespace keeps under a name, as it was sealed, or ErrNoValue.
func (n *NS) SealedValue(ctx context.Context, name string) (SealedValue, error) {
	var s SealedValue
	err := n.tx.QueryRow(ctx,
		`select version, master, salt, wrapped_key, wrap_nonce, ciphertext, nonce
		 from secret_values where namespace = $1 and name = $2 and master is not null`,
		n.namespace, name).
		Scan(&s.Version, &s.Master, &s.Salt, &s.WrappedKey, &s.WrapNonce, &s.Ciphertext, &s.Nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return SealedValue{}, ErrNoValue
	}
	if err != nil {
		return SealedValue{}, fmt.Errorf("db: the value of %s could not be read: %w", name, err)
	}
	return s, nil
}

// ForgetSealed removes the value the namespace keeps under a name, and is not an error where it
// keeps none.
//
// The sealed value goes and the row stays, holding its version and nothing else, so that the next
// write of the name is sealed at a version no earlier write had.
func (n *NS) ForgetSealed(ctx context.Context, name string) error {
	if _, err := n.tx.Exec(ctx,
		`update secret_values
		   set master = null, salt = null, wrapped_key = null, wrap_nonce = null,
		       ciphertext = null, nonce = null
		 where namespace = $1 and name = $2 and master is not null`,
		n.namespace, name); err != nil {
		return fmt.Errorf("db: the value of %s could not be forgotten: %w", name, err)
	}
	return nil
}
