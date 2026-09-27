package api

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The administrator's routes for people: the users of an installation, the enrolment link a user
// with no credential enrols their first passkey with, and the groups users are put in.
//
// "A platform administrator manages users, groups, namespaces, quotas, runners, runner policies and
// the authentication policy", so every route here requires grant:manage at installation scope,
// which an administrator holds, and the bootstrap operator until the first administrator has
// enrolled: the bootstrap token is how the first administrator is created at all.
//
// A group's membership is a row of its own and touches no grant. Who holds what is resolved from
// the memberships at every request, so a member added gains what the group's grants give from their
// next request, and one removed loses it the same way.
//
// Each act is recorded in the audit log in its own transaction, as every act is: a user created or
// removed, an enrolment link issued, a group created or removed, a member added or removed, since
// each changes who may reach what.
//
// Each transaction takes its rows' locks first, the bootstrap state's next, and appends to the audit
// log last. Appending locks the head of the chain, one row for the whole installation, until the
// transaction ends, so an act that appended and then waited on a row another act holds, while that
// act waits on the head to append its own entry, is a deadlock: two administrators, one asking a
// fresh link for alice and the other putting her in a group, would have had one of them refused.

// EnrolmentLife is how long an enrolment link opens anything: "single use and good for an hour",
// long enough to send it to its user and short enough that one forgotten in a chat is dead by the
// time anybody finds it.
const EnrolmentLife = time.Hour

// enrolPage is where an enrolment link points on the public URL: the API's own page, which runs the
// passkey ceremony on the Relying Party's origin.
const enrolPage = "/auth/enrol#"

// displayNameMax is the longest display name, in characters, as $defs/user holds it.
const displayNameMax = 256

// installationActor is how the installation itself is written as the author of its own rows, the
// pool default for one. It is refused as a login, as operator is, so that no user reads as it.
const installationActor = "installation"

// UserOptions are what the user and group routes are given.
type UserOptions struct {
	Pool *db.Pool

	// PublicURL is the address the API is reached at, with no slash at its end, which every
	// enrolment link points at: the enrolment page runs the passkey ceremony, and a browser
	// runs one only on the origin of the Relying Party, the host of this address.
	PublicURL string

	// Now is the clock links are issued by, the wall clock where it is nil.
	Now func() time.Time
}

// UserAPI is the user and group routes.
type UserAPI struct {
	pool  *db.Pool
	enrol string
	now   func() time.Time

	// ipAddressed is an installation whose public URL names an IP address, where no passkey signs
	// anybody in, which whether an administrator can sign in depends on.
	ipAddressed bool
}

// NewUsers registers the user and group routes on a router.
func NewUsers(rt *Router, o UserOptions) (*UserAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a user is a row")
	case o.PublicURL == "":
		return nil, errors.New("api: no public URL, and an enrolment link is an address on it")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &UserAPI{pool: o.Pool, enrol: enrolAt(o.PublicURL), now: o.Now}
	if u, err := url.Parse(o.PublicURL); err == nil {
		s.ipAddressed = net.ParseIP(u.Hostname()) != nil
	}

	admin := Needs{Permission: GrantManage, Scope: Installation}
	for _, r := range []struct {
		method  string
		pattern string
		handler Handler
	}{
		{"POST", "/api/v1/users", s.createUser},
		{"GET", "/api/v1/users", s.users},
		{"GET", "/api/v1/users/{login}", s.user},
		{"DELETE", "/api/v1/users/{login}", s.removeUser},
		{"POST", "/api/v1/users/{login}/enrolment", s.issueEnrolment},
		{"POST", "/api/v1/users/{login}/recovery", s.issueRecovery},
		{"POST", "/api/v1/groups", s.createGroup},
		{"GET", "/api/v1/groups", s.groups},
		{"GET", "/api/v1/groups/{group}", s.group},
		{"DELETE", "/api/v1/groups/{group}", s.removeGroup},
		{"PUT", "/api/v1/groups/{group}/members/{login}", s.addMember},
		{"DELETE", "/api/v1/groups/{group}/members/{login}", s.removeMember},
	} {
		if err := rt.Handle(r.method, r.pattern, admin, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// LoginName refuses a login no user can be created under: one outside $defs/login, which is the
// grammar and the reserved words a namespace is named with, since a login is also the name of its
// user's personal namespace, and operator and installation, which name the authors of rows the
// installation holds, so that a user of either name would read as that author.
func LoginName(login string) error { return loginName(login, agk.IsReservedNamespace) }

// LoginRef refuses a login no user can hold, which a route naming one answers as it answers a user
// who does not exist: LoginName's refusals, save a word reserved late, as NamespaceRef reads one.
func LoginRef(login string) error { return loginName(login, agk.NamesNoNamespace) }

func loginName(login string, reserved func(string) bool) error {
	switch {
	case login == "":
		return errors.New("login: a user has a login, lowercase words joined by hyphens, such as alice or bob-martin")
	case len(login) > agk.IdentifierMaxBytes:
		return fmt.Errorf("login: a login is at most %d characters and this one is %d, since it is also the name of its user's personal namespace", agk.IdentifierMaxBytes, len(login))
	case !givenName.MatchString(login):
		return fmt.Errorf("login: %.64q is not a login: a login is lowercase words joined by hyphens, such as alice or bob-martin, the grammar a namespace is named in, since it is also the name of its user's personal namespace", login)
	case reserved(login):
		return fmt.Errorf("login: %s is reserved: it is %s, and a login is also the name of a namespace", login, routesOn(login))
	case login == string(BootstrapOperator) || login == installationActor:
		return fmt.Errorf("login: %s is reserved: it names the author of rows the installation holds, and a user of that name would read as that author", login)
	}
	return nil
}

// GroupName refuses a name no group can be created under: one outside the grammar and the reserved
// words a namespace is named with, which $defs/group holds a group's name to "so that every name a
// person gives on an installation is written one way".
func GroupName(name string) error { return groupName(name, agk.IsReservedNamespace) }

// groupRef refuses a name no group can carry: GroupName's refusals, save a word reserved late, as
// NamespaceRef reads one.
func groupRef(name string) error { return groupName(name, agk.NamesNoNamespace) }

func groupName(name string, reserved func(string) bool) error {
	switch {
	case name == "":
		return errors.New("name: a group has a name, lowercase words joined by hyphens, such as team-finance")
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("name: a group's name is at most %d characters and this one is %d", agk.IdentifierMaxBytes, len(name))
	case !givenName.MatchString(name):
		return fmt.Errorf("name: %.64q is not a group's name: a group is named in lowercase words joined by hyphens, such as team-finance, as a namespace is", name)
	case reserved(name):
		return fmt.Errorf("name: %s is reserved: it is %s, which names no namespace and so no group", name, routesOn(name))
	}
	return nil
}

// displayName refuses a display name $defs/user would refuse, and one that holds a control
// character: it is shown in a console and printed at a terminal, where a line break forges a line
// and an escape sequence rewrites what is shown.
func displayName(name string) error {
	switch n := utf8.RuneCountInString(name); {
	case name == "":
		return errors.New("display_name: a user has a display name, the name people read in the console and in the sharing panel, such as Alice Martin")
	case n > displayNameMax:
		return fmt.Errorf("display_name: a display name is at most %d characters and this one is %d", displayNameMax, n)
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("display_name: it holds a line break or another control character, and it is one line, shown in a console and printed at a terminal as it is")
	}
	return nil
}

// NewUser is a user to create, as agk user create sends it: openapi.json's userCreate. agk sends
// only what it was given, and the display name and admin a request leaves out are the login and
// false for a user created, and what is recorded for one asked for again.
type NewUser struct {
	Login       string `json:"login"`
	DisplayName string `json:"display_name,omitempty"`
	Admin       bool   `json:"admin,omitempty"`

	// namesOne and saysAdmin are whether the request wrote a display name and admin, null being
	// neither: a user asked for again for a fresh link is refused only for what the request
	// says otherwise than was recorded, so that agk user create LOGIN, run again with nothing
	// more, answers the fresh link it is run again for.
	namesOne, saysAdmin bool
}

func (u *NewUser) field(b *body, name string) error {
	switch name {
	case "login":
		return text(b, &u.Login)
	case "display_name":
		u.namesOne = b.d.PeekKind() != jsontext.KindNull
		return text(b, &u.DisplayName)
	case "admin":
		u.saysAdmin = b.d.PeekKind() != jsontext.KindNull
		return flag(b, &u.Admin)
	}
	return unknown(name)
}

// otherwise says whether the request says something of existing other than was recorded.
func (u NewUser) otherwise(existing db.User) bool {
	return (u.namesOne && u.DisplayName != existing.DisplayName) || (u.saysAdmin && u.Admin != existing.Admin)
}

// User is a user as the routes answer one, $defs/user: never a credential.
type User struct {
	Kind        string `json:"kind"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Admin       bool   `json:"admin"`
	Suspended   bool   `json:"suspended"`

	// SuspendedFor is why the authentication policy suspended the account, no_passkey, and absent
	// for a suspension it did not make: an administrator reading it knows that an enrolment link
	// or a recovery code brings the account back, which lifts that suspension and no other.
	SuspendedFor string `json:"suspended_for,omitempty"`

	CreatedAt    time.Time `json:"created_at"`
	LastSignInAt time.Time `json:"last_sign_in_at,omitzero"`
}

func userOf(u db.User) User {
	answered := User{
		Kind: db.KindUser, Login: u.Login, DisplayName: u.DisplayName, Admin: u.Admin, Suspended: u.Suspended,
		SuspendedFor: u.SuspendedFor, CreatedAt: u.CreatedAt.UTC(),
	}
	if !u.LastSignInAt.IsZero() {
		answered.LastSignInAt = u.LastSignInAt.UTC()
	}
	return answered
}

// EnrolmentLink is a link that enrols the first passkey of an account, shown once: openapi.json's
// enrolmentLink.
type EnrolmentLink struct {
	Link      string    `json:"link"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreatedUser is a user created, or asked for again before enrolling, and the link that enrols
// their first passkey: openapi.json's createdUser.
type CreatedUser struct {
	User      User          `json:"user"`
	Enrolment EnrolmentLink `json:"enrolment"`
}

// The refusals of a user that exists, each a 409.
var (
	// errOtherwise is a user asked for again with another display name or admin, which is not
	// the same request made again but a second user under a login already taken.
	errOtherwise = errors.New("api: that login is a user created otherwise")
	// errLastAdministrator is the removal of the last administrator who can sign in.
	errLastAdministrator = errors.New("api: that is the last administrator")
)

// ownsNamespaces is a principal refused removal while namespaces' records name it as owner.
type ownsNamespaces struct {
	principal  string
	namespaces []string
}

func (o *ownsNamespaces) Error() string {
	return fmt.Sprintf("%s owns namespace %s, and is not removed while a namespace's record names them as owner, so that none is left owned by somebody who is gone: give it another owner first", o.principal, strings.Join(o.namespaces, ", namespace "))
}

// stillBootstrapping refuses the bootstrap operator once the first administrator has enrolled,
// asked again in the transaction of the act, after its rows are locked and before it is recorded:
// the router authorised the request in a transaction of its own, and an enrolment that ended the
// token since then ends what the token may do here too. The bootstrap state is held until the act
// commits, so that the enrolment ending it waits for the act rather than landing in between.
func stillBootstrapping(ctx context.Context, wide *db.Wide, who Principal) error {
	if who != BootstrapOperator {
		return nil
	}
	b, err := wide.BootstrapHeld(ctx)
	if err != nil {
		return err
	}
	if b.Ended() {
		return db.ErrBootstrapEnded
	}
	return nil
}

// createUser is POST /api/v1/users: a user, and the link that enrols their first passkey.
//
// Asked again for a user who has not enrolled, with the same login, and the display name and admin
// as they were created or left out, it answers a fresh link and revokes the one before, with 200
// rather than 201, so that a link that lapsed unused locks nobody out: "run again for the same
// login before it has enrolled, it answers a fresh link and revokes the one before". The bootstrap
// token relies on this until the first administrator has enrolled.
func (s *UserAPI) createUser(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask NewUser
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := LoginName(ask.Login); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if ask.namesOne {
		if err := displayName(ask.DisplayName); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// To the microsecond the database keeps, so that the expiry answered is the one stored.
	now := s.now().Truncate(time.Microsecond)
	var answer CreatedUser
	var created bool
	var err error
	// Twice at most: two requests creating one new login at once each find it missing, and the
	// one that loses the race to insert it is asked again, when it finds the user the other
	// created and answers as a repeat of it.
	for attempt := 0; attempt < 2; attempt++ {
		answer, created, err = s.create(r.Context(), who, ask, now)
		if !errors.Is(err, db.ErrPrincipalExists) {
			break
		}
	}
	switch {
	case errors.Is(err, db.ErrNameTaken):
		fail(w, http.StatusConflict, fmt.Sprintf("%s is already a namespace, and logins and namespaces share one name space, since a user's personal namespace is named after their login", ask.Login))
	case errors.Is(err, errOtherwise):
		fail(w, http.StatusConflict, fmt.Sprintf("%s is a user created with another display name or admin: a user is asked for again as they were created, or with neither, for a fresh link while they have not enrolled", ask.Login))
	case errors.Is(err, db.ErrEnrolled):
		fail(w, http.StatusConflict, fmt.Sprintf("%s is a user who has enrolled already, and an enrolment link enrols the first passkey of an account that holds none", ask.Login))
	case errors.Is(err, db.ErrPrincipalExists):
		fail(w, http.StatusConflict, fmt.Sprintf("%s was created by another request at the same moment: ask again for a fresh link", ask.Login))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the user could not be created")
	case created:
		shownOnce(w, http.StatusCreated, answer)
	default:
		shownOnce(w, http.StatusOK, answer)
	}
}

// create is createUser's transaction: the user, where there is none by that login, the link, the
// namespaces handed over to an administrator the bootstrap token creates (handOver), and each
// recorded.
func (s *UserAPI) create(ctx context.Context, who Principal, ask NewUser, now time.Time) (CreatedUser, bool, error) {
	var answer CreatedUser
	var created bool
	err := s.pool.Installation(ctx, db.Identity, func(ctx context.Context, wide *db.Wide) error {
		existing, err := wide.User(ctx, ask.Login)
		switch {
		case errors.Is(err, db.ErrNoPrincipal):
			// A user created with no display name reads as their login until one is given.
			name := ask.DisplayName
			if !ask.namesOne {
				name = ask.Login
			}
			if err := wide.CreateUser(ctx, db.User{Login: ask.Login, DisplayName: name, Admin: ask.Admin}); err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		case ask.otherwise(existing):
			return errOtherwise
		}
		// Read back, since when it was created is the database's to say.
		user, err := wide.User(ctx, ask.Login)
		if err != nil {
			return err
		}
		link, issued, err := s.issue(ctx, wide, who, user, now)
		if err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		handed, err := handOver(ctx, wide, who, user, now)
		if err != nil {
			return err
		}
		result := audit.Done
		if !created {
			result = audit.Unchanged
		}
		if err := wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.UserCreate, Target: user.Login, Result: result,
			Detail: map[string]any{"display_name": user.DisplayName, "admin": user.Admin},
		}); err != nil {
			return err
		}
		if err := wide.Audit(ctx, issued); err != nil {
			return err
		}
		for _, g := range handed {
			if err := wide.AuditIn(ctx, g.Scope.Namespace, audit.Record{
				Actor: string(who), Action: audit.GrantCreate, Target: g.ID, Result: audit.Done, Detail: grantDetail(g),
			}); err != nil {
				return err
			}
		}
		answer = CreatedUser{User: userOf(user), Enrolment: link}
		return nil
	})
	return answer, created, err
}

// handOver gives an administrator the bootstrap token creates the owner role on every namespace
// whose record names no owner, and answers the grants it wrote: those the token owned in effect,
// which init, agentiik-api namespace create or a v0.2 installation made, "so an upgraded or new
// installation's first administrator owns its namespaces without an extra command". The token
// ends at that administrator's first sign-in, and what it held would otherwise be shared by hand
// by the administrator it made, one namespace at a time.
//
// A namespace the administrator owns already is left, so that a repeat of the create, for a fresh
// link, gives nothing twice, and hands over a namespace made since. One somebody else holds the
// owner role on through a grant, the record naming nobody, is handed over all the same, as the
// decision reads it. Written in the create's transaction, after its rows are locked and before it
// appends to the audit log, by the bootstrap token as every act of the create is, and told to
// nobody, as a grant the token writes through the grant routes is: nobody widens their own access.
// The namespaces are held while they are handed over (db.Wide.Ownerless), so that one removed at
// the same moment is either left out or waits, and never fails the creation. Nothing is handed
// over by an administrator creating a user, nor to a user who does not administer.
func handOver(ctx context.Context, wide *db.Wide, who Principal, user db.User, now time.Time) ([]access.Grant, error) {
	if who != BootstrapOperator || !user.Admin {
		return nil, nil
	}
	namespaces, err := wide.Ownerless(ctx)
	if err != nil {
		return nil, err
	}
	principal := access.Principal{Ref: user.Login}
	held, err := wide.AccessGrantsAcross(ctx, principal, now)
	if err != nil {
		return nil, err
	}
	var handed []access.Grant
	for _, name := range namespaces {
		if access.Owns(principal, held, name, now) {
			continue
		}
		g := access.Grant{
			ID: ulid.New(), Principal: user.Login, Scope: access.Scope{Namespace: name},
			Role: access.Owner, GrantedBy: string(who), GrantedAt: now,
		}
		if err := wide.GrantAccess(ctx, g); err != nil {
			return nil, err
		}
		handed = append(handed, g)
	}
	return handed, nil
}

// issue issues a link for user, as who, revoking the one it replaces, and answers the entry that
// records it, for its caller to append once every row it locks is locked.
//
// A link the bootstrap operator issues for an administrator is a first administrator's, which
// replaces every first administrator's link still open, whoever it was for, "so that a link made
// with the bootstrap token for a mistyped login is never left open beside the one used", and ends
// with the bootstrap token. Any other is a new user's, which replaces that user's open link. Either
// is refused, db.ErrEnrolled, for a user who holds a credential already.
func (s *UserAPI) issue(ctx context.Context, wide *db.Wide, who Principal, user db.User, now time.Time) (EnrolmentLink, audit.Record, error) {
	kind := db.EnrolmentNewUser
	if who == BootstrapOperator && user.Admin {
		kind = db.EnrolmentFirstAdministrator
	}
	code, issued, err := issueCode(ctx, wide, s.enrol, string(who), user.Login, kind, now)
	if err != nil {
		return EnrolmentLink{}, audit.Record{}, err
	}
	return EnrolmentLink{Link: code.link, ExpiresAt: code.expires}, issued, nil
}

// issueEnrolment is POST /api/v1/users/{login}/enrolment: a fresh link for a user who holds no
// credential yet, revoking the one before.
func (s *UserAPI) issueEnrolment(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	login := r.PathValue("login")
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUser)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	var link EnrolmentLink
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.User(ctx, login)
		if err != nil {
			return err
		}
		var issued audit.Record
		if link, issued, err = s.issue(ctx, wide, who, user, now); err != nil {
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
	case errors.Is(err, db.ErrEnrolled):
		fail(w, http.StatusConflict, fmt.Sprintf("%s holds a credential already, and an enrolment link enrols the first passkey of an account that holds none: a lost passkey is replaced with a recovery code", login))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the enrolment link could not be issued")
	default:
		shownOnce(w, http.StatusCreated, link)
	}
}

// noUser is the refusal of a login that names no user. The routes are an administrator's, who may
// list every user, so it hides nothing and says what it means.
const noUser = "no user by that login"

// users is GET /api/v1/users: every user, by login.
func (s *UserAPI) users(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var found []db.User
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		found, err = wide.Users(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the users could not be read")
		return
	}
	listed := make([]User, 0, len(found))
	for _, u := range found {
		listed = append(listed, userOf(u))
	}
	write(w, http.StatusOK, map[string]any{"users": listed})
}

// user is GET /api/v1/users/{login}: one user, and never a credential.
func (s *UserAPI) user(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	login := r.PathValue("login")
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUser)
		return
	}
	var found db.User
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		found, err = wide.User(ctx, login)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noUser)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the user could not be read")
	default:
		write(w, http.StatusOK, userOf(found))
	}
}

// removeUser is DELETE /api/v1/users/{login}: the user, with their credentials, tokens, sessions,
// enrolment links, memberships and grants, and their personal namespace where it holds nothing.
//
// Refused while a namespace's record names them as owner, "so that no namespace is left owned by
// somebody who is gone". Their personal namespace is theirs alone and nobody else's to own, so it
// goes with them where it is empty, and refuses their removal, saying what it holds, where it is
// not: what a namespace holds is somebody's work.
//
// Refused too, once the bootstrap token has ended, for the last administrator who can sign in, not
// suspended and holding a credential the policy that applies to them lets them sign in with: nothing
// else makes an administrator after that, and an installation nobody can administer is the lockout
// the bootstrap token ends at an enrolment rather than at a creation to avoid. Before it has ended,
// the token makes another.
func (s *UserAPI) removeUser(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	login := r.PathValue("login")
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUser)
		return
	}
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		user, err := wide.User(ctx, login)
		if err != nil {
			return err
		}
		if user.Admin {
			if err := notTheLastAdministrator(ctx, wide, login, s.now(), s.ipAddressed); err != nil {
				return err
			}
		}
		owned, err := wide.NamespacesOwnedBy(ctx, login)
		if err != nil {
			return err
		}
		personal, others := false, []string(nil)
		for _, n := range owned {
			if n.Kind == db.NamespacePersonal && n.Name == login {
				personal = true
				continue
			}
			others = append(others, n.Name)
		}
		if len(others) > 0 {
			return &ownsNamespaces{principal: login, namespaces: others}
		}
		if personal {
			if err := wide.RemoveNamespace(ctx, login); err != nil {
				return err
			}
		}
		if err := wide.RemovePrincipal(ctx, login); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		if personal {
			if err := wide.Audit(ctx, audit.Record{
				Actor: string(who), Action: audit.NamespaceDelete, Target: login, Result: audit.Done,
				Detail: map[string]any{"personal": true},
			}); err != nil {
				return err
			}
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.UserDelete, Target: login, Result: audit.Done,
			Detail: map[string]any{"display_name": user.DisplayName, "admin": user.Admin},
		})
	})
	var owns *ownsNamespaces
	var holds *db.NamespaceHolds
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noUser)
	case errors.As(err, &owns):
		fail(w, http.StatusConflict, owns.Error())
	case errors.Is(err, errLastAdministrator):
		fail(w, http.StatusConflict, fmt.Sprintf("%s is the last administrator who can sign in, and an installation with none is one nobody can administer: make another administrator, and let them enrol, first", login))
	case errors.As(err, &holds):
		fail(w, http.StatusConflict, fmt.Sprintf("%s was not removed with their personal namespace: %s", login, holds.Held()))
	case errors.Is(err, db.ErrOwnsNamespace):
		// A namespace given them as owner after it was looked for, which the table refuses
		// all the same.
		fail(w, http.StatusConflict, fmt.Sprintf("%s owns a namespace given them a moment ago, and is not removed while a namespace's record names them as owner", login))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the user could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// notTheLastAdministrator refuses the removal of login, an administrator, where the bootstrap token
// has ended and no other administrator can sign in, holding a credential the policy that applies to
// them lets them sign in with (administratorsSigningIn). Every administrator's row is locked first,
// in one order, so that two administrators removing each other at once take turns and the second
// finds the first gone.
func notTheLastAdministrator(ctx context.Context, wide *db.Wide, login string, now time.Time, ipAddressed bool) error {
	signing, err := administratorsSigningIn(ctx, wide, now, ipAddressed)
	if err != nil {
		return err
	}
	b, err := wide.BootstrapHeld(ctx)
	if err != nil || !b.Ended() {
		return err
	}
	for _, a := range signing {
		if a != login {
			return nil
		}
	}
	return errLastAdministrator
}

// NewGroup is a group to create, empty or with its first members: openapi.json's groupCreate.
type NewGroup struct {
	Name    string   `json:"name"`
	Members []string `json:"members,omitempty"`
}

func (g *NewGroup) field(b *body, name string) error {
	switch name {
	case "name":
		return text(b, &g.Name)
	case "members":
		return texts(b, &g.Members, namesMax, fmt.Sprintf("a group is created with at most %d members, and the rest are added one by one", namesMax))
	}
	return unknown(name)
}

// Group is a group as the routes answer one, $defs/group: its members by login, and an empty list
// for one with none.
type Group struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

func groupOf(g db.Group) Group {
	return Group{Kind: db.KindGroup, Name: g.Name, Members: orEmpty(g.Members)}
}

// groupPrincipal is a group as a grant, the API and the audit log write it.
func groupPrincipal(name string) string { return "group:" + name }

// noGroup is the refusal of a name that names no group.
const noGroup = "no group of that name"

// errNoGroup is a group nobody created, told apart from a user nobody created.
var errNoGroup = errors.New("api: no group of that name")

// createGroup is POST /api/v1/groups: a group, empty or with its first members.
func (s *UserAPI) createGroup(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask NewGroup
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := GroupName(ask.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := distinct(ask.Members, "member", func(login string) error {
		if err := LoginRef(login); err != nil {
			return fmt.Errorf("members: %w", err)
		}
		return nil
	}); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var made db.Group
	var missing string
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.CreateGroup(ctx, ask.Name); err != nil {
			return err
		}
		for _, login := range ask.Members {
			if _, err := wide.AddMember(ctx, ask.Name, login); err != nil {
				missing = login
				return err
			}
		}
		var err error
		if made, err = wide.Group(ctx, ask.Name); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		// One entry with its first members: a group just created holds no grant, so nobody
		// gains anything by being put in it here, and what it is created with is in the entry.
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.GroupCreate, Target: groupPrincipal(ask.Name), Result: audit.Done,
			Detail: map[string]any{"members": orEmpty(made.Members)},
		})
	})
	switch {
	case errors.Is(err, db.ErrPrincipalExists):
		fail(w, http.StatusConflict, fmt.Sprintf("a group %s exists already, and a group's name is its only identity", ask.Name))
	case errors.Is(err, db.ErrNoPrincipal) && missing != "":
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("members: %s is no user, and a group's members are users, by login", missing))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the group could not be created")
	default:
		write(w, http.StatusCreated, groupOf(made))
	}
}

// groups is GET /api/v1/groups: every group, with its members.
func (s *UserAPI) groups(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var found []db.Group
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		found, err = wide.Groups(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the groups could not be read")
		return
	}
	listed := make([]Group, 0, len(found))
	for _, g := range found {
		listed = append(listed, groupOf(g))
	}
	write(w, http.StatusOK, map[string]any{"groups": listed})
}

// group is GET /api/v1/groups/{group}: one group and its members.
func (s *UserAPI) group(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	name := r.PathValue("group")
	if groupRef(name) != nil {
		fail(w, http.StatusNotFound, noGroup)
		return
	}
	var found db.Group
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		found, err = wide.Group(ctx, name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noGroup)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the group could not be read")
	default:
		write(w, http.StatusOK, groupOf(found))
	}
}

// removeGroup is DELETE /api/v1/groups/{group}: the group with its memberships and grants, "what
// the group's grants gave its members ends from their next request, and the users themselves stay".
// Refused while a namespace's record names it as owner.
func (s *UserAPI) removeGroup(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	name := r.PathValue("group")
	if groupRef(name) != nil {
		fail(w, http.StatusNotFound, noGroup)
		return
	}
	principal := groupPrincipal(name)
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		held, err := wide.Group(ctx, name)
		if err != nil {
			return err
		}
		owned, err := wide.NamespacesOwnedBy(ctx, principal)
		if err != nil {
			return err
		}
		if len(owned) > 0 {
			names := make([]string, len(owned))
			for i, n := range owned {
				names[i] = n.Name
			}
			return &ownsNamespaces{principal: principal, namespaces: names}
		}
		// An administrator removing a group they are in lifts the group's denies from their own
		// access, as revoking one of them would. The owners are told before the removal takes the
		// denies, since telling them holds each namespace, which a removal of the namespace holds
		// before it takes the grants in it: taken the other way round, the two would each wait on
		// what the other holds.
		detail := map[string]any{"members": orEmpty(held.Members)}
		if slices.Contains(held.Members, string(who)) {
			denies, err := groupGrants(ctx, wide, name, false, s.now())
			if err != nil {
				return err
			}
			if err := widened(ctx, wide, denies, db.Widening{Act: db.ActGroupRemoved, By: string(who)}, s.now(), detail); err != nil {
				return err
			}
		}
		if err := wide.RemovePrincipal(ctx, principal); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.GroupDelete, Target: principal, Result: audit.Done, Detail: detail,
		})
	})
	var owns *ownsNamespaces
	switch {
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noGroup)
	case errors.As(err, &owns):
		fail(w, http.StatusConflict, owns.Error())
	case errors.Is(err, db.ErrOwnsNamespace):
		fail(w, http.StatusConflict, fmt.Sprintf("%s owns a namespace given it a moment ago, and is not removed while a namespace's record names it as owner", principal))
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the group could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// addMember is PUT /api/v1/groups/{group}/members/{login}: one user put in the group, touching no
// grant. Put in twice is the same answer, and recorded as unchanged. Refused with 409 naming the
// setting where, once the bootstrap token has ended, it would leave no administrator able to sign in,
// the group holding a role in a namespace whose policy takes their way in (keepAnAdministrator).
// Whoever is put in, the owners of each namespace where the group holds a role are told, as of a
// grant written there by the installation's power: the user now holds what that role gives.
func (s *UserAPI) addMember(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	s.membership(w, r, who, true)
}

// removeMember is DELETE /api/v1/groups/{group}/members/{login}: one user taken out of the group,
// touching no grant. Taking out somebody who is not in it is the same answer, and recorded as
// unchanged.
func (s *UserAPI) removeMember(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	s.membership(w, r, who, false)
}

// membership puts a user in a group or takes them out, and answers the group as it now stands.
//
// A login outside its grammar is one no user can have, and is answered as no user on both, as every
// route here answers it: a request naming Alice where it meant alice is a mistake to say, and taking
// out of a group a name nobody can hold is not a change to record. A login on the grammar that no
// user has now is simply not a member, and taking it out changes nothing, as the page says.
func (s *UserAPI) membership(w http.ResponseWriter, r *http.Request, who Principal, in bool) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	name, login := r.PathValue("group"), r.PathValue("login")
	if groupRef(name) != nil {
		fail(w, http.StatusNotFound, noGroup)
		return
	}
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUser)
		return
	}
	var now db.Group
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		// The group is read first, so that one that is not there is told from a user who is
		// not, and so that taking out of a missing group is refused rather than unchanged.
		if _, err := wide.Group(ctx, name); errors.Is(err, db.ErrNoPrincipal) {
			return errNoGroup
		} else if err != nil {
			return err
		}
		var changed bool
		var err error
		action := audit.GroupMemberAdd
		if in {
			err = keepAnAdministrator(ctx, wide, s.now(), s.ipAddressed, func() error {
				// Held before the insert, as the removal of the member takes them, since the
				// owners told below may count the member among them (db.Wide.HoldMember).
				if err := wide.HoldMember(ctx, login); err != nil {
					return err
				}
				var err error
				changed, err = wide.AddMember(ctx, name, login)
				return err
			})
		} else {
			action = audit.GroupMemberRemove
			changed, err = wide.RemoveMember(ctx, name, login)
		}
		if err != nil {
			return err
		}
		if now, err = wide.Group(ctx, name); err != nil {
			return err
		}
		// Putting somebody in a group widens their access wherever its grants give a role, which
		// the namespace's owners are told of as of a grant the installation's power wrote, whoever
		// is put in; and an administrator taking themselves out of one widens their own wherever
		// its denies took something away: "an administrator widening their own access notifies
		// the namespace owners", however they came to it. Every caller here administers the
		// installation, the bootstrap token included.
		detail := map[string]any{"member": login}
		var reach []access.Grant
		var what db.Widening
		switch {
		case changed && in:
			reach, err = groupGrants(ctx, wide, name, true, s.now())
			what = db.Widening{Act: db.ActJoinedGroup, By: string(who), Member: login}
		case changed && string(who) == login:
			reach, err = groupGrants(ctx, wide, name, false, s.now())
			what = db.Widening{Act: db.ActLeftGroup, By: string(who)}
		}
		if err != nil {
			return err
		}
		if err := widened(ctx, wide, reach, what, s.now(), detail); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		result := audit.Done
		if !changed {
			result = audit.Unchanged
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: action, Target: groupPrincipal(name), Result: result, Detail: detail,
		})
	})
	var locked *errLockedOut
	switch {
	// A group removed between the read and the insert is refused by the table, naming it.
	case errors.Is(err, errNoGroup), errors.Is(err, db.ErrNoPrincipal) && strings.HasSuffix(err.Error(), ": "+groupPrincipal(name)):
		fail(w, http.StatusNotFound, noGroup)
	case errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, noUser)
	case errors.As(err, &locked):
		failSetting(w, http.StatusConflict, fmt.Sprintf("putting %s in %s would leave no administrator able to sign in: it brings those who can now under the authentication policy of a namespace where %s holds a role, which accepts none of the passkeys or passwords they hold, and nobody would be left to administer this installation. Enrol a passkey that policy accepts first", login, groupPrincipal(name), groupPrincipal(name)), locked.setting)
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the group's members could not be changed")
	default:
		write(w, http.StatusOK, groupOf(now))
	}
}

// groupGrants answers the grants of group that have not expired at now, in every namespace and at
// both scopes: those giving a role where gives is set, and its denies where it is not.
func groupGrants(ctx context.Context, wide *db.Wide, group string, gives bool, now time.Time) ([]access.Grant, error) {
	all, err := wide.AccessGrantsAcross(ctx, access.Principal{Ref: groupPrincipal(group)}, now)
	if err != nil {
		return nil, err
	}
	var reach []access.Grant
	for _, g := range all {
		if (g.Role != "") == gives {
			reach = append(reach, g)
		}
	}
	return reach, nil
}

// widened tells the owners of the namespace of each grant that an administrator widened access there
// by it, the group's role given to a member or its deny taken from them, as what says, the act, who
// did it and the member put in, as TellOwners tells them of a grant written there by the
// installation's power, and writes who was told into detail as notified, by namespace. A namespace
// whose owners are the administrator alone is written with nobody, so that the entry says the act
// widened access there all the same. It writes nothing where grants is empty.
func widened(ctx context.Context, wide *db.Wide, grants []access.Grant, what db.Widening, now time.Time, detail map[string]any) error {
	if len(grants) == 0 {
		return nil
	}
	what.At = now.UTC().Truncate(time.Microsecond)
	notified := map[string][]string{}
	for _, g := range grants {
		what.Grant = g
		told, err := wide.TellOwnersIn(ctx, g.Scope.Namespace, what)
		if err != nil {
			return err
		}
		names := notified[g.Scope.Namespace]
		for _, n := range told {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		notified[g.Scope.Namespace] = orEmpty(names)
	}
	detail["notified"] = notified
	return nil
}

// shownOnce answers a secret shown this once, a link's code, and says no cache on the way may keep a
// copy of it.
func shownOnce(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	write(w, status, body)
}

// bootstrapEnded answers the bootstrap token asked for something after the first administrator
// enrolled, which ended it while the request was being served: as the next request with it is
// answered.
func bootstrapEnded(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	refuse(w, http.StatusUnauthorized, noTokenSinceTheBootstrap)
}
