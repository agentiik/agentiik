package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/agentiik/agentiik/api"
)

// The interim operator, until v0.3.0.
//
// A v0.2.0 installation has no principals, and api.DenyAll, which is what an installation with no
// access model has, refuses pushing, starting a run, creating a pool, issuing a join token and
// writing a secret alike. So it has one operator: a token whose hash sits in the file
// AGK_OPERATOR_TOKEN_FILE names, identified as one principal allowed every permission at every
// scope, while everyone else stays denied.
//
// Both halves are here, in the API's main package, and nowhere else, so that v0.3.0's bootstrap
// token deletes them in one place. Package api keeps DenyAll as its default and knows nothing of
// this: a test of the routes that forgot to name an authorizer still gets the refusal, and nothing
// a library links can admit the operator by accident.

// theOperator is the principal the token identifies. Nothing else in v0.2.0 is identified, so it
// cannot be mistaken for anybody.
const theOperator api.Principal = "operator"

// operator is both halves: what identifies the token, and what allows it.
type operator struct {
	hash [sha256.Size]byte
}

// newOperator takes the SHA-256 of the token, in hexadecimal, as internal/config read it.
//
// The hash and never the token, as every credential of the installation is kept: "stored hashed,
// shown once at creation". What is compared is the hash of what a request presents, so the file
// is worth nothing to somebody who reads it.
func newOperator(hash string) (*operator, error) {
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != sha256.Size {
		return nil, errors.New("the operator token's hash is not a SHA-256 in hexadecimal")
	}
	o := &operator{}
	copy(o.hash[:], raw)
	return o, nil
}

// identify is the router's Identify: the operator for a request bearing the token, and nobody for
// anything else, which the router answers 401.
//
// Nobody rather than an error, for a token that is wrong as much as for one that is missing: a
// caller who is not the operator is not a failure, it is a caller who gets what an unauthenticated
// caller gets. The comparison takes as long whatever the token is, since what is compared is a hash
// of it, and the hashes are compared in constant time besides.
func (o *operator) identify(r *http.Request) (api.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", nil
	}
	presented := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(presented[:], o.hash[:]) != 1 {
		return "", nil
	}
	return theOperator, nil
}

// Allow allows the operator every permission at every scope, and everyone else nothing.
func (o *operator) Allow(_ context.Context, who api.Principal, what api.Permission, _ api.Target) (bool, error) {
	return who == theOperator && what.Valid(), nil
}
