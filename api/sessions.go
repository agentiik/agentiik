package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// The console's session: "an opaque session identifier in a cookie: HttpOnly, Secure, SameSite=Lax.
// Revocable server-side, idle expiry. Records the credential that opened it, so that what it may do
// is read at every request from that credential and the policy that applies then, not by
// convention."
//
// OpenSession opens one, for the sign-in routes to call once a passkey or a password has proved who
// is there; Principals.Identify reads it back on every request, beside the bearer token.
//
// A session ends when its holder signs out, POST /api/v1/auth/sign-out, which the sign-in page
// offers; idle, at its lifetime, with its credential, and while its user is suspended. The
// identifier is 256 bits from the operating system's generator, shown in the cookie alone and kept as its SHA-256, as a token is, so that the
// table opens nothing to whoever reads it.

// SessionCookie is the cookie a session travels in, as the OpenAPI document's session scheme names
// it. The __Host- prefix makes a browser refuse the cookie unless it is Secure, set for the whole
// origin and bound to no Domain, so that no other host under the same domain can set one the API
// would read.
const SessionCookie = "__Host-agentiik_session"

// SessionIdle is how long a session lives without a request: twelve hours. The page asks for an
// idle expiry and names none, so this is the reading taken: a working day with its breaks is one
// sign-in, and a browser left overnight is signed out by the morning, which is what somebody who
// walked away from a shared machine is owed.
const SessionIdle = 12 * time.Hour

// SessionLifetime is how long a session lives at most, however often it is used: thirty days. A
// session that never goes idle, a tab left open on a dashboard, would otherwise be a credential for
// good, which the page refuses a token for; so a person proves again who they are at least once a
// month, with the passkey and the policy of that day, and a stolen cookie kept busy lives no longer.
const SessionLifetime = 30 * 24 * time.Hour

// sessionTouch is how far a request has to move a session's idle expiry for the move to be
// written: a minute. A log stream asks who its caller is again as it goes, and a page asks several
// things at once; writing the session's row for each would be a write per read, where an expiry
// left a minute short of where it could be ends a session a minute early at most.
const sessionTouch = time.Minute

// sessionBytes is how much of the operating system's generator a session identifier is: 256 bits,
// what every credential of the installation carries, written in the 43 base64url characters the
// OpenAPI document's example shows.
const sessionBytes = 32

// The sentences a session refused is answered with.
const (
	// noSession is a session that opens nothing, one sentence for every reason, as noToken is.
	noSession = "that session opens nothing: it is no session this installation opened, or it was revoked, has ended or was left idle too long, or its holder is suspended. Sign in again, or open a fresh enrolment link"

	// crossOrigin is a request changing something that a session carries from another origin.
	// SameSite=Lax keeps another site's page from carrying the cookie on a POST, a PUT or a
	// DELETE, but not a page of another host of the same site, which a browser counts as the same
	// site; the Origin header it sends names the page, so a request is accepted from the public
	// URL's pages alone.
	crossOrigin = "a session changes something only from the pages of this installation's public URL, and this request's Origin header names another or none"

	// spentElsewhere is a read that would spend an artifact's fetch, carried with a session from
	// anywhere but the public URL's pages, or from a browser that does not say where it comes
	// from. SameSite=Lax lets a link followed from another site carry the cookie, and an image
	// on another host of the same site, so that whoever follows or loads one would spend a fetch
	// whose bytes go to them, until the budget is gone for everybody. A browser that sends neither
	// Sec-Fetch-Site nor Origin on a GET cannot be told from such a page, and is refused it too,
	// since the budget is what is at stake; a token, which no page carries, spends as it did.
	spentElsewhere = "a session spends an artifact's fetch only from this installation's pages, and this request comes from another site, or does not say where it comes from, as a browser that sends no Sec-Fetch-Site header does: download it from the console, or with an API token"

	// enrolsOnly is a session opened to enrol a passkey, anywhere else, the OpenAPI document's
	// sentence for it.
	enrolsOnly = "this session enrols passkeys and nothing else"

	// oneCredential is a request carrying more than one credential. Which of two to believe is
	// not something the API guesses: a request is answered as one principal, and one carrying a
	// bearer token beside a browser's session, or two sessions, is a client confused about which
	// it means.
	oneCredential = "this request carries more than one credential, a bearer token beside a session cookie or two session cookies, and a request is answered as one principal: send the one meant"

	// tokenSignsNothingOut is a sign-out presenting a bearer token, which is no session.
	tokenSignsNothingOut = "a sign-out ends the session a browser's cookie carries, and this request carries a bearer token: a token is revoked with DELETE /api/v1/auth/tokens/{id}"
)

// OpenedBy is what opens a session: a credential of its user, named by its identifier, which opens
// a session reaching what the user's grants allow. An enrolment code opens none: the passkey
// ceremonies take the code in the registration's options and spend it when the passkey is recorded,
// which signs its user in with that passkey, and a password set from a code signs them in with the
// password.
//
// A session a password opened may only enrol, wherever the policy that applies to its account
// requires a passkey, whatever passkeys the account holds, where the OpenAPI document's
// sessionKind says so of an account holding none yet: the passkey that brings the account to
// min_passkeys takes the password, and a password found beside them opens no more (policy.go). That
// is read from the credential the session records, at every request, and never written: see
// identifySession.
type OpenedBy struct {
	Credential string
}

// OpenSession opens a session of login at now, in the transaction w is, and answers the cookie that
// carries it, for the answer that opened it to set once that transaction has committed: a cookie
// set before would name a session that may never have been written.
//
// It is the one way a session is opened, so the value is minted here, shown in this cookie alone,
// and the cookie's attributes are written in one place: HttpOnly, so that no script of the page
// reads it; Secure and SameSite=Lax; Path=/ and no Domain, which the __Host- prefix requires. It
// carries no expiry, as the OpenAPI document's example carries none, so that a browser closed ends
// it too: the server ends it after SessionIdle without a request and SessionLifetime at most.
//
// A suspended user opens none, which is db.ErrSessionRefused.
func OpenSession(ctx context.Context, w *db.Wide, login string, by OpenedBy, now time.Time) (*http.Cookie, error) {
	if by.Credential == "" {
		return nil, errors.New("api: a session is opened by a credential of its user, and none was named")
	}
	raw := make([]byte, sessionBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("api: a session could not be opened: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(value))
	err := w.OpenSession(ctx, db.Session{
		Hash: hash[:], Login: login, Credential: by.Credential,
		CreatedAt: now, IdleExpiresAt: now.Add(SessionIdle),
	})
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name: SessionCookie, Value: value,
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}, nil
}

// AcceptSessions has Identify read the console's session cookie beside the bearer token, for an
// installation whose public URL is publicURL: AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy. A
// request changing something that a session carries is accepted from that URL's origin alone.
// Without it a session cookie is not read, and a request carrying one carries no credential.
//
// It is called once, before the router serves anything.
func (p *Principals) AcceptSessions(publicURL string) error {
	origin, err := originOf(publicURL)
	if err != nil {
		return err
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return err
	}
	p.origin, p.ipAddressed = origin, net.ParseIP(u.Hostname()) != nil
	return nil
}

// sessionsOf is the session identifiers a request carries, none where sessions are not accepted.
// Every one of the name, so that a request carrying two is refused rather than answered as
// whichever a parser read first; an empty one is none, as an empty bearer token is.
func (p *Principals) sessionsOf(r *http.Request) []string {
	if p.origin == "" {
		return nil
	}
	var values []string
	for _, c := range r.CookiesNamed(SessionCookie) {
		if c.Value != "" {
			values = append(values, c.Value)
		}
	}
	return values
}

// identifySession is who the session a request carries belongs to, if it is live, and keeps it
// open: its idle expiry moves to SessionIdle from now, and never past SessionLifetime from its
// opening, so that the idle expiry the table holds is the one end the lookup has to ask about.
//
// A request changing something is refused before the session is looked up where it does not come
// from the public URL's origin, so that a page of another host neither acts on the session nor
// keeps it open. A safe one is answered from anywhere, and marked Elsewhere where it does not come
// from those pages, so that it spends nothing a read spends (fromPages).
//
// What a session a password opened may do is read from the policy that applies to its account now,
// so that a policy changed applies from the next request, as do passkeys enrolled from the session:
// nothing where passwords are forbidden, since the policy says no password exists any more and
// whatever one opened goes with it; enrolling alone where a passkey is required, which the
// account's passkeys, not its password, are the way past; and whatever the user's grants allow
// otherwise. Such a session is enrolling, and the registration ceremony registers from it. A session
// a synced passkey opened opens nothing where device_bound_only applies, read the same way.
//
// The first request of an administrator's full session ends the bootstrap token where it has not
// ended, recorded as bootstrap.end: a session a password opened while the policy required a passkey
// only enrolled and left the token going, and once the policy relaxes it is full from its next
// request, from which its holder administers. Ended at no sign-in or enrolment of theirs, the token
// would go on beside the administrator it made for as long as that session did. It ends in a
// transaction after the request's, taking its locks as every act ending the token does
// (endBootstrapAtSession).
func (p *Principals) identifySession(r *http.Request, value string) (Identity, error) {
	if !safe(r.Method) {
		if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != p.origin {
			return Identity{Refused: crossOrigin, RefusedAs: http.StatusForbidden}, nil
		}
	}
	elsewhere := safe(r.Method) && !p.fromPages(r)
	hash := sha256.Sum256([]byte(value))
	now := p.now()
	as := Identity{Refused: noSession}
	ending := false
	err := p.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		s, err := w.SessionByHash(ctx, hash[:], now)
		if errors.Is(err, db.ErrNoSession) {
			return nil
		}
		if err != nil {
			return err
		}
		opens, enrolling, err := p.sessionOpens(ctx, w, s, now)
		if err != nil || !opens {
			return err
		}
		ends := s.CreatedAt.Add(SessionLifetime)
		until := now.Add(SessionIdle)
		if until.After(ends) {
			until = ends
		}
		if until.Sub(s.IdleExpiresAt) >= sessionTouch {
			err := w.TouchSession(ctx, hash[:], now, until)
			if errors.Is(err, db.ErrNoSession) {
				// Revoked since it was read.
				return nil
			}
			if err != nil {
				return err
			}
		}
		if s.Admin && !enrolling {
			b, err := w.Bootstrap(ctx)
			if err != nil {
				return err
			}
			ending = !b.Ended()
		}
		as = Identity{
			Principal: Principal(s.Login), Enrolling: enrolling, ProvedAt: s.CreatedAt, OpenedBy: s.Credential,
			Elsewhere: elsewhere,
		}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	if ending {
		if err := p.endBootstrapAtSession(r.Context(), string(as.Principal), hash[:], now); err != nil {
			return Identity{}, err
		}
	}
	return as, nil
}

// fromPages says whether a request comes from the pages of the public URL, or from the address bar
// or a bookmark, as the browser that sent it says: Sec-Fetch-Site same-origin or none, where it sends
// one, since a browser writes it and no page can; and where it sends none, an Origin header that is
// the public URL's origin. A browser sends no Origin on a GET from its own origin, so one sending
// neither header says nothing of where it comes from, and is not taken to come from these pages.
// More than one value of either is a client confused about where it is, and is not either.
func (p *Principals) fromPages(r *http.Request) bool {
	if sites := r.Header.Values("Sec-Fetch-Site"); len(sites) > 0 {
		return len(sites) == 1 && (sites[0] == "same-origin" || sites[0] == "none")
	}
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == p.origin
}

// sessionOpens says whether s opens anything at now, and whether it may only enrol, read from the
// credential that opened it and the policy that applies to its account now: see identifySession.
func (p *Principals) sessionOpens(ctx context.Context, w *db.Wide, s db.Session, now time.Time) (opens, enrolling bool, err error) {
	switch {
	case s.CredentialType == db.CredentialPassword:
		policy, err := policyFor(ctx, w, s.Login, now, p.ipAddressed)
		if err != nil || policy.passwordsForbidden {
			return false, false, err
		}
		enrolling = policy.enrolling()
	case s.CredentialType == db.CredentialPasskey && s.BackupEligible:
		// A synced passkey signs nobody in where device_bound_only applies, and what it opened
		// before the policy came to say so goes with it; a device-bound one's sessions need no
		// policy read.
		policy, err := policyFor(ctx, w, s.Login, now, p.ipAddressed)
		if err != nil || policy.deviceBoundOnly {
			return false, false, err
		}
	}
	return true, enrolling, nil
}

// endBootstrapAtSession ends the bootstrap token at a request of login's session, the one whose
// identifier hashes to hash, where the request found it an administrator's full session and the
// token live, and records the end.
//
// In a transaction of its own, after the request's, and in the order every act ending the token
// takes its locks: the user's row first (HoldUser), then the bootstrap state, then the audit log. What
// the request read is read again under the user's row, since what it read was held by nothing: an
// administrator removed, suspended, or given a role where their way in is refused, at the same
// moment, is seen here or waits, and never ends the token beside the lockout it would leave. The
// session's row, which the request may have kept open, is not held here, so that a removal holding
// the bootstrap state and waiting on the session through its user never waits on this. Two first
// requests at the same moment take turns on the user's row, and the second ends nothing.
func (p *Principals) endBootstrapAtSession(ctx context.Context, login string, hash []byte, now time.Time) error {
	return p.pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		user, err := w.HoldUser(ctx, login)
		if errors.Is(err, db.ErrNoPrincipal) {
			return nil
		}
		if err != nil || !user.Admin || user.Suspended {
			return err
		}
		s, err := w.SessionByHash(ctx, hash, now)
		if errors.Is(err, db.ErrNoSession) || (err == nil && s.Login != login) {
			return nil
		}
		if err != nil {
			return err
		}
		opens, enrolling, err := p.sessionOpens(ctx, w, s, now)
		if err != nil || !opens || enrolling {
			return err
		}
		ended, err := w.EndBootstrap(ctx, now)
		if err != nil || !ended {
			return err
		}
		return w.Audit(ctx, audit.Record{
			Actor: login, Action: audit.BootstrapEnd, Target: string(BootstrapOperator), Result: audit.Done,
			Detail: map[string]any{"first_administrator": login, "credential": s.Credential},
		})
	})
}

// proofLife is how recently a session has to have been signed in to for a credential that lasts to
// be added from it: a first password, a TOTP generator, a passkey registered from the session, an
// API token minted from it. Ten minutes, the time the page's steps take from a sign-in, and short
// enough that a session left open on a shared machine, or a cookie carried off, is not enough to
// give whoever holds it a way in of their own that outlives the session. A sign-in again opens a
// session of its own, which proves possession anew: see Identity.ProvedAt. A password changed
// proves the one it replaces instead, which is sent beside it.
const proofLife = 10 * time.Minute

// signInAgain is a credential that lasts, asked for from a session signed in to longer ago than
// proofLife.
const signInAgain = "adding a way in takes a sign-in in the last 10 minutes, so that a session left open is not enough to add one, and this session was signed in to earlier: sign in again, then try once more"

// provedSince says whether a session proved at provedAt proved possession within proofLife of at:
// never for a request carrying no session, whose proof is the zero time.
func provedSince(provedAt, at time.Time) bool {
	return !provedAt.IsZero() && at.Sub(provedAt) <= proofLife
}

// askAgain answers a credential that lasts asked for without a recent enough proof: 403 with the
// sentence saying so, and the challenge of RFC 9470, "insufficient_user_authentication" with the
// max_age a proof may have, for the page to tell this refusal from the others by, rather than by
// matching the sentence.
func askAgain(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_user_authentication", max_age="%d"`, int(proofLife/time.Second)))
	fail(w, http.StatusForbidden, signInAgain)
}

// endSession ends the session a request carries, as a sign-out asks at, and answers the status and
// the sentence it is refused with, a zero status where it is not.
//
// It carries one session and nothing else: a bearer token is no session, and two credentials are
// refused as they are everywhere. It comes from the public URL's origin, whatever it carries, since
// ending a session changes something: a page of another host of the same site could otherwise sign
// a browser out. A session that opens nothing now, idle, revoked or its user's suspended, is
// ended all the same, and a request carrying none ends nothing: either way the browser is signed
// out, which is what was asked, and its cookie is cleared.
func (p *Principals) endSession(r *http.Request, now time.Time) (int, string, error) {
	// What a request carries is refused first, since refusing it changes nothing, so that a script
	// presenting a token, with no Origin header, is told how a token is revoked.
	_, bearer := bearerOf(r)
	values := p.sessionsOf(r)
	switch {
	case (bearer && len(values) > 0) || len(values) > 1:
		return http.StatusBadRequest, oneCredential, nil
	case bearer:
		return http.StatusBadRequest, tokenSignsNothingOut, nil
	}
	if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != p.origin {
		return http.StatusForbidden, crossOrigin, nil
	}
	if len(values) == 0 {
		return 0, "", nil
	}
	hash := sha256.Sum256([]byte(values[0]))
	err := p.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndSession(ctx, hash[:], now)
		return err
	})
	return 0, "", err
}

// safe says whether a method only reads, which a request of another origin may carry a session on:
// SameSite=Lax lets a link followed from another site carry the cookie, and what it reads is not
// the other site's to see, since the API answers no cross-origin read.
func safe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// originOf is the origin of a public URL's pages as a browser writes it in an Origin header: https,
// the host in lower case, and the port only where it is not 443, with no path, since the API may be
// served under one and an origin carries none.
//
// A host outside ASCII is compared as it is written, where a browser writes its punycode, and the
// standard library converts neither way: an installation on such a name writes its public URL in
// punycode, or its sessions change nothing, which refuses rather than admits.
//
// The URL is never repeated in the refusal, since a URL can carry a password.
func originOf(publicURL string) (string, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("api: sessions are accepted on the pages of an https URL with a host, and the public URL given is not one")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", errors.New("api: sessions are accepted on the pages of an https URL with a host, and the public URL given names a port no origin can")
		}
		if n != 443 {
			host += ":" + strconv.FormatUint(n, 10)
		}
	}
	return "https://" + host, nil
}
