package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// The caller's credentials: GET /api/v1/me/credentials, "each with its identifier, kind, flags, label
// and last use, never a public key, a hash or a secret", and DELETE /api/v1/me/credentials/{id},
// which "removes one of the caller's credentials, by the identifier the listing shows, as a token is
// revoked by its own, and ends the sessions it opened. A removal below the policy minimum is
// refused."
//
// # What is listed
//
// A user's passkeys, the password where one is still stored and the TOTP generator where one is
// enrolled, oldest first, each as $defs/credential writes it: a passkey with its kind, synced or
// device-bound, and both its flags, so that its holder can reason about their exposure; a password
// and a generator by the identifier the engine minted, since each has none of its own. A service
// account holds none, and neither does the bootstrap token's operator. A token narrowed by a scope
// reaches no credential, since managing credentials is none of the nine a scope keeps.
//
// # What a removal leaves
//
// The rule is the policy's, as it applies to the account now: a passkey the policy accepts is not
// removed where fewer than min_passkeys would remain, nor the password where the account holds fewer
// than that, "so that losing a device is an inconvenience rather than an incident"; nor, on an
// installation addressed by an IP address, the password, which is the one credential that signs
// anybody in there; nor the last credential of any type, which would leave the account nothing to
// sign in with. A passkey the policy does not accept, a synced one where device_bound_only applies,
// signs nobody in and counts for nothing, so removing it takes nothing the rule counts. The TOTP
// generator goes with the password, and is removed on its own at DELETE /api/v1/me/totp alone, with a
// code it shows, as passwords_totp.go says why: here it is a 409 saying so.
//
// A removal is made from a browser's session, as the password is set and removed: a bearer token
// that leaked would otherwise be a way to take its holder's passkeys down to the minimum, or their
// password away. The sessions the credential opened end with its row, the one it is removed from
// among them where that one was.

// CredentialOptions are what the credential routes are given.
type CredentialOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: a host that is an IP address is
	// an installation where no passkey signs anybody in, and where the password is kept.
	PublicURL string

	// Now is the clock grants expire by, the wall clock where it is nil.
	Now func() time.Time
}

// CredentialAPI serves the caller's credentials.
type CredentialAPI struct {
	pool        *db.Pool
	now         func() time.Time
	ipAddressed bool
}

// The sentences the credential routes are refused with.
const (
	// narrowedReachesNoCredential is a token narrowed by a scope asking about credentials.
	narrowedReachesNoCredential = "a token narrowed by a scope reaches no credential of its holder's, since managing credentials is none of the permissions a scope keeps: use a credential that carries no scope"

	// noSuchCredential is a credential the caller does not hold, one sentence whether it is
	// somebody else's or nobody's.
	noSuchCredential = "no such credential, or not yours"

	// lastCredential is the removal of the last credential an account signs in with.
	lastCredential = "removing this credential would leave this account nothing to sign in with: enrol another first"

	// totpRemovedWithACode is a TOTP generator removed here rather than with a code it shows.
	totpRemovedWithACode = "a TOTP generator is removed with a code it shows, at DELETE /api/v1/me/totp, so that a session left open does not take a second factor away without the device that holds it"

	// credentialsRemovedFromASession is a credential removed with a bearer token.
	credentialsRemovedFromASession = "a credential is removed from a browser's session, on the sign-in page, and this request carries a bearer token: a token that leaked would otherwise be a way to take its holder's passkeys and password away"
)

// credentialForm is the grammar of a credential's identifier, $defs/passkey/properties/id: base64url
// with no padding, at most credentialMax characters.
var credentialForm = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// credentialMax is the longest identifier a credential has: the 1,364 characters a credential ID of
// 1,023 bytes, the most a passkey's may be, is written in.
const credentialMax = 1364

// NewCredentials registers the routes about the caller's credentials on a router, each taking Own.
func NewCredentials(rt *Router, o CredentialOptions) (*CredentialAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the credentials are kept there")
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("api: the credential routes are given the public URL, whose host says whether a passkey signs anybody in, and the one given names none")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &CredentialAPI{pool: o.Pool, now: o.Now, ipAddressed: net.ParseIP(u.Hostname()) != nil}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"GET", "/api/v1/me/credentials", s.list},
		{"DELETE", "/api/v1/me/credentials/{id}", s.remove},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// CredentialList is openapi.json's credentialList: each a Passkey or a PasswordHeld, as its type
// says.
type CredentialList struct {
	Credentials []any `json:"credentials"`
}

// credentialOf is one credential as it is listed.
func credentialOf(c db.Credential) any {
	if c.Type == db.CredentialPasskey {
		return passkeyOf(c)
	}
	return heldOf(c)
}

// list is GET /api/v1/me/credentials.
func (s *CredentialAPI) list(w http.ResponseWriter, r *http.Request, caller Caller) {
	if caller.Narrowed() {
		fail(w, http.StatusForbidden, narrowedReachesNoCredential)
		return
	}
	var held []db.Credential
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		// A service account's and the bootstrap operator's name no row of credentials, which
		// belong to users alone, and are answered the none they hold.
		held, err = wide.CredentialsOf(ctx, string(caller.Principal))
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the credentials could not be read")
		return
	}
	list := CredentialList{Credentials: []any{}}
	for _, c := range held {
		list.Credentials = append(list.Credentials, credentialOf(c))
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, list)
}

// errBelow is a removal that would leave fewer passkeys the policy accepts than min_passkeys: of the
// password, where the account holds fewer than that, or of a passkey the policy accepts, which would
// bring it below.
type errBelow struct {
	what      string
	left, min int
}

func (e *errBelow) Error() string {
	passkeys := "passkeys"
	if e.left == 1 {
		passkeys = "passkey"
	}
	return fmt.Sprintf("removing %s would leave this account with %d %s the policy accepts, and min_passkeys is %d: enrol another passkey first, on another device", e.what, e.left, passkeys, e.min)
}

// errOnlyHere is the password removed on an installation addressed by an IP address.
var errOnlyHere = errors.New(onlyCredentialHere)

// errLast is the removal of the last credential an account signs in with.
var errLast = errors.New(lastCredential)

// errWithACode is a TOTP generator asked to go without a code it shows.
var errWithACode = errors.New(totpRemovedWithACode)

// removable refuses the removal of target from an account holding held, under the policy that
// applies to it: errBelow, errOnlyHere, errWithACode or errLast, and nil where it may go.
func removable(policy accountPolicy, held []db.Credential, target db.Credential, ipAddressed bool) error {
	accepted := policy.passkeys(held)
	left := 0
	for _, c := range held {
		switch {
		case c.ID == target.ID:
		case c.Type == db.CredentialTOTP && target.Type == db.CredentialPassword:
			// Goes with the password it stands beside.
		default:
			left++
		}
	}
	switch {
	case target.Type == db.CredentialTOTP:
		return errWithACode
	case target.Type == db.CredentialPassword && ipAddressed:
		return errOnlyHere
	case target.Type == db.CredentialPassword && accepted < policy.minPasskeys:
		return &errBelow{what: "the password", left: accepted, min: policy.minPasskeys}
	case policy.accepts(target) && accepted-1 < policy.minPasskeys:
		return &errBelow{what: "this passkey", left: accepted - 1, min: policy.minPasskeys}
	case left == 0:
		return errLast
	}
	return nil
}

// answerRemoval answers a removal removable or the store refused, and false where it refused none.
func answerRemoval(w http.ResponseWriter, err error) bool {
	var below *errBelow
	switch {
	case errors.As(err, &below):
		failSetting(w, http.StatusConflict, below.Error(), minPasskeys)
	case errors.Is(err, errOnlyHere), errors.Is(err, errLast), errors.Is(err, errWithACode):
		fail(w, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}

// removedEntries are the entries recording the removal of c from login, and of the TOTP generator
// totp beside it where c is the password and a generator is held.
func removedEntries(login string, c db.Credential, totp db.Credential) []entry {
	detail := map[string]any{"type": c.Type}
	if c.Type == db.CredentialPasskey {
		detail["kind"], detail["label"] = passkeyOf(c).Kind, c.Label
	}
	entries := []entry{{record: audit.Record{
		Actor: login, Action: audit.CredentialRemove, Target: c.ID, Result: audit.Done, Detail: detail,
	}}}
	if c.Type == db.CredentialPassword && totp.ID != "" {
		entries = append(entries, entry{record: audit.Record{
			Actor: login, Action: audit.CredentialRemove, Target: totp.ID, Result: audit.Done,
			Detail: map[string]any{"type": db.CredentialTOTP, "reason": "removed with the password it stood beside"},
		}})
	}
	return entries
}

// remove is DELETE /api/v1/me/credentials/{id}.
//
// One transaction under the user's row, as every act on an account takes it: the credentials and the
// policy read, the rule held, the credential removed, and credential.remove recorded, a generator
// going with the password recorded beside it.
func (s *CredentialAPI) remove(w http.ResponseWriter, r *http.Request, caller Caller) {
	if caller.Token != "" || caller.Principal == BootstrapOperator {
		fail(w, http.StatusForbidden, credentialsRemovedFromASession)
		return
	}
	login := string(caller.Principal)
	id := r.PathValue("id")
	if len(id) > credentialMax || !credentialForm.MatchString(id) {
		fail(w, http.StatusNotFound, noSuchCredential)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if _, err := wide.HoldUser(ctx, login); err != nil {
			return err
		}
		held, err := wide.CredentialsOf(ctx, login)
		if err != nil {
			return err
		}
		var target, totp db.Credential
		for _, c := range held {
			switch {
			case c.ID == id:
				target = c
			case c.Type == db.CredentialTOTP:
				totp = c
			}
		}
		if target.ID == "" {
			return db.ErrNoCredential
		}
		policy, err := policyFor(ctx, wide, login, now, s.ipAddressed)
		if err != nil {
			return err
		}
		if err := removable(policy, held, target, s.ipAddressed); err != nil {
			return err
		}
		if err := wide.RemoveCredential(ctx, login, id); err != nil {
			return err
		}
		return appendEntries(ctx, wide, removedEntries(login, target, totp))
	})
	switch {
	case answerRemoval(w, err):
	case errors.Is(err, db.ErrNoCredential):
		fail(w, http.StatusNotFound, noSuchCredential)
	case errors.Is(err, db.ErrNoPrincipal):
		// Removed since the session was read, which the removal ended.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noSession)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the credential could not be removed")
	default:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
