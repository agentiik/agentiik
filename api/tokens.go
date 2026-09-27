package api

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/agentiik/agentiik/internal/ulid"
)

// API tokens, minted, listed and revoked one by one: POST and GET /api/v1/auth/tokens, and DELETE
// /api/v1/auth/tokens/{id}.
//
// "Held by a user or a service account, never a group, which holds no credential of its own. Stored
// hashed, shown once, revocable one by one, listed with last use and device label. Expires after 90
// days unless asked otherwise, and after a year at most." A token is minted for its caller, or for
// a service account of a namespace the caller owns, and what it may do is decided at every request
// it makes, from its principal's grants narrowed by its scope: nothing here grants anything.
//
// A revocation takes effect at the token's next request, since Principals identifies a token from
// the database at every one, and a log stream open on it asks again every 30 s.

// The lifetimes the page gives a token: "an API token's expiry is 90 days by default, so a token
// forgotten in a script stops on its own within a season, and a year at most, so that no token is
// a credential for good and every one is renewed by someone who still means it". Both are counted
// on the calendar in UTC from the moment the token is minted; the table holds the year to 8808
// hours, which a year counted any way never reaches.
const (
	tokenDefaultDays = 90
	tokenMostYears   = 1
)

// deviceLabelMax is how long a device label may be, in characters, as the wire's apiToken holds it:
// enough to say which machine or which script, short enough to list.
const deviceLabelMax = 256

// ulidForm is the alphabet an identifier the engine mints is written in, the wire's ulid: a
// token's, a grant's or a notification's. An identifier outside it names nothing, and is answered
// as the absence it is before the database is asked.
var ulidForm = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{1,255}$`)

// The refusals of the token routes that say something of their own.
const (
	// A scoped token mints nothing: what it minted would carry its principal's rights and not
	// its narrowing.
	narrowedMintsNothing = "a token narrowed by a scope mints no token, since the token it minted would not be narrowed by it: mint from a credential that carries no scope"

	// The bootstrap token's "one lasting use is to create the first human administrator", and a
	// token it minted would be a second one, outliving it.
	bootstrapMintsNothing = "the bootstrap token mints no API token: its one lasting use is to create the first administrator, with agk user create LOGIN --admin, and a token is minted by the users and service accounts that follow"

	// A token the caller may not revoke answers what one that does not exist answers.
	noSuchToken = "no such token, or not yours"
)

// TokenRequest is what POST /api/v1/auth/tokens reads, openapi.json's tokenRequest. Every member is
// optional: an empty object mints the caller a token for 90 days with its full rights.
type TokenRequest struct {
	// Principal is whose token it is: the caller's own login, or NS/NAME for a service account
	// of a namespace the caller owns. Empty is the caller.
	Principal string `json:"principal,omitempty"`

	// DeviceLabel is what the token is minted on or for, "so that a list of tokens can be read
	// and the one on a lost machine revoked".
	DeviceLabel string `json:"device_label,omitempty"`

	// ExpiresAt is when it stops being accepted, and nil is 90 days after it is minted.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Scope narrows it, and nil is the principal's full rights.
	Scope *TokenScope `json:"scope,omitempty"`
}

func (q *TokenRequest) field(b *body, name string) error {
	switch name {
	case "principal":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		if err := text(b, &q.Principal); err != nil {
			return err
		}
		if q.Principal == "" {
			return errors.New("principal is empty: a token of your own leaves it out, and one of a service account names it NS/NAME")
		}
		return nil
	case "device_label":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		if err := text(b, &q.DeviceLabel); err != nil {
			return err
		}
		switch {
		case q.DeviceLabel == "":
			return errors.New("device_label is empty: it says which machine or which script the token is for, and a token with none leaves it out")
		case utf8.RuneCountInString(q.DeviceLabel) > deviceLabelMax:
			return fmt.Errorf("device_label is %d characters, and it is at most %d, to be listed beside the token", utf8.RuneCountInString(q.DeviceLabel), deviceLabelMax)
		case strings.ContainsRune(q.DeviceLabel, 0):
			// PostgreSQL holds no U+0000 in text, so a label holding one would be refused by
			// the database, as a failure of the API's own, rather than here, in front of
			// whoever sent it.
			return errors.New("device_label holds U+0000, which no label is written with and the installation cannot keep")
		}
		return nil
	case "expires_at":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		var written string
		if err := text(b, &written); err != nil {
			return err
		}
		at, err := instantOf(written)
		if err != nil {
			return fmt.Errorf("expires_at: %w", err)
		}
		q.ExpiresAt = &at
		return nil
	case "scope":
		if absent, err := null(b); absent || err != nil {
			return err
		}
		var s TokenScope
		if err := b.fields(&s); err != nil {
			return err
		}
		q.Scope = &s
		return nil
	}
	return unknown(name)
}

// null reads a null, and answers whether it read one: a member written null is one left out.
func null(b *body) (bool, error) {
	if b.d.PeekKind() != jsontext.KindNull {
		return false, nil
	}
	_, err := b.d.ReadToken()
	return true, malformed(err)
}

// TokenScope is what a token is narrowed to, as the wire's apiToken.scope writes it: the
// permissions it keeps and the namespaces and workflows it reaches, each nil where it does not
// narrow that way.
type TokenScope struct {
	Permissions []string `json:"permissions,omitempty"`
	Within      []string `json:"within,omitempty"`
}

func (s *TokenScope) field(b *body, name string) error {
	switch name {
	case "permissions":
		return texts(b, &s.Permissions, namesMax, fmt.Sprintf("a token's scope keeps at most %d permissions, and there are nine", namesMax))
	case "within":
		return texts(b, &s.Within, namesMax, fmt.Sprintf("a token's scope reaches at most %d namespaces and workflows", namesMax))
	}
	return unknown(name)
}

// check refuses a scope the wire refuses: neither half present, a half that is empty, one naming
// something twice, a permission that is not one of the nine, or a scope no grant could name. A
// permission the principal lacks is taken, since it grants nothing.
func (s TokenScope) check() error {
	switch {
	case s.Permissions == nil && s.Within == nil:
		return errors.New("the scope narrows by permissions, within or both, and this one names neither: a token with its principal's full rights leaves scope out")
	case s.Permissions != nil && len(s.Permissions) == 0:
		return errors.New("the scope keeps no permission, which would be a token that opens nothing: name the permissions it keeps, or leave permissions out to keep them all")
	case s.Within != nil && len(s.Within) == 0:
		return errors.New("the scope reaches nothing, which would be a token that opens nothing: name the namespaces and workflows it reaches, or leave within out to reach wherever its principal does")
	}
	if err := distinct(s.Permissions, "permission", func(string) error { return nil }); err != nil {
		return err
	}
	if err := distinct(s.Within, "namespace or workflow", func(string) error { return nil }); err != nil {
		return err
	}
	_, err := access.ParseTokenScope(s.Permissions, s.Within)
	return err
}

// scopeOf is a stored scope as the wire writes it, nil where the token is not narrowed.
func scopeOf(t db.APIToken) *TokenScope {
	if t.Permissions == nil && t.Within == nil {
		return nil
	}
	return &TokenScope{Permissions: t.Permissions, Within: t.Within}
}

// APIToken is a token as it is listed, the wire's apiToken: never its value.
type APIToken struct {
	ID          string      `json:"id"`
	Principal   string      `json:"principal"`
	DeviceLabel string      `json:"device_label,omitempty"`
	Scope       *TokenScope `json:"scope,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	ExpiresAt   time.Time   `json:"expires_at"`

	// LastUsedAt is the last request it authenticated, and nil until it has been used.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// listedToken is a stored token as it is answered.
func listedToken(t db.APIToken) APIToken {
	listed := APIToken{
		ID: t.ID, Principal: t.Principal, DeviceLabel: t.DeviceLabel, Scope: scopeOf(t),
		CreatedAt: t.CreatedAt.UTC(), ExpiresAt: t.ExpiresAt.UTC(),
	}
	if !t.LastUsedAt.IsZero() {
		used := t.LastUsedAt.UTC()
		listed.LastUsedAt = &used
	}
	return listed
}

// IssuedToken is a token just minted, openapi.json's issuedToken: its value, this once, and its
// record as the listing shows it from now on.
type IssuedToken struct {
	Token    string   `json:"token"`
	APIToken APIToken `json:"api_token"`
}

// TokenList is openapi.json's tokenList: the tokens still accepted, newest first.
type TokenList struct {
	Tokens []APIToken `json:"tokens"`
}

// TokenOptions are what the token routes are given.
type TokenOptions struct {
	Pool *db.Pool

	// Now is the clock tokens are minted and revoked by, the wall clock where it is nil.
	Now func() time.Time
}

// TokenAPI serves the token routes.
type TokenAPI struct {
	pool *db.Pool
	now  func() time.Time
}

// NewTokens registers the token routes on a router, each a route about its caller's own
// credentials: see Own.
func NewTokens(rt *Router, o TokenOptions) (*TokenAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a token is kept there")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	t := &TokenAPI{pool: o.Pool, now: o.Now}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"POST", "/api/v1/auth/tokens", t.mint},
		{"GET", "/api/v1/auth/tokens", t.list},
		{"DELETE", "/api/v1/auth/tokens/{id}", t.revoke},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// mint answers a token for the caller or for a service account of a namespace it owns, shown this
// once, and records it in the audit log in the transaction that writes it.
//
// What the caller presented is judged before the body is read, since no body changes it: a
// narrowed token mints nothing, and neither does the bootstrap token.
func (t *TokenAPI) mint(w http.ResponseWriter, r *http.Request, caller Caller) {
	switch {
	case caller.Principal == BootstrapOperator:
		fail(w, http.StatusForbidden, bootstrapMintsNothing)
		return
	case caller.Narrowed():
		fail(w, http.StatusForbidden, narrowedMintsNothing)
		return
	}

	var q TokenRequest
	if err := readAtMost(r, &q, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	var narrowed TokenScope
	if q.Scope != nil {
		if err := q.Scope.check(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		narrowed = *q.Scope
	}
	holder := string(caller.Principal)
	if q.Principal != "" && q.Principal != holder {
		if err := access.TokenHolder(q.Principal); err != nil {
			fail(w, http.StatusBadRequest, "principal: "+err.Error())
			return
		}
		// Another principal is a service account of a namespace the caller owns, or nobody the
		// caller may mint for, whichever it is: one sentence for the absent and the hidden.
		namespace, _, account := strings.Cut(q.Principal, "/")
		owned, err := caller.Owned(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
			return
		}
		if !account || !slices.Contains(owned, namespace) {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("principal %s is neither you nor a service account of a namespace you own", q.Principal))
			return
		}
		holder = q.Principal
	}

	// Read off one clock, to the microsecond PostgreSQL keeps, so that the expiry answered is the
	// one stored and the year is counted from the creation the table checks it against.
	now := t.now().UTC().Truncate(time.Microsecond)
	expires := now.AddDate(0, 0, tokenDefaultDays)
	if q.ExpiresAt != nil {
		expires = q.ExpiresAt.UTC().Truncate(time.Microsecond)
		switch {
		case !expires.After(now):
			fail(w, http.StatusUnprocessableEntity, "expires_at has already passed: a token expires after it is minted")
			return
		case expires.After(now.AddDate(tokenMostYears, 0, 0)):
			fail(w, http.StatusUnprocessableEntity, "expires_at is more than a year away, and a token expires within a year, so that none is a credential for good")
			return
		}
	}

	clear, _, err := token.New(token.API, "")
	if err != nil {
		fail(w, http.StatusInternalServerError, "a token could not be minted")
		return
	}
	hash := sha256.Sum256([]byte(clear))
	row := db.APIToken{
		ID: ulid.New(), Hash: hash[:], Principal: holder,
		Permissions: narrowed.Permissions, Within: narrowed.Within,
		DeviceLabel: q.DeviceLabel, CreatedAt: now, ExpiresAt: expires,
	}
	issued := listedToken(row)
	err = t.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.MintToken(ctx, row); err != nil {
			return err
		}
		// Recorded by its identifier, and never by the token, which is shown once, here.
		detail := map[string]any{"principal": holder, "expires_at": expires.Format(time.RFC3339Nano)}
		if row.DeviceLabel != "" {
			detail["device_label"] = row.DeviceLabel
		}
		if issued.Scope != nil {
			detail["scope"] = issued.Scope
		}
		return auditToken(ctx, wide, holder, audit.Record{
			Actor: string(caller.Principal), Action: audit.APITokenCreate, Target: row.ID, Result: audit.Done,
			Detail: detail,
		})
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("principal %s names nobody", holder))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the token could not be minted")
		return
	}
	// It exists in this answer and nowhere else.
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusCreated, IssuedToken{Token: clear, APIToken: issued})
}

// list answers the caller's tokens and those of the service accounts of the namespaces it owns that
// are still accepted, newest first, with their expiry, last use, device label and scope, and never
// a value. A narrowed token is answered itself alone: see Caller.Narrowed.
func (t *TokenAPI) list(w http.ResponseWriter, r *http.Request, caller Caller) {
	owned, err := caller.Owned(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
		return
	}
	var tokens []db.APIToken
	err = t.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		tokens, err = wide.TokensOf(ctx, string(caller.Principal), owned, t.now())
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the tokens could not be read")
		return
	}
	listed := TokenList{Tokens: []APIToken{}}
	for _, tk := range tokens {
		if caller.Narrowed() && tk.ID != caller.Token {
			continue
		}
		listed.Tokens = append(listed.Tokens, listedToken(tk))
	}
	write(w, http.StatusOK, listed)
}

// revoke revokes one token from its next request: the caller's own, the one making this request
// included, or one of a service account of a namespace the caller owns, and records it in the audit
// log in the transaction that revokes it. A token revoked already is answered as revoked, and
// recorded as unchanged, since who asked is part of what happened. Any other is answered as one
// that does not exist.
func (t *TokenAPI) revoke(w http.ResponseWriter, r *http.Request, caller Caller) {
	id := r.PathValue("id")
	if !ulidForm.MatchString(id) {
		fail(w, http.StatusNotFound, noSuchToken)
		return
	}
	owned, err := caller.Owned(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the caller owns could not be read")
		return
	}
	err = t.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		tk, err := wide.Token(ctx, id)
		if err != nil {
			return err
		}
		if !caller.mayRevoke(tk, owned) {
			return db.ErrNoToken
		}
		was, err := wide.RevokeToken(ctx, tk.Principal, id, t.now())
		if err != nil {
			return err
		}
		result := audit.Done
		if !was {
			result = audit.Unchanged
		}
		return auditToken(ctx, wide, tk.Principal, audit.Record{
			Actor: string(caller.Principal), Action: audit.APITokenRevoke, Target: id, Result: result,
			Detail: map[string]any{"principal": tk.Principal},
		})
	})
	switch {
	case errors.Is(err, db.ErrNoToken):
		fail(w, http.StatusNotFound, noSuchToken)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the token could not be revoked")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// auditToken appends an act on a token to the audit log: in the namespace of the service account it
// belongs to, as a grant is recorded in the namespace it gives something in, since whoever reads the
// log for what was done in a namespace looks for who was handed a way in as that namespace's
// identity; and on the installation for a user's, which belongs to no namespace.
func auditToken(ctx context.Context, wide *db.Wide, principal string, r audit.Record) error {
	if namespace, _, account := strings.Cut(principal, "/"); account {
		return wide.AuditIn(ctx, namespace, r)
	}
	return wide.Audit(ctx, r)
}

// mayRevoke says whether the caller may revoke a token: its own, or one of a service account of a
// namespace it owns, and through a narrowed token the one it presented alone.
func (c Caller) mayRevoke(t db.APIToken, owned []string) bool {
	if c.Narrowed() {
		return c.Token != "" && t.ID == c.Token && t.Principal == string(c.Principal)
	}
	if t.Principal == string(c.Principal) {
		return true
	}
	namespace, _, account := strings.Cut(t.Principal, "/")
	return account && slices.Contains(owned, namespace)
}

// Owned is the router's Owners: the namespaces who owns, ordered by name, where it holds the owner
// role on the namespace by a grant of its own or of one of its groups, as access.Owns says.
//
// The bootstrap operator owns every namespace while it has not ended, as Allow answers it, and
// nothing after. A suspended user, and a login removed since, own nothing.
func (p *Principals) Owned(ctx context.Context, who Principal) ([]string, error) {
	if who == "" {
		return nil, nil
	}
	principal, _, bootstrapped, err := p.resolve(ctx, who)
	if err != nil {
		return nil, err
	}
	now := p.now()
	var owned []string
	err = p.pool.Installation(ctx, db.Authorisation, func(ctx context.Context, w *db.Wide) error {
		if who == BootstrapOperator {
			if !bootstrapped {
				return nil
			}
			all, err := w.Namespaces(ctx)
			for _, n := range all {
				owned = append(owned, n.Name)
			}
			return err
		}
		if principal.Ref == "" {
			return nil
		}
		grants, err := w.AccessGrantsAcross(ctx, principal, now)
		for _, g := range grants {
			namespace := g.Scope.Namespace
			if !slices.Contains(owned, namespace) && access.Owns(principal, grants, namespace, now) {
				owned = append(owned, namespace)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(owned)
	return owned, nil
}
