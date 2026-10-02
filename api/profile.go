package api

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	// The zone database is embedded rather than read from the host alone, so that a time zone a
	// person names is accepted by an API in a distroless image as by one on a full host; a host's
	// own database still answers first where it has one, as time.LoadLocation reads it.
	_ "time/tzdata"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// What a user says of themself: PATCH /api/v1/me, which changes their profile, given and family
// names, title, location, time zone and bio, and answers who they are as GET /api/v1/me does. Their
// display name is made of the two names (db.User.DisplayName) and written by nobody.
//
// Set by the user alone, and by no administrator: a profile is what a person tells the people they
// work with about themself, and one an administrator could rewrite would be a sentence put in their
// mouth. An administrator still gives the given and family names at the creation, so that a user is
// shown by their name from the start, and removes a photo that should not be shown (avatar.go).
//
// The email address is the other way round: an administrator's to give, at the creation and at
// PATCH /api/v1/users/{login} (users.go), and not the user's to change, so this route refuses it.
// Nothing proves an address, and one the installation shows beside a name, for the people who read it
// to write to, comes from whoever answers for its accounts, each change of it audited.
//
// Only a user has a profile. A service account is named by its namespace and its name, which say
// all a grant needs, and the bootstrap token is nobody; both are refused with 403 saying so rather
// than with a 404 that would read as a route missing. A token narrowed by a scope is refused too:
// "a scope can only narrow", it keeps only the permissions it names, and saying who its holder is to
// everybody who reads their name is none of them, so a script handed one cannot rename its owner.

// The longest each field of a profile is, in characters, as $defs/user and the users table hold
// them. A name, a title and a location are a line on a card. A time zone is twice the longest name
// the IANA database holds, so that no zone it may add is refused. A bio is a few sentences, the
// length of a short post, since it is shown beside the name rather than read as a page.
const (
	profileLineMax = 128
	timezoneMax    = 64
	bioMax         = 280
)

// The refusals of a caller who has no profile, each a 403.
const (
	bootstrapHasNoProfile   = "the bootstrap token is nobody and has no profile or photo: its one lasting use is to create the first administrator, who sets their own once signed in"
	accountHasNoProfile     = "a service account has no profile or photo: it is named by its namespace and its name, which are all a grant reads, and a profile says who a person is to the people they work with"
	narrowedWritesNoProfile = "a token narrowed by a scope changes no profile or photo, since a scope keeps only the permissions it names and saying who its holder is to everybody who reads their name is none of them: use a credential that carries no scope"
)

// userWriting answers the login of a caller that writes its own profile or photo, and refuses any
// other with 403 and why: the bootstrap token and a service account, which have neither, and a token
// narrowed by a scope.
func userWriting(w http.ResponseWriter, caller Caller) (string, bool) {
	switch {
	case caller.Principal == BootstrapOperator:
		fail(w, http.StatusForbidden, bootstrapHasNoProfile)
	case strings.Contains(string(caller.Principal), "/"):
		fail(w, http.StatusForbidden, accountHasNoProfile)
	case caller.Narrowed():
		fail(w, http.StatusForbidden, narrowedWritesNoProfile)
	default:
		return string(caller.Principal), true
	}
	return "", false
}

// ProfileChange is what PATCH /api/v1/me changes, openapi.json's profileUpdate: each field it names
// is set to what it holds, the empty string clearing it, and each it leaves out is kept. Every field
// may be cleared: a user who clears both names is shown by their login.
type ProfileChange struct {
	GivenName  *string `json:"given_name,omitempty"`
	FamilyName *string `json:"family_name,omitempty"`
	Title      *string `json:"title,omitempty"`
	Location   *string `json:"location,omitempty"`
	Timezone   *string `json:"timezone,omitempty"`
	Bio        *string `json:"bio,omitempty"`
}

// profileFields are the fields of a profile change, in the order the audit log names those that
// changed.
var profileFields = []string{"given_name", "family_name", "title", "location", "timezone", "bio"}

// emailGiven is the refusal of an email address a user writes of themself.
var emailGiven = errors.New("email: an email address is given by an administrator, at PATCH /api/v1/users/{login}, and a user does not write their own")

// of answers where the field called name is kept, and nil for a name that is none.
func (c *ProfileChange) of(name string) **string {
	switch name {
	case "given_name":
		return &c.GivenName
	case "family_name":
		return &c.FamilyName
	case "title":
		return &c.Title
	case "location":
		return &c.Location
	case "timezone":
		return &c.Timezone
	case "bio":
		return &c.Bio
	}
	return nil
}

// field reads one field. Null is refused rather than read as leaving the field or clearing it,
// either of which a client could have meant: the empty string clears a field, and a field left out
// is kept.
func (c *ProfileChange) field(b *body, name string) error {
	into := c.of(name)
	switch {
	case name == "display_name":
		return displayNameMade
	case name == "email":
		return emailGiven
	case into == nil:
		return unknown(name)
	}
	if b.d.PeekKind() == jsontext.KindNull {
		return fmt.Errorf("%s: null says neither what the field holds nor that it is cleared: the empty string clears a field, and a field left out is kept", name)
	}
	var value string
	if err := text(b, &value); err != nil {
		return err
	}
	*into = &value
	return nil
}

// check refuses a field a profile would not hold, saying which and why.
func (c *ProfileChange) check() error {
	for _, name := range profileFields {
		value := *c.of(name)
		if value == nil {
			continue
		}
		var err error
		switch name {
		case "timezone":
			err = timeZone(*value)
		case "bio":
			err = profileLine(name, *value, bioMax)
		default:
			err = profileLine(name, *value, profileLineMax)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// apply writes the change over what is recorded, and answers the names of the fields it changed,
// in profileFields' order: a field written with what it already holds is no change.
func (c *ProfileChange) apply(p *db.Profile) []string {
	changed := []string{}
	for _, name := range profileFields {
		value := *c.of(name)
		if value == nil {
			continue
		}
		var kept *string
		switch name {
		case "given_name":
			kept = &p.GivenName
		case "family_name":
			kept = &p.FamilyName
		case "title":
			kept = &p.Title
		case "location":
			kept = &p.Location
		case "timezone":
			kept = &p.Timezone
		case "bio":
			kept = &p.Bio
		}
		if *kept != *value {
			*kept = *value
			changed = append(changed, name)
		}
	}
	return changed
}

// profileLine refuses a field of a profile longer than most characters, or holding a control
// character: it is shown in a console and printed at a terminal, the names as the display name they
// make, where a line break forges a line and an escape sequence rewrites what is shown. The empty
// string clears it.
func profileLine(name, value string, most int) error {
	switch n := utf8.RuneCountInString(value); {
	case n > most:
		return fmt.Errorf("%s: it is at most %d characters and this one is %d", name, most, n)
	case strings.ContainsFunc(value, unicode.IsControl):
		return fmt.Errorf("%s: it holds a line break or another control character, and it is one line, shown in a console and printed at a terminal as it is", name)
	}
	return nil
}

// timeZone refuses a time zone the IANA database does not name, which is what a client turns an
// instant into the user's own hours with. Local is refused too: it is whatever zone the API's host
// is set to, which names no place and would read differently on two hosts of one installation.
func timeZone(name string) error {
	switch {
	case name == "":
		return nil
	case utf8.RuneCountInString(name) > timezoneMax:
		return fmt.Errorf("timezone: a time zone is at most %d characters, and the IANA database names none longer", timezoneMax)
	case name == "Local":
		return errors.New("timezone: Local is the zone the server is set to and names no place: name the zone, such as Europe/Paris, or UTC")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("timezone: %.64q is not a time zone of the IANA database, which names one as Europe/Paris or America/Argentina/Buenos_Aires", name)
	}
	return nil
}

// updateProfile is PATCH /api/v1/me: the caller's profile, as the body names it, answered as GET
// /api/v1/me answers who the caller is.
//
// The user's row is held while the change is merged into it, so that two changes at once each keep
// what the other wrote of the fields it left out. The change is recorded as user.profile with the
// names of the fields that changed, and as unchanged where none did.
func (m *MeAPI) updateProfile(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := userWriting(w, caller)
	if !ok {
		return
	}
	var change ProfileChange
	if err := readAtMost(r, &change, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if change == (ProfileChange{}) {
		fail(w, http.StatusBadRequest, "the request names nothing to change: given_name, family_name, title, location, timezone, bio, or any of them")
		return
	}
	if err := change.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	err := m.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.HoldUser(ctx, login)
		if errors.Is(err, db.ErrNoPrincipal) {
			return errGone
		}
		if err != nil {
			return err
		}
		profile := user.Profile
		changed := change.apply(&profile)
		result := audit.Unchanged
		if len(changed) > 0 {
			result = audit.Done
			if err := wide.UpdateProfile(ctx, login, profile); err != nil {
				return err
			}
		}
		return wide.Audit(ctx, audit.Record{
			Actor: login, Action: audit.UserProfile, Target: login, Result: result,
			Detail: map[string]any{"fields": changed},
		})
	})
	switch {
	case errors.Is(err, errGone):
		// Removed since its credential was looked up: the credential opens nothing now.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noToken)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the profile could not be written")
		return
	}
	m.answer(w, r, caller)
}
