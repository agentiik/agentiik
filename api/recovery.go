package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
)

// Recovery codes: "issued by an administrator. Single use, good for an hour", "audited with the
// issuing administrator and the account it was issued for". POST /api/v1/users/{login}/recovery
// issues one, and BreakGlass, which agentiik-api recover runs on the installation's host, issues
// one for an administrator on the day no administrator can sign in to issue it.
//
// A recovery code is an enrolment code of the kind recovery. The enrolment page takes it typed, or
// in the link that carries it after its #, and it enrols a passkey, or sets a password where the
// policy that applies to the account allows passwords, in place of the one held, as an enrolment
// link does: every kind of code may set a password, since on an installation addressed by an IP
// address a password is the one way back. A fresh one revokes the user's open recovery code. Where
// the account is an administrator's who is not suspended and the bootstrap token has not ended, the
// enrolment it makes ends it, as the first administrator's link does, a password's where the session
// it opens is a full one (passkeys.go, passwords_set.go).
//
// It is issued whatever the user holds, a credential or none: what they lost may be all they had,
// and a user who never enrolled is given one as surely as a fresh link, which the administrator may
// not know to ask for instead.
//
// It is sent nowhere: it is answered once, to whoever issued it, who hands it over. A recovery link
// by mail "would put the account back behind a mailbox and forfeit the passkey's phishing
// resistance", and nothing in this module sends mail, which mail_test.go holds.
//
// An administrator issues none for themselves. A code is audited with two identities because two
// people are meant to be behind it, one who lost what signs them in and one who vouches for them.
// Issued to oneself, it would let a token or a session somebody took from an administrator give
// that administrator's own account a credential that outlives the theft, quietly, with nobody else
// vouching for it, as a bearer token may not set a credential from /me either. An administrator who
// lost theirs asks another, and where nobody is left who can sign in, whoever holds the
// installation's settings runs agentiik-api recover on its host.
//
// The bootstrap token issues them as it administers everything else, until the first administrator
// has enrolled, and those it issued open nothing once it has ended (db.Wide.EnrolmentCodeByHash).
// And once a recovery code has enrolled its user, the link they were created with, if still open,
// opens nothing either: a link enrols the first credential of an account that holds none.

// RecoveryCode is a recovery code, shown once: openapi.json's recoveryCode, the code to read out or
// type, and the link to the enrolment page that carries it.
type RecoveryCode struct {
	Code      string    `json:"code"`
	Link      string    `json:"link"`
	ExpiresAt time.Time `json:"expires_at"`
}

// The refusals of a recovery code's issue.
const (
	// SelfRecovery is an administrator asking a recovery code for their own account. agk reads
	// it to say it as it is, rather than as a caller who does not administer.
	SelfRecovery = "you may not issue yourself a recovery code, which another administrator issues and the audit log records with both, so that every recovery has somebody vouching for it and a token or a session somebody took from you cannot give your own account a credential that outlives it: ask another administrator, or, where none can sign in, run agentiik-api recover on the installation's host"

	// serviceAccountRecovery is a recovery code asked for a service account, which holds nothing
	// a code replaces.
	serviceAccountRecovery = "a service account is no user, and holds API tokens alone, which whoever owns its namespace mints again: nothing a recovery code replaces"
)

// ErrNotAdministrator is the break-glass path asked for a user who is not an administrator.
var ErrNotAdministrator = errors.New("api: that user is not an administrator")

// issuedCode is an enrolment code issued: its value, shown once, the link that carries it, and when
// it lapses.
type issuedCode struct {
	value   string
	link    string
	expires time.Time
}

// issueCode issues login an enrolment code of kind, as issuer, revoking at now the open one it
// replaces, and answers it with the entry that records it, for its caller to append once every row
// it locks is locked: with who issued it and for whom, and never with its value, which is shown once,
// in the answer. enrol is the enrolment page's address up to its #.
func issueCode(ctx context.Context, wide *db.Wide, enrol, issuer, login, kind string, now time.Time) (issuedCode, audit.Record, error) {
	value, _, err := token.New(token.Enrol, "")
	if err != nil {
		return issuedCode{}, audit.Record{}, err
	}
	hash := sha256.Sum256([]byte(value))
	expires := now.Add(EnrolmentLife)
	replaced, err := wide.IssueEnrolmentCode(ctx, db.EnrolmentCode{
		Hash: hash[:], Login: login, Kind: kind, IssuedBy: issuer, IssuedAt: now, ExpiresAt: expires,
	})
	if err != nil {
		return issuedCode{}, audit.Record{}, err
	}
	issued := audit.Record{
		Actor: issuer, Action: audit.EnrolmentIssue, Target: login, Result: audit.Done,
		Detail: map[string]any{"kind": kind, "expires_at": expires.UTC().Format(time.RFC3339Nano), "replaced": replaced},
	}
	return issuedCode{value: value, link: enrol + value, expires: expires.UTC()}, issued, nil
}

// enrolAt is the enrolment page's address on publicURL, up to the # a code follows.
func enrolAt(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + enrolPage
}

// issueRecovery is POST /api/v1/users/{login}/recovery: a recovery code for somebody who lost what
// signs them in, revoking the one issued before it.
func (s *UserAPI) issueRecovery(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	login := r.PathValue("login")
	switch {
	case strings.Contains(login, "/"):
		// NS/NAME, a service account's reference, escaped into one segment of the path.
		fail(w, http.StatusNotFound, serviceAccountRecovery)
		return
	case LoginRef(login) != nil:
		fail(w, http.StatusNotFound, noUser)
		return
	case Principal(login) == who:
		fail(w, http.StatusForbidden, SelfRecovery)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	var code issuedCode
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var issued audit.Record
		var err error
		if code, issued, err = issueCode(ctx, wide, s.enrol, string(who), login, db.EnrolmentRecovery, now); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		return wide.Audit(ctx, issued)
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noUser)
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the recovery code could not be issued")
	default:
		shownOnce(w, http.StatusCreated, RecoveryCode{Code: code.value, Link: code.link, ExpiresAt: code.expires})
	}
}

// BreakGlass issues a recovery code for the administrator login, as the installation itself, and
// answers it: agentiik-api recover, "for the day every administrator loses their authenticator",
// which the page calls the break-glass line of recovery, "documented and held offline".
//
// It takes no credential. It is run where the API runs, with the installation's database settings,
// which is what holds it offline: out of reach of anybody the network brings, and in reach of
// whoever holds the host, who could write the code's row by hand anyway, unaudited. It is recorded
// as enrolment.issue by installation, as the namespace verbs' acts are, since no principal a grant
// names acts; the link it answers points at publicURL, the enrolment page's origin.
//
// It recovers an administrator and nobody else: once one can sign in again, a user's recovery code
// is theirs to issue, audited with both identities. A suspended administrator is issued one all the
// same, since enrolling is how an account suspended for having no passkey comes back, and so is one
// holding no credential at all, so that no state an administrator's account can be left in keeps
// the installation locked. It is ErrNotAdministrator for a user who is not one, and
// db.ErrNoPrincipal for a login no user has.
//
// Every administrator is told, the one recovered included, with a break_glass_recovery notification
// in their GET /api/v1/me written in the same transaction, and the entry names who was told: no
// administrator vouches for this code, so an administrator who did not run it learns that whoever
// holds the host did.
func BreakGlass(ctx context.Context, pool *db.Pool, publicURL, login string, now time.Time) (RecoveryCode, error) {
	if err := LoginRef(login); err != nil {
		return RecoveryCode{}, err
	}
	// To the microsecond the database keeps, so that the expiry answered is the one stored.
	now = now.UTC().Truncate(time.Microsecond)
	var code issuedCode
	err := pool.Installation(ctx, db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.User(ctx, login)
		if err != nil {
			return err
		}
		if !user.Admin {
			return fmt.Errorf("%w: %s", ErrNotAdministrator, login)
		}
		var issued audit.Record
		if code, issued, err = issueCode(ctx, wide, enrolAt(publicURL), installationActor, login, db.EnrolmentRecovery, now); err != nil {
			return err
		}
		told, err := wide.TellAdministrators(ctx, login, now)
		if err != nil {
			return err
		}
		issued.Detail["notified"] = told
		return wide.Audit(ctx, issued)
	})
	if err != nil {
		return RecoveryCode{}, err
	}
	return RecoveryCode{Code: code.value, Link: code.link, ExpiresAt: code.expires}, nil
}
