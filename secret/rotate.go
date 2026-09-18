package secret

import (
	"errors"
	"fmt"
)

// Retiring a master key.
//
// "Rotation is a write, never a read-then-write" is about a secret's value and about what the API
// hands a role: "No role reads a secret value through the API." Neither sentence is about what the
// process does with its own key material, and reading the first as though it were would leave an
// installation unable to retire a key at all. A value written two years ago and never touched
// again would keep the key that sealed it in service for ever, which makes rotation a thing an
// installation can start and never finish.
//
// So there is a path from one key to another, and it is deliberately narrow: it takes both keys,
// it opens under the old one and seals under the new one, and the value never leaves this
// package. Nothing here answers a caller with a value, and Reseal cannot be used to learn one:
// what it returns is a Sealed, which is what was in the database before and what goes back into
// it after.

// Keyring is the keys an installation holds: the one it seals under, and the older ones it can
// still open with.
//
// A ring rather than a key, because a rotation is not instant. Values sealed under the previous
// key keep opening while they wait their turn, and an installation that has finished resealing
// drops the old key from the ring, which is the moment the key can be destroyed.
type Keyring struct {
	current *Master
	past    map[string]*Master
}

// NewKeyring holds one key to seal under and any number to open with.
func NewKeyring(current *Master, past ...*Master) (*Keyring, error) {
	if current == nil {
		return nil, errors.New("secret: a keyring with nothing to seal under")
	}
	k := &Keyring{current: current, past: map[string]*Master{}}
	for _, m := range past {
		if m == nil {
			continue
		}
		if m.id == current.id {
			return nil, fmt.Errorf("secret: %q is on the keyring twice, and two keys of one name are two keys nobody can tell apart", m.id)
		}
		if _, twice := k.past[m.id]; twice {
			return nil, fmt.Errorf("secret: %q is on the keyring twice", m.id)
		}
		k.past[m.id] = m
	}
	return k, nil
}

// Current is the key everything new is sealed under.
func (k *Keyring) Current() *Master { return k.current }

// Seal writes a value under the current key.
func (k *Keyring) Seal(namespace, name string, version int, value []byte) (Sealed, error) {
	return k.current.Seal(namespace, name, version, value)
}

// Open reads a value under whichever key sealed it.
//
// A value naming a key the ring does not hold is refused as ErrNotMine rather than as corruption,
// because the two are fixed differently: one is a key somebody has to put back on the ring, and
// the other is a row somebody has to restore.
func (k *Keyring) Open(namespace, name string, version int, s Sealed) ([]byte, error) {
	m, err := k.keyFor(s)
	if err != nil {
		return nil, err
	}
	return m.Open(namespace, name, version, s)
}

// Reseal moves one value from an older key to the current one, without the value leaving this
// package.
//
// It answers whether anything was done, so that a sweep over a whole store can say how far it has
// got. A value already under the current key is answered false and untouched, which is what makes
// running the sweep twice free and what lets it be run until it reports nothing left to do.
//
// The version does not change. This is the same value at the same write, re-sealed: bumping it
// would make a maintenance operation look like a rotation in every audit that counts versions.
func (k *Keyring) Reseal(namespace, name string, version int, s Sealed) (Sealed, bool, error) {
	if s.Master == k.current.id {
		return s, false, nil
	}
	m, err := k.keyFor(s)
	if err != nil {
		return Sealed{}, false, err
	}

	value, err := m.Open(namespace, name, version, s)
	if err != nil {
		return Sealed{}, false, err
	}
	defer wipe(value)

	next, err := k.current.Seal(namespace, name, version, value)
	if err != nil {
		return Sealed{}, false, err
	}
	return next, true, nil
}

// Retirable says whether every value on the ring is under the current key, which is the question
// an operator is really asking when they ask whether a key can be destroyed.
//
// It takes what a store found rather than reading one, because this package holds no rows. A
// store answers it by asking for the distinct master names it holds.
func (k *Keyring) Retirable(held []string) (bool, []string) {
	var left []string
	for _, id := range held {
		if id != k.current.id {
			left = append(left, id)
		}
	}
	return len(left) == 0, left
}

func (k *Keyring) keyFor(s Sealed) (*Master, error) {
	if s.Master == k.current.id {
		return k.current, nil
	}
	if m, held := k.past[s.Master]; held {
		return m, nil
	}
	return nil, fmt.Errorf("%w: it names %q and this keyring holds %s", ErrNotMine, s.Master, k.names())
}

func (k *Keyring) names() string {
	names := k.current.id
	for id := range k.past {
		names += ", " + id
	}
	return names
}
