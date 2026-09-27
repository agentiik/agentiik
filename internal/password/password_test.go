package password

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// A hash is written at the baseline the documentation names, in the PHC string format, with a salt
// of its own, and verifies the password it was made from and no other.
func TestAHashIsArgon2idAtTheOWASPBaseline(t *testing.T) {
	encoded, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(encoded, prefix) {
		t.Fatalf("the hash is %q, and it begins %q", encoded, prefix)
	}
	parts := strings.Split(strings.TrimPrefix(encoded, prefix), "$")
	salt, _ := base64.RawStdEncoding.DecodeString(parts[0])
	key, _ := base64.RawStdEncoding.DecodeString(parts[1])
	if len(parts) != 2 || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("the hash carries %d parts, a salt of %d bytes and a key of %d", len(parts), len(salt), len(key))
	}
	// The key is the algorithm's own, at the baseline's costs, and not something else that
	// happens to verify.
	if want := argon2.IDKey([]byte("correct horse battery staple"), salt, 2, 19*1024, 1, 32); string(key) != string(want) {
		t.Error("the key is not Argon2id's at 19 MiB, 2 iterations and 1 lane")
	}
	if MemoryBytes != 19<<20 {
		t.Errorf("one hash holds %d bytes, and the baseline is 19 MiB", MemoryBytes)
	}

	for password, want := range map[string]bool{
		"correct horse battery staple":  true,
		"correct horse battery staple ": false,
		"Correct horse battery staple":  false,
		"":                              false,
	} {
		ok, err := Verify(encoded, password)
		if err != nil || ok != want {
			t.Errorf("verifying %q answered %v, %v", password, ok, err)
		}
	}

	again, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if again == encoded {
		t.Error("the same password was hashed twice to the same string, with no salt of its own")
	}
}

// A hash at a lower cost than the baseline, which an older release could have written, still
// reads: the costs are read from the hash.
func TestAnOlderHashAtALowerCostStillReads(t *testing.T) {
	salt := []byte("sixteen byte slt")
	key := argon2.IDKey([]byte("hunter2"), salt, 1, 8*1024, 1, 32)
	encoded := encode(params{memory: 8 * 1024, iterations: 1, parallelism: 1}, salt, key)
	if ok, err := Verify(encoded, "hunter2"); !ok || err != nil {
		t.Errorf("a hash at 8 MiB and one iteration answered %v, %v", ok, err)
	}
	if ok, err := Verify(encoded, "hunter3"); ok || err != nil {
		t.Errorf("a wrong password against it answered %v, %v", ok, err)
	}
}

// What a hash may not ask for, and what is not a hash at all, is unreadable rather than a password
// that does not match, and costs no hashing: a hash above the baseline would spend memory the bound
// on concurrent hashing does not count.
func TestAHashAskingForMoreThanTheBaselineIsUnreadable(t *testing.T) {
	salt := b64.EncodeToString([]byte("sixteen byte slt"))
	key := b64.EncodeToString(make([]byte, 32))
	for name, encoded := range map[string]string{
		"more memory":          "$argon2id$v=19$m=65536,t=2,p=1$" + salt + "$" + key,
		"more iterations":      "$argon2id$v=19$m=19456,t=3,p=1$" + salt + "$" + key,
		"more lanes":           "$argon2id$v=19$m=19456,t=2,p=4$" + salt + "$" + key,
		"no lane":              "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key,
		"no iteration":         "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key,
		"too little memory":    "$argon2id$v=19$m=7,t=2,p=1$" + salt + "$" + key,
		"past 32 bits":         "$argon2id$v=19$m=4294967296,t=2,p=1$" + salt + "$" + key,
		"a leading zero":       "$argon2id$v=19$m=019456,t=2,p=1$" + salt + "$" + key,
		"a sign":               "$argon2id$v=19$m=+19456,t=2,p=1$" + salt + "$" + key,
		"costs out of order":   "$argon2id$v=19$t=2,m=19456,p=1$" + salt + "$" + key,
		"a cost missing":       "$argon2id$v=19$m=19456,t=2$" + salt + "$" + key,
		"Argon2i":              "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"version 16":           "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key,
		"no version":           "$argon2id$m=19456,t=2,p=1$" + salt + "$" + key,
		"a padded salt":        "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "==$" + key,
		"a short salt":         "$argon2id$v=19$m=19456,t=2,p=1$" + b64.EncodeToString([]byte("seven b")) + "$" + key,
		"a short key":          "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64.EncodeToString(make([]byte, 15)),
		"a key past 64 bytes":  "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64.EncodeToString(make([]byte, 65)),
		"url-safe base64":      "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawURLEncoding.EncodeToString([]byte("sixteen byte sl\xff")) + "$" + key,
		"a part more":          "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + "$",
		"bcrypt":               "$2b$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW",
		"nothing":              "",
		"a key without a salt": "$argon2id$v=19$m=19456,t=2,p=1$" + key,
	} {
		ok, err := Verify(encoded, "whatever")
		if ok || !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: verifying %q answered %v, %v", name, encoded, ok, err)
		}
	}
}
