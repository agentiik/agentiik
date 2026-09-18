package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// The master key: the one piece of key material that is not in the database.
//
// "with its master key read from a file the API user alone can open, or from the host keyring."
// The file is what this reads; a keyring is an installation's own arrangement and reaches this
// package as bytes like anything else.

// KeyLength is how long a master key is. Thirty-two bytes, because it wraps AES-256 data keys
// with AES-256-GCM and a shorter key would make the wrapping the weakest part of the chain.
const KeyLength = 32

// Master is the key that wraps data keys, and an identifier for it.
//
// The identifier travels with every sealed value, which is what makes rotation possible at all: a
// store holding values sealed under two master keys can open both, and a value that names a key
// the installation no longer has says so instead of failing as corruption.
type Master struct {
	id  string
	key []byte
}

// ID names this key in everything sealed under it.
func (m *Master) ID() string { return m.id }

// LoadMaster reads a master key from a file.
//
// The mode is checked and a readable file is refused, because "a file the API user alone can
// open" is the whole of what protects it and a file anybody can read is a master key anybody has.
// Refusing is better than warning: a warning in a log nobody reads is how a key stays world
// readable for a year.
//
// The file holds the identifier and the key, one per line, as
//
//	id: 2026-09
//	key: <base64 of thirty-two bytes>
//
// rather than raw bytes, so that a person can look at it and tell which key it is without opening
// anything that would print the key itself.
func LoadMaster(path string) (*Master, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secret: the master key could not be read: %w", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("secret: %s is mode %#o and a master key is readable by its owner alone: chmod 600 it, because a key anybody on the host can read is a key anybody on the host has", path, mode)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secret: the master key could not be read: %w", err)
	}
	return ParseMaster(body)
}

// ParseMaster reads a master key from the bytes a file or a keyring holds.
func ParseMaster(body []byte) (*Master, error) {
	var id, encoded string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("secret: the master key is written as id: and key:, one per line")
		}
		switch strings.TrimSpace(key) {
		case "id":
			id = strings.TrimSpace(value)
		case "key":
			encoded = strings.TrimSpace(value)
		default:
			return nil, fmt.Errorf("secret: the master key holds %q, and it holds id and key", key)
		}
	}
	if id == "" {
		return nil, errors.New("secret: the master key has no id, and a value sealed under a key nobody can name cannot be rotated away from")
	}
	if strings.ContainsAny(id, " \t\n") {
		return nil, fmt.Errorf("secret: %q is not a key identifier", id)
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("secret: the master key is not base64: %w", err)
	}
	if len(raw) != KeyLength {
		return nil, fmt.Errorf("secret: the master key is %d bytes and AES-256 takes %d", len(raw), KeyLength)
	}
	return &Master{id: id, key: raw}, nil
}

// NewMaster mints a master key, for an installer that has none.
func NewMaster(id string) (*Master, error) {
	if id == "" {
		return nil, errors.New("secret: a master key with no identifier")
	}
	key, err := randomBytes(KeyLength)
	if err != nil {
		return nil, err
	}
	return &Master{id: id, key: key}, nil
}

// Write answers the bytes to put in the file, which is the only time the key leaves this process.
func (m *Master) Write() []byte {
	return []byte("# The master key of the built-in secret store. Mode 600, owned by the API user.\n" +
		"# Everything sealed under it names this id, so a rotation is a second key beside this one.\n" +
		"id: " + m.id + "\n" +
		"key: " + base64.StdEncoding.EncodeToString(m.key) + "\n")
}

// aead is the master key as a cipher.
func (m *Master) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, fmt.Errorf("secret: the master key could not be used: %w", err)
	}
	return cipher.NewGCM(block)
}

// ErrNotMine is a value sealed under a master key this one is not.
var ErrNotMine = errors.New("secret: that value was sealed under another master key")

// ErrNoMasterKey is what a caller gets for a store with no key at all, separated from a read
// failure because the two are fixed differently: one is a missing file and the other is a wrong
// one.
var ErrNoMasterKey = fs.ErrNotExist
