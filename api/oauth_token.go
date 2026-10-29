package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The token endpoint, POST /api/v1/auth/oauth/token: an MCP client's authorisation code traded, with
// its verifier, for an access token and a refresh token, and a refresh token for the next pair.
//
// A grant is one row of its person's tokens (db.APIToken with an Audience), so that "an OAuth access
// token for /mcp should be, or behave as, a token of the principal, narrowed to what it was granted,
// revocable and listed as the principal's other tokens are": its row's hash is its current access
// token's, and a refresh replaces it. Its refresh token is single use, "for public clients,
// authorization servers MUST rotate refresh tokens", and the one spent presented again ends the
// grant, since "the authorization server cannot determine which party submitted the invalid refresh
// token, but it will revoke the active refresh token as well as the access authorization grant".
//
// A code is redeemed as agk login's is at the exchange, from the same table and under the same
// checks: taken once its form holds and before anything else, so that the first presentation spends
// it whatever it carries, then held to its client, its redirect URI and its verifier, and to what its
// sign-in may mint now (codeOpens). A refusal is OAuth's error, "invalid_grant" for every code or
// refresh token that opens nothing, so that a refusal teaches nothing about which part was wrong, and
// a refused code is recorded as a sign-in refused, as a refused exchange is.

// refreshToken is openapi.json's refreshToken: agkrefresh_ and 256 bits of base64url.
var refreshToken = regexp.MustCompile(`^agkrefresh_[A-Za-z0-9_-]{43,}$`)

// unknownAuthorisation is the account a refused code names where it names none: never minted, or
// taken already, written with spaces as unknownExchange is.
const unknownAuthorisation = "an unknown authorisation code"

// authorisationCredential is what signin.fail's detail says the token endpoint was presented: OAuth's
// authorization code, as its grant type names it.
const authorisationCredential = "authorization_code"

// noGrant is the description of every code and refresh token that opens nothing.
const noGrant = "that code or refresh token opens nothing: used already, past its time, never issued, revoked, or not issued to this client, for this redirect URI or for this verifier"

// The reasons a refused code is recorded with, beside those the exchange records.
const (
	codeOfTheTerminal  = "the code was minted for agk login, which trades it at its exchange"
	codeOfAnother      = "the code was minted for another client"
	codeRedirectedElse = "the redirect URI is not the one the code was sent to"
	codeOtherResource  = "the resource is not the one the code was minted for"
)

// tokenAnswered is OAuth's token response, openapi.json's oauthToken.
type tokenAnswered struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// tokenRefused answers OAuth's error response: "HTTP 400 (Bad Request) status code", error and
// error_description, with no-store as every answer of the endpoint carries.
func tokenRefused(w http.ResponseWriter, code, why string) {
	shownOnce(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": why})
}

// token is POST /api/v1/auth/oauth/token. The form is read whole, at most smallMaxBytes, and a
// parameter written twice refused, since "request and response parameters defined by this
// specification MUST NOT be included more than once".
func (s *OAuth) token(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/x-www-form-urlencoded" {
		tokenRefused(w, "invalid_request", "the token endpoint takes a form, application/x-www-form-urlencoded, as OAuth writes its token request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, smallMaxBytes))
	if err != nil {
		tokenRefused(w, "invalid_request", fmt.Sprintf("the form could not be read whole, or is longer than %d bytes", smallMaxBytes))
		return
	}
	q, err := url.ParseQuery(string(raw))
	if err != nil {
		tokenRefused(w, "invalid_request", "the form does not read as application/x-www-form-urlencoded")
		return
	}
	for name, values := range q {
		if len(values) > 1 {
			tokenRefused(w, "invalid_request", "the form names "+describable(name)+" more than once")
			return
		}
	}
	switch q.Get("grant_type") {
	case "authorization_code":
		s.redeem(w, r, q)
	case "refresh_token":
		s.refresh(w, r, q)
	case "":
		tokenRefused(w, "invalid_request", "the form names no grant_type: authorization_code trades a code, refresh_token a refresh token")
	default:
		tokenRefused(w, "unsupported_grant_type", "the grant types are authorization_code and refresh_token")
	}
}

// pair mints an access token and a refresh token, and answers their values and hashes.
func pair() (access, refresh string, accessHash, refreshHash [32]byte, err error) {
	if access, _, err = token.New(token.API, ""); err != nil {
		return
	}
	if refresh, _, err = token.New(token.Refresh, ""); err != nil {
		return
	}
	return access, refresh, sha256.Sum256([]byte(access)), sha256.Sum256([]byte(refresh)), nil
}

// redeem is the authorization_code grant: "code, code_verifier, client_id and redirect_uri as the
// authorisation had them, and resource, the same, where it is sent".
func (s *OAuth) redeem(w http.ResponseWriter, r *http.Request, q url.Values) {
	code, verifier, client, redirect, resource := q.Get("code"), q.Get("code_verifier"), q.Get("client_id"), q.Get("redirect_uri"), q.Get("resource")
	switch {
	case code == "" || verifier == "" || client == "" || redirect == "":
		tokenRefused(w, "invalid_request", "an authorization_code grant carries code, code_verifier, client_id and redirect_uri")
		return
	case !codeVerifier.MatchString(verifier):
		tokenRefused(w, "invalid_request", "code_verifier is 43 to 128 unreserved characters, as RFC 7636 writes one")
		return
	case !exchangeCode.MatchString(code):
		// Never minted, since every code is written so: nothing is looked up, and nothing recorded.
		tokenRefused(w, "invalid_grant", noGrant)
		return
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte(code))
	failed := exchangeFailure{address: s.signIns.addressOf(r), target: unknownAuthorisation}
	refused := func(oauthCode, why string) {
		s.refuse(r, failed, now)
		tokenRefused(w, oauthCode, why)
	}
	var taken db.ExchangeCode
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		taken, err = wide.TakeExchangeCode(ctx, hash[:], now)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoExchangeCode):
		failed.reason = noLiveCode
		refused("invalid_grant", noGrant)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the code could not be read")
		return
	}
	failed.target, failed.credential = taken.Login, taken.Credential
	minted := taken.OAuth
	answered := sha256.Sum256([]byte(verifier))
	switch {
	case minted == nil:
		failed.reason = codeOfTheTerminal
	case minted.ClientID != client:
		failed.reason = codeOfAnother
	case minted.RedirectURI != redirect:
		failed.reason = codeRedirectedElse
	case subtle.ConstantTimeCompare([]byte(b64.EncodeToString(answered[:])), []byte(taken.CodeChallenge)) != 1:
		// RFC 7636 section 4.6, in constant time as every credential is compared.
		failed.reason = verifierMismatch
	}
	if failed.reason != "" {
		refused("invalid_grant", noGrant)
		return
	}
	failed.verified = true
	if resource != "" && resource != minted.Resource {
		failed.reason = codeOtherResource
		refused("invalid_target", "resource is not the one the code was allowed for, which the token is issued for and accepted at alone")
		return
	}

	access, refresh, accessHash, refreshHash, err := pair()
	if err != nil {
		fail(w, http.StatusInternalServerError, "a token could not be minted")
		return
	}
	row := db.APIToken{
		ID: ulid.New(), Hash: accessHash[:], Principal: taken.Login, Permissions: minted.Scope,
		DeviceLabel: minted.ClientName, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, grantDays),
		Audience: minted.Resource, ClientID: minted.ClientID, AccessExpiresAt: now.Add(accessTokenLife),
	}
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if err := codeOpens(ctx, wide, taken, now, s.ipAddressed); err != nil {
			return err
		}
		detail := map[string]any{"credential": taken.Credential, "client_id": minted.ClientID, "audience": minted.Resource}
		if err := mintToken(ctx, wide, row, taken.Login, detail); err != nil {
			return err
		}
		return wide.IssueRefreshToken(ctx, row.ID, refreshHash[:], now)
	})
	var gone *exchangeOpensNothing
	switch {
	case errors.As(err, &gone):
		failed.reason = gone.reason
	case errors.Is(err, errForbidden):
		failed.reason = codeForbidden
	case errors.Is(err, errEnrolsOnly):
		failed.reason = codeEnrolsOnly
	case errors.Is(err, errSynced):
		failed.reason = codeSynced
	case errors.Is(err, errTokensMost):
		failed.reason = codeTokensMost
		refused("invalid_grant", fmt.Sprintf("%s holds %d live tokens, the most one principal may hold: revoke one no longer used, and allow the client again", taken.Login, tokensMost))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the token could not be minted")
		return
	}
	if failed.reason != "" {
		refused("invalid_grant", noGrant)
		return
	}
	// They exist in this answer and nowhere else.
	shownOnce(w, http.StatusOK, tokenAnswered{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTokenLife / time.Second),
		RefreshToken: refresh, Scope: strings.Join(minted.Scope, " "),
	})
}

// errRefreshRefused is a refresh refused after its token was found, whose transaction is rolled back
// so that the refresh token is not spent: a request from somebody holding it and naming another
// client, resource or scope than the grant's spends nothing the client it was issued to holds.
var errRefreshRefused = errors.New("api: the refresh is refused")

// refresh is the refresh_token grant: "refresh_token and the client_id it was issued to; scope and
// resource where sent, within the grant's", answering a new access token and a new refresh token.
// "The requested scope MUST NOT include any scope not originally granted by the resource owner", and
// one naming fewer narrows the grant from then on, which only ever takes something away.
func (s *OAuth) refresh(w http.ResponseWriter, r *http.Request, q url.Values) {
	presented, client, scope, resource := q.Get("refresh_token"), q.Get("client_id"), q.Get("scope"), q.Get("resource")
	switch {
	case presented == "" || client == "":
		tokenRefused(w, "invalid_request", "a refresh_token grant carries refresh_token and the client_id it was issued to")
		return
	case !refreshToken.MatchString(presented):
		tokenRefused(w, "invalid_grant", noGrant)
		return
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte(presented))
	access, refresh, accessHash, refreshHash, err := pair()
	if err != nil {
		fail(w, http.StatusInternalServerError, "a token could not be minted")
		return
	}
	var answer tokenAnswered
	var refusal *oauthRefused
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		grant, err := wide.TakeRefreshToken(ctx, hash[:], now)
		var spent *db.RefreshTokenSpent
		switch {
		case errors.As(err, &spent):
			refusal = &oauthRefused{code: "invalid_grant", why: noGrant}
			return s.endGrant(ctx, wide, spent.Token, now)
		case errors.Is(err, db.ErrNoRefreshToken):
			refusal = &oauthRefused{code: "invalid_grant", why: noGrant}
			return nil
		case err != nil:
			return err
		}
		switch {
		case grant.ClientID != client:
			refusal = &oauthRefused{code: "invalid_grant", why: noGrant}
			return errRefreshRefused
		case resource != "" && resource != grant.Audience:
			refusal = &oauthRefused{code: "invalid_target", why: "resource is not the one the grant was allowed for, which its tokens are accepted at alone"}
			return errRefreshRefused
		}
		kept := grant.Permissions
		if scope != "" {
			kept = nil
			for _, asked := range strings.Split(scope, " ") {
				if !slices.Contains(grant.Permissions, asked) || slices.Contains(kept, asked) {
					refusal = &oauthRefused{code: "invalid_scope", why: "scope names " + describable(asked) + ", which the grant does not keep or which it names twice: a refresh asks for the grant's scopes or fewer"}
					return errRefreshRefused
				}
				kept = append(kept, asked)
			}
		}
		until := now.Add(accessTokenLife)
		if grant.ExpiresAt.Before(until) {
			until = grant.ExpiresAt
		}
		if err := wide.RotateToken(ctx, grant.ID, accessHash[:], until, kept); err != nil {
			return err
		}
		if err := wide.IssueRefreshToken(ctx, grant.ID, refreshHash[:], now); err != nil {
			return err
		}
		if err := wide.TokenUsed(ctx, grant.ID, now); err != nil {
			return err
		}
		answer = tokenAnswered{
			AccessToken: access, TokenType: "Bearer", ExpiresIn: int(until.Sub(now) / time.Second),
			RefreshToken: refresh, Scope: strings.Join(kept, " "),
		}
		return nil
	})
	switch {
	case refusal != nil && (err == nil || errors.Is(err, errRefreshRefused)):
		tokenRefused(w, refusal.code, refusal.why)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the token could not be refreshed")
		return
	}
	shownOnce(w, http.StatusOK, answer)
}

// endGrant revokes a grant whose refresh token was presented after it was spent, in the transaction
// wide is, and records it as api_token.revoke by the installation, which ended it, with why: "the
// authorization server cannot determine which party submitted the invalid refresh token", so neither
// keeps it, and its person allows the client again. A grant revoked already is left as it is and
// recorded nothing, since nothing more ended.
func (s *OAuth) endGrant(ctx context.Context, wide *db.Wide, grant db.APIToken, now time.Time) error {
	was, err := wide.RevokeToken(ctx, grant.Principal, grant.ID, now)
	if err != nil || !was {
		return err
	}
	return auditToken(ctx, wide, grant.Principal, audit.Record{
		Actor: installationActor, Action: audit.APITokenRevoke, Target: grant.ID, Result: audit.Done,
		Detail: map[string]any{"principal": grant.Principal, "client_id": grant.ClientID, "audience": grant.Audience, "reason": "refresh_token_reused"},
	})
}

// refuse records a code refused at the token endpoint, as the exchange records one it refuses.
func (s *OAuth) refuse(r *http.Request, f exchangeFailure, now time.Time) {
	refuseCode(r, s.pool, s.signIns, s.trouble, f, authorisationCredential, now)
}
