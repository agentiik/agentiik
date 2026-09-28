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
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// The authentication policy: what applies to one account, read in one place by every ceremony,
// every sign-in, every session and the sign-in page; and the routes that set it, GET and PUT
// /api/v1/auth/policy for the installation and GET and PUT /api/v1/{ns}/auth/policy for what a
// namespace tightens.
//
// # What applies
//
// "An account signs in under the installation's policy tightened by that of each namespace it holds
// a grant in", "the stricter of the two, setting by setting": passwords forbidden where any forbids
// them, a passkey required where any requires one, user verification required where any requires
// it, synced passkeys refused where any refuses them, and the largest min_passkeys. It is read at
// every request that asks, never kept, so that a policy changed, a grant given or a passkey
// registered applies from the next request.
//
// On an installation addressed by an IP address, where a browser runs no passkey ceremony, what
// applies is the policy "with passwords allowed and no passkey", whatever is stored, and the stored
// policy is left as it is, so that its settings apply again once the installation is addressed by
// a name; a policy forbidding passwords is refused there, since nobody could sign in.
//
// # The path off passwords
//
// Where a passkey is required, a session a password opened enrols passkeys and nothing else, until
// the account holds min_passkeys passkeys the policy accepts; the passkey that brings it there
// deletes the password, the hash and the TOTP generator beside it as rows, since the password was a
// way to the passkeys and they are held now ("holds min_passkeys: password hash deleted, not
// disabled"). A password found beside them all the same opens a session that only enrols.
//
// Forbidding passwords deletes every password it reaches in the transaction that forbids them, and
// suspends each account it reaches that holds no passkey the policy accepts, "rather than leaving
// it reachable by a password the policy says no longer exists", recording why; a passkey enrolled
// from an enrolment link or a recovery code lifts that suspension and no other (passkeys.go).

// accountPolicy is the authentication policy that applies to one account: the installation's,
// tightened setting by setting by that of every namespace the account holds a grant in.
type accountPolicy struct {
	// passwordsForbidden is password: forbidden; passkeyRequired is passkey: required.
	passwordsForbidden bool
	passkeyRequired    bool

	// userVerification is user_verification: required, on registration and on every assertion.
	userVerification bool

	// deviceBoundOnly refuses a passkey whose Backup Eligibility flag is set.
	deviceBoundOnly bool

	// minPasskeys is how many passkeys the policy accepts the account holds before its password
	// goes, and below which none is removed.
	minPasskeys int
}

// The settings of the policy, as $defs/authPolicy spells them, which a refusal names.
const (
	passwordSetting         = "password"
	passkeySetting          = "passkey"
	userVerificationSetting = "user_verification"
	deviceBoundOnly         = "device_bound_only"
	minPasskeys             = "min_passkeys"
)

// accepts says whether c is a passkey the policy accepts: any passkey, but a synced one where
// device_bound_only applies, since it signs nobody in there.
func (p accountPolicy) accepts(c db.Credential) bool {
	return c.Type == db.CredentialPasskey && !(p.deviceBoundOnly && c.BackupEligible)
}

// passkeys counts the passkeys among held the policy accepts.
func (p accountPolicy) passkeys(held []db.Credential) int {
	n := 0
	for _, c := range held {
		if p.accepts(c) {
			n++
		}
	}
	return n
}

// enrolling says whether a session a password opened may only enrol: wherever a passkey is
// required, since the password is then the way to the passkeys and never a way round them. It enrols
// until the account holds min_passkeys the policy accepts, when the passkey that brings it there
// takes the password, and the session with it; a password found beside them, set since or held when
// the policy came to require a passkey, gives no more than that, rather than the full session its
// holder's passkeys give.
func (p accountPolicy) enrolling() bool {
	return p.passkeyRequired
}

// enrolledPast says whether a password set now could only ever enrol, and has nothing left to enrol
// for: the policy requires a passkey, so that every session a password opens only enrols, and the
// account holds the min_passkeys passkeys it accepts already, beside which a password goes at the next
// passkey and gives no more than enrolling until then. Setting one is refused rather than kept as a
// credential that does nothing but stand beside the passkeys.
func (p accountPolicy) enrolledPast(held []db.Credential) bool {
	return p.passkeyRequired && p.passkeys(held) >= p.minPasskeys
}

// offPasswords says whether the policy takes the account off passwords: it forbids them, or it
// requires a passkey, the step before forbidding them, where a password is the way to a passkey and
// nothing more. The passkey that brings such an account to min_passkeys takes its password.
func (p accountPolicy) offPasswords() bool {
	return p.passwordsForbidden || p.passkeyRequired
}

// policyFor is the policy that applies to login at now: the installation's, tightened by that of
// every namespace login holds a grant carrying a role in, its own or one of its groups'. A login no
// user holds holds no grant, and the empty login is nobody, which the sign-in page asks about before
// anybody has signed in: each is under the installation's alone. On an installation addressed by an
// IP address, passwords are allowed and no passkey is required, whatever the policies say.
func policyFor(ctx context.Context, wide *db.Wide, login string, now time.Time, ipAddressed bool) (accountPolicy, error) {
	installation, tightening, err := policiesOf(ctx, wide, login, now)
	if err != nil {
		return accountPolicy{}, err
	}
	p := accountPolicy{
		passwordsForbidden: installation.Password == "forbidden",
		passkeyRequired:    installation.Passkey == "required",
		userVerification:   installation.UserVerification != "preferred",
		deviceBoundOnly:    installation.DeviceBoundOnly != nil && *installation.DeviceBoundOnly,
		minPasskeys:        max(installation.MinPasskeys, 1),
	}
	for _, tightened := range tightening {
		p.passwordsForbidden = p.passwordsForbidden || tightened.Password == "forbidden"
		p.passkeyRequired = p.passkeyRequired || tightened.Passkey == "required"
		p.userVerification = p.userVerification || tightened.UserVerification == "required"
		p.deviceBoundOnly = p.deviceBoundOnly || (tightened.DeviceBoundOnly != nil && *tightened.DeviceBoundOnly)
		p.minPasskeys = max(p.minPasskeys, tightened.MinPasskeys)
	}
	if ipAddressed {
		p.passwordsForbidden, p.passkeyRequired = false, false
	}
	return p, nil
}

// policiesOf is the policies that apply to login: the installation's, and those of every namespace
// login holds a grant in, its own or one of its groups', since "an account signs in under the
// installation's policy tightened by that of each namespace it holds a grant in". A deny alone is
// not a grant, as it is nowhere else.
func policiesOf(ctx context.Context, wide *db.Wide, login string, now time.Time) (db.AuthPolicy, []db.AuthPolicy, error) {
	installation, err := wide.InstallationPolicy(ctx)
	if err != nil || login == "" {
		return installation, nil, err
	}
	groups, err := wide.GroupsOf(ctx, login)
	if err != nil {
		return db.AuthPolicy{}, nil, err
	}
	grants, err := wide.AccessGrantsAcross(ctx, access.Principal{Ref: login, Groups: groups}, now)
	if err != nil {
		return db.AuthPolicy{}, nil, err
	}
	var tightening []db.AuthPolicy
	read := map[string]bool{}
	for _, g := range grants {
		if g.Role == "" || read[g.Scope.Namespace] {
			continue
		}
		read[g.Scope.Namespace] = true
		tightened, err := wide.NamespacePolicy(ctx, g.Scope.Namespace)
		if err != nil {
			return db.AuthPolicy{}, nil, err
		}
		tightening = append(tightening, tightened)
	}
	return installation, tightening, nil
}

// AuthPolicy is $defs/authPolicy as the policy routes read and answer it: the installation's with
// every setting written, a namespace's with the settings it tightens and no other.
type AuthPolicy struct {
	Password         string `json:"password,omitempty"`
	Passkey          string `json:"passkey,omitempty"`
	UserVerification string `json:"user_verification,omitempty"`
	DeviceBoundOnly  *bool  `json:"device_bound_only,omitempty"`
	MinPasskeys      int    `json:"min_passkeys,omitempty"`
}

// minPasskeysMax is the most min_passkeys may be: what the column, a PostgreSQL integer, holds, so
// that one past it is this request's refusal rather than the database's.
const minPasskeysMax = quotaCountMax

func (p *AuthPolicy) field(b *body, name string) error {
	if b.d.PeekKind() == jsontext.KindNull {
		if _, err := b.d.ReadToken(); err != nil {
			return malformed(err)
		}
		return fmt.Errorf("%s is null, and a setting the policy leaves as it inherits is left out rather than written null", name)
	}
	among := func(into *string, values ...string) error {
		if err := text(b, into); err != nil {
			return err
		}
		for _, v := range values {
			if *into == v {
				return nil
			}
		}
		return fmt.Errorf("%s is %.64q, and it is %s or %s", name, *into, values[0], values[1])
	}
	switch name {
	case passwordSetting:
		return among(&p.Password, "allowed", "forbidden")
	case passkeySetting:
		return among(&p.Passkey, "optional", "required")
	case userVerificationSetting:
		return among(&p.UserVerification, "required", "preferred")
	case deviceBoundOnly:
		var bound bool
		if err := flag(b, &bound); err != nil {
			return err
		}
		p.DeviceBoundOnly = &bound
		return nil
	case minPasskeys:
		if err := integer(b, &p.MinPasskeys); err != nil {
			return err
		}
		if p.MinPasskeys < 1 || p.MinPasskeys > minPasskeysMax {
			return fmt.Errorf("min_passkeys is %d, and it is a whole number from 1 to %d: zero would let a password go from an account with nothing to replace it", p.MinPasskeys, minPasskeysMax)
		}
		return nil
	}
	return unknown(name)
}

// stored is the policy as package db writes it.
func (p AuthPolicy) stored() db.AuthPolicy {
	return db.AuthPolicy{
		Password: p.Password, Passkey: p.Passkey, UserVerification: p.UserVerification,
		DeviceBoundOnly: p.DeviceBoundOnly, MinPasskeys: p.MinPasskeys,
	}
}

// policyOf is a stored policy as it is answered.
func policyOf(p db.AuthPolicy) AuthPolicy {
	return AuthPolicy{
		Password: p.Password, Passkey: p.Passkey, UserVerification: p.UserVerification,
		DeviceBoundOnly: p.DeviceBoundOnly, MinPasskeys: p.MinPasskeys,
	}
}

// withDefaults is p with every setting it leaves out at the documented default: "a setting the body
// leaves out returns to its default, so that the body is the whole of the policy".
func (p AuthPolicy) withDefaults() AuthPolicy {
	if p.Password == "" {
		p.Password = "allowed"
	}
	if p.Passkey == "" {
		p.Passkey = "required"
	}
	if p.UserVerification == "" {
		p.UserVerification = "required"
	}
	if p.DeviceBoundOnly == nil {
		bound := false
		p.DeviceBoundOnly = &bound
	}
	if p.MinPasskeys == 0 {
		p.MinPasskeys = 2
	}
	return p
}

// loosens answers the first setting of a namespace's policy that is looser than the installation's,
// and what the installation holds there, or an empty setting where none is: "a namespace may
// tighten the installation's policy, never loosen it".
func (p AuthPolicy) loosens(installation db.AuthPolicy) (string, string) {
	switch {
	case p.Password == "allowed" && installation.Password == "forbidden":
		return passwordSetting, "forbidden"
	case p.Passkey == "optional" && installation.Passkey == "required":
		return passkeySetting, "required"
	case p.UserVerification == "preferred" && installation.UserVerification == "required":
		return userVerificationSetting, "required"
	case p.DeviceBoundOnly != nil && !*p.DeviceBoundOnly && installation.DeviceBoundOnly != nil && *installation.DeviceBoundOnly:
		return deviceBoundOnly, "true"
	case p.MinPasskeys != 0 && p.MinPasskeys < installation.MinPasskeys:
		return minPasskeys, fmt.Sprint(installation.MinPasskeys)
	}
	return "", ""
}

// PolicyOptions are what the policy routes are given.
type PolicyOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: a host that is an IP address is
	// an installation where no passkey ceremony runs, and where a policy forbidding passwords is
	// refused.
	PublicURL string

	// Now is the clock grants expire by, the wall clock where it is nil.
	Now func() time.Time
}

// PolicyAPI serves the authentication policy's routes.
type PolicyAPI struct {
	pool        *db.Pool
	now         func() time.Time
	ipAddressed bool

	// checked is run between a change's checks and the transaction that writes it, where a test
	// changes what was checked. Nil outside tests.
	checked func()
}

// The sentences setting a policy is refused with.
const (
	// forbiddenByIP is passwords forbidden on an installation addressed by an IP address.
	forbiddenByIP = "this installation is addressed by an IP address, where no passkey ceremony runs and a password is the one way in: a policy forbidding passwords would leave nobody able to sign in, and is refused until the installation is addressed by a name"

	// lockedOut is a policy under which no administrator who could sign in before it still can.
	lockedOut = "this policy would leave no administrator able to sign in, since none of those who can now holds a passkey it accepts or a password it allows, and nobody to administer this installation: enrol a passkey it accepts first"
)

// NewPolicies registers the policy routes on a router: reading the installation's is anybody's who
// is signed in, since the policy applies to them, and reading a namespace's is an administrator's
// and whoever holds a grant in it, for the same reason; setting either is an administrator's.
func NewPolicies(rt *Router, o PolicyOptions) (*PolicyAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the policy is kept there")
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("api: the policy routes are given the public URL, whose host says whether passwords may be forbidden, and the one given names none")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &PolicyAPI{pool: o.Pool, now: o.Now, ipAddressed: net.ParseIP(u.Hostname()) != nil}
	if err := rt.HandleOwn("GET", "/api/v1/auth/policy", Own{}, s.installation); err != nil {
		return nil, err
	}
	admin := Needs{Permission: GrantManage, Scope: Installation}
	for _, r := range []struct {
		method, pattern string
		guard           Guard
		handler         Handler
	}{
		{"PUT", "/api/v1/auth/policy", admin, s.setInstallation},
		{"GET", "/api/v1/{namespace}/auth/policy", OnNamespace{}, s.namespace},
		{"PUT", "/api/v1/{namespace}/auth/policy", admin, s.setNamespace},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// installation is GET /api/v1/auth/policy: the installation's policy as it is stored, every setting
// written, and never what an installation addressed by an IP address applies in its place, since
// the stored one is what an administrator set and what applies again once it is addressed by a
// name.
func (s *PolicyAPI) installation(w http.ResponseWriter, r *http.Request, _ Caller) {
	var p db.AuthPolicy
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		p, err = wide.InstallationPolicy(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the authentication policy could not be read")
		return
	}
	write(w, http.StatusOK, policyOf(p))
}

// errLockedOut is a policy that would leave no administrator able to sign in, and the setting that
// would, as lockingSetting says.
type errLockedOut struct{ setting string }

func (e *errLockedOut) Error() string { return lockedOut }

// lockingSetting is the setting a change that left none of the administrators of before able to sign
// in takes their way in away by, read under the policy the change wrote against was, what applied
// to each before it: device_bound_only where the change came to refuse a synced passkey one of them
// holds, since with synced passkeys accepted that one would still sign in, and forbidding passwords
// would neither delete it nor suspend its holder; password otherwise. A synced passkey refused before
// the change took nothing that the change took, and is not named for it. The other settings take no
// credential away from anybody that this can know of.
func lockingSetting(ctx context.Context, wide *db.Wide, before []string, was map[string]accountPolicy, now time.Time, ipAddressed bool) (string, error) {
	for _, login := range before {
		policy, err := policyFor(ctx, wide, login, now, ipAddressed)
		if err != nil {
			return "", err
		}
		held, err := wide.CredentialsOf(ctx, login)
		if err != nil {
			return "", err
		}
		synced := slices.ContainsFunc(held, func(c db.Credential) bool { return c.Type == db.CredentialPasskey && c.BackupEligible })
		if policy.deviceBoundOnly && !was[login].deviceBoundOnly && synced && !ipAddressed {
			return deviceBoundOnly, nil
		}
	}
	return passwordSetting, nil
}

// administratorsSigningIn answers the logins of the installation's administrators who can sign in
// under the policy as it stands in the transaction wide is: not suspended, and holding a passkey the
// policy that applies to them accepts, where a passkey signs anybody in, or a password where it
// allows passwords. Every administrator's row is held first, in the order of their logins
// (db.Wide.Administrators), as every act that counts them takes them, so that two acts that could
// each take the last one away take turns.
//
// Nothing here knows whether a passkey was registered with user verification, which is not
// recorded: a policy coming to require it is not counted as taking anything away.
func administratorsSigningIn(ctx context.Context, wide *db.Wide, now time.Time, ipAddressed bool) ([]string, error) {
	admins, err := wide.Administrators(ctx)
	if err != nil {
		return nil, err
	}
	var signing []string
	for _, a := range admins {
		user, err := wide.User(ctx, a.Login)
		if err != nil {
			return nil, err
		}
		if user.Suspended {
			continue
		}
		policy, err := policyFor(ctx, wide, a.Login, now, ipAddressed)
		if err != nil {
			return nil, err
		}
		held, err := wide.CredentialsOf(ctx, a.Login)
		if err != nil {
			return nil, err
		}
		password := slices.ContainsFunc(held, func(c db.Credential) bool { return c.Type == db.CredentialPassword })
		if (!ipAddressed && policy.passkeys(held) > 0) || (!policy.passwordsForbidden && password) {
			signing = append(signing, a.Login)
		}
	}
	return signing, nil
}

// keepAnAdministrator runs change, which writes something in the transaction wide is, and refuses
// it with errLockedOut where, once the bootstrap token has ended, it would leave no administrator
// able to sign in of those who could before it: the installation would have nobody to administer it,
// as the removal of the last administrator who can sign in is refused. The bootstrap token, while it
// lasts, administers it and makes another; and a change where nobody could sign in before takes
// nothing away that was there.
//
// A policy changed is one such change. A grant carrying a role and a user put in a group are the
// others, since each brings who it reaches under the policy of the namespaces the role is held in:
// the only administrator given a role in a namespace forbidding passwords, or put in a group holding
// one there, would find their password refused at their next sign-in, as the policy's own change
// would have made it.
func keepAnAdministrator(ctx context.Context, wide *db.Wide, now time.Time, ipAddressed bool, change func() error) error {
	before, err := administratorsSigningIn(ctx, wide, now, ipAddressed)
	if err != nil {
		return err
	}
	was := map[string]accountPolicy{}
	for _, login := range before {
		if was[login], err = policyFor(ctx, wide, login, now, ipAddressed); err != nil {
			return err
		}
	}
	if err := change(); err != nil {
		return err
	}
	after, err := administratorsSigningIn(ctx, wide, now, ipAddressed)
	if err != nil {
		return err
	}
	bootstrap, err := wide.Bootstrap(ctx)
	if err != nil {
		return err
	}
	if bootstrap.Ended() && len(before) > 0 && len(after) == 0 {
		setting, err := lockingSetting(ctx, wide, before, was, now, ipAddressed)
		if err != nil {
			return err
		}
		return &errLockedOut{setting: setting}
	}
	return nil
}

// setInstallation is PUT /api/v1/auth/policy: the installation's policy, whole, a setting left out
// at its default.
//
// Where it forbids passwords that were allowed, every password goes in the same transaction and
// every account holding no passkey the policy accepts is suspended. A change that would leave no
// administrator able to sign in once the bootstrap token has ended is refused whole, whichever
// setting would: forbidding passwords, or refusing the synced passkeys they hold
// (keepAnAdministrator). The bootstrap token's change is refused once the bootstrap has ended, as
// its every act is.
func (s *PolicyAPI) setInstallation(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask AuthPolicy
	if err := readObject(r, &ask, smallMaxBytes, "the authentication policy"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	set := ask.withDefaults()
	if s.ipAddressed && set.Password == "forbidden" {
		failSetting(w, http.StatusConflict, forbiddenByIP, passwordSetting)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	if s.checked != nil {
		s.checked()
	}
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		was, err := wide.InstallationPolicy(ctx)
		if err != nil {
			return err
		}
		detail := map[string]any{"policy": set, "was": policyOf(was)}
		forbids := set.Password == "forbidden" && was.Password != "forbidden"
		var removed []entry
		if err := keepAnAdministrator(ctx, wide, now, s.ipAddressed, func() error {
			if err := wide.SetInstallationPolicy(ctx, set.stored(), now); err != nil {
				return err
			}
			if !forbids {
				return nil
			}
			logins, err := wide.Logins(ctx)
			if err != nil {
				return err
			}
			removed, err = s.forbidPasswords(ctx, wide, who, logins, now, detail)
			return err
		}); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		return appendEntries(ctx, wide, append([]entry{{record: audit.Record{
			Actor: string(who), Action: audit.PolicyChange, Target: "installation", Result: audit.Done, Detail: detail,
		}}}, removed...))
	})
	var locked *errLockedOut
	switch {
	case errors.As(err, &locked):
		failSetting(w, http.StatusConflict, lockedOut, locked.setting)
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the authentication policy could not be set")
	default:
		write(w, http.StatusOK, set)
	}
}

// namespace is GET /api/v1/{namespace}/auth/policy, which the router let through to a caller who
// sees the namespace: what it tightens, and nothing it inherits.
func (s *PolicyAPI) namespace(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	if NamespaceRef(over.Namespace) != nil {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	var p db.AuthPolicy
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if _, err := wide.NamespaceNamed(ctx, over.Namespace); err != nil {
			return err
		}
		var err error
		p, err = wide.NamespacePolicy(ctx, over.Namespace)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace's authentication policy could not be read")
	default:
		write(w, http.StatusOK, policyOf(p))
	}
}

// loosened is a namespace's policy that would loosen a setting of the installation's.
type loosened struct{ setting, installation string }

func (l *loosened) Error() string {
	return fmt.Sprintf("a namespace may tighten the installation's policy, never loosen it: %s is %s there", l.setting, l.installation)
}

// setNamespace is PUT /api/v1/{namespace}/auth/policy: what the namespace tightens, whole, a setting
// left out inherited, and an empty object tightening nothing.
//
// A setting looser than the installation's is refused naming it, against the installation's policy
// as it stands in the transaction that writes the namespace's. Where the namespace comes to forbid
// passwords that nothing forbade before, the accounts holding a grant in it lose their passwords and
// those holding no passkey the policy accepts are suspended, as forbidding them on the installation
// does to every account.
func (s *PolicyAPI) setNamespace(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := over.Namespace
	if NamespaceRef(name) != nil {
		fail(w, http.StatusNotFound, "there is no namespace of that name")
		return
	}
	var ask AuthPolicy
	if err := readObject(r, &ask, smallMaxBytes, "the namespace's authentication policy"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if s.ipAddressed && ask.Password == "forbidden" {
		failSetting(w, http.StatusConflict, forbiddenByIP, passwordSetting)
		return
	}
	now := s.now().Truncate(time.Microsecond)
	if s.checked != nil {
		s.checked()
	}
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		if _, err := wide.NamespaceNamed(ctx, name); err != nil {
			return err
		}
		installation, err := wide.InstallationPolicy(ctx)
		if err != nil {
			return err
		}
		if setting, held := ask.loosens(installation); setting != "" {
			return &loosened{setting: setting, installation: held}
		}
		was, err := wide.NamespacePolicy(ctx, name)
		if err != nil {
			return err
		}
		detail := map[string]any{"policy": ask, "was": policyOf(was)}
		forbids := ask.Password == "forbidden" && was.Password != "forbidden" && installation.Password != "forbidden"
		var removed []entry
		if err := keepAnAdministrator(ctx, wide, now, s.ipAddressed, func() error {
			if err := wide.SetNamespacePolicy(ctx, name, ask.stored(), now); err != nil {
				return err
			}
			if !forbids {
				return nil
			}
			logins, err := wide.UsersUnderPolicy(ctx, name, now)
			if err != nil {
				return err
			}
			removed, err = s.forbidPasswords(ctx, wide, who, logins, now, detail)
			return err
		}); err != nil {
			return err
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		return appendEntries(ctx, wide, append([]entry{{namespace: name, record: audit.Record{
			Actor: string(who), Action: audit.PolicyChange, Target: name, Result: audit.Done, Detail: detail,
		}}}, removed...))
	})
	var loose *loosened
	var locked *errLockedOut
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "there is no namespace of that name")
	case errors.As(err, &loose):
		failSetting(w, http.StatusConflict, loose.Error(), loose.setting)
	case errors.As(err, &locked):
		failSetting(w, http.StatusConflict, lockedOut, locked.setting)
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the namespace's authentication policy could not be set")
	default:
		write(w, http.StatusOK, ask)
	}
}

// forbidPasswords does what forbidding passwords does to the accounts of logins, in the transaction
// wide is, which has just written the policy that forbids them: for each account the policy now
// applying to it forbids passwords to, its password goes, as a row, and the TOTP generator beside it
// with it, which ends every session the password opened; and the account is suspended where it holds
// no passkey the policy accepts, recorded as suspended for that, unless it is suspended already.
// What it did is written into detail, for the entry recording the change, and each credential that
// went is answered as the credential.remove entry recording it, by who, whose change took it, for
// its caller to append after that entry: a password the policy took is recorded as one its holder
// removed is, so that whoever looks for what became of it finds it under its own identifier.
//
// Each account's row is held before its credentials are read, as every act on an account holds it,
// so that a passkey registered at the same moment is seen or waits, and an account is never
// suspended beside the passkey that would have kept it in. The accounts are taken in the order of
// their logins, the order the administrators are read in, whose rows keepAnAdministrator holds
// already.
func (s *PolicyAPI) forbidPasswords(ctx context.Context, wide *db.Wide, who Principal, logins []string, now time.Time, detail map[string]any) ([]entry, error) {
	deleted, suspended := []string{}, []string{}
	var removed []entry
	for _, login := range logins {
		user, err := wide.HoldUser(ctx, login)
		if errors.Is(err, db.ErrNoPrincipal) {
			// Removed since the logins were read, with everything they held.
			continue
		}
		if err != nil {
			return nil, err
		}
		policy, err := policyFor(ctx, wide, login, now, s.ipAddressed)
		if err != nil {
			return nil, err
		}
		if !policy.passwordsForbidden {
			continue
		}
		held, err := wide.CredentialsOf(ctx, login)
		if err != nil {
			return nil, err
		}
		for _, c := range held {
			if c.Type != db.CredentialPassword {
				continue
			}
			// The TOTP generator goes with the password's row, as the table holds it, and the
			// sessions the password opened with it.
			if err := wide.RemoveCredential(ctx, login, c.ID); err != nil {
				return nil, err
			}
			deleted = append(deleted, login)
			removed = append(removed, entry{record: audit.Record{
				Actor: string(who), Action: audit.CredentialRemove, Target: c.ID, Result: audit.Done,
				Detail: map[string]any{"type": db.CredentialPassword, "login": login, "reason": forbiddenTakes},
			}})
			for _, totp := range held {
				if totp.Type == db.CredentialTOTP {
					removed = append(removed, entry{record: audit.Record{
						Actor: string(who), Action: audit.CredentialRemove, Target: totp.ID, Result: audit.Done,
						Detail: map[string]any{"type": db.CredentialTOTP, "login": login, "reason": "removed with the password it stood beside"},
					}})
				}
			}
		}
		if policy.passkeys(held) == 0 && !user.Suspended {
			if _, err := wide.Suspend(ctx, login, db.SuspendedNoPasskey); err != nil {
				return nil, err
			}
			suspended = append(suspended, login)
		}
	}
	detail["passwords_deleted"], detail["suspended"] = deleted, suspended
	return removed, nil
}

// forbiddenTakes is why a password goes when a policy change comes to forbid passwords to its
// account.
const forbiddenTakes = "the authentication policy came to forbid passwords to the account"

// retired is why a password goes once its account holds min_passkeys, as its entry says.
const retired = "the account holds min_passkeys passkeys the policy accepts, and the policy takes it off passwords"

// retirePassword takes login's password, and the TOTP generator beside it, where the passkeys login
// now holds bring them to min_passkeys under a policy that takes them off passwords: "holds
// min_passkeys: password hash deleted, not disabled". It answers the entries recording what went,
// for its caller to append. The sessions the password opened end with it, the one a registration was
// made from among them where a password opened it: its user signs in with a passkey from then on.
//
// Its caller holds the user's row (HoldUser), as every act on an account does, and has written the
// passkey that may bring them there.
func retirePassword(ctx context.Context, wide *db.Wide, login string, now time.Time) ([]entry, error) {
	policy, err := policyFor(ctx, wide, login, now, false)
	if err != nil {
		return nil, err
	}
	held, err := wide.CredentialsOf(ctx, login)
	if err != nil || !policy.offPasswords() || policy.passkeys(held) < policy.minPasskeys {
		return nil, err
	}
	var entries []entry
	for _, c := range held {
		if c.Type != db.CredentialPassword {
			continue
		}
		// The TOTP generator goes with the password's row, as the table holds it.
		if err := wide.RemoveCredential(ctx, login, c.ID); err != nil {
			return nil, err
		}
		entries = append(entries, entry{record: audit.Record{
			Actor: login, Action: audit.CredentialRemove, Target: c.ID, Result: audit.Done,
			Detail: map[string]any{"type": db.CredentialPassword, "reason": retired},
		}})
	}
	for _, c := range held {
		if c.Type == db.CredentialTOTP && len(entries) > 0 {
			entries = append(entries, entry{record: audit.Record{
				Actor: login, Action: audit.CredentialRemove, Target: c.ID, Result: audit.Done,
				Detail: map[string]any{"type": db.CredentialTOTP, "reason": "removed with the password it stood beside"},
			}})
		}
	}
	return entries, nil
}
