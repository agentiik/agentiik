package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
)

// theToken is the bootstrap token of the installations these tests make: the v0.2 operator token
// under its v0.3.0 name, as a person sets it in .env.
const theToken = "agk_op_3q2Z7x9Kf1LmQ8vR4tYw6pBn0sDhJc5A"

// theHash is what init keeps of it, in hexadecimal, which is no token.
var theHash = func() string {
	sum := sha256.Sum256([]byte(theToken))
	return hex.EncodeToString(sum[:])
}()

// bootstrapped keeps the hash of theToken in the database of an installation migrated already, as
// init does at every run.
func bootstrapped(t *testing.T, database config.Database) {
	t.Helper()
	pool, err := db.Open(t.Context(), database.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := bootstrapToken(t.Context(), pool, "init", theToken, nil, io.Discard); err != nil {
		t.Fatalf("the bootstrap token could not be kept: %s", err)
	}
}
