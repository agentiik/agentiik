package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/db"
)

// Who a request is from, and what they may do: the installation's Identify and Authorizer, over the
// principals, their tokens, their grants and the bootstrap state the database holds.
//
// Both halves read the database at every request rather than remembering an answer, since a grant
// revoked "from the next request" and a token revoked one by one are only true of an API that asks
// again.

// BootstrapOperator is the principal the bootstrap token identifies.
//
// The bootstrap token is the v0.2 operator token under its v0.3.0 name, so its principal is written
// operator, as that operator was on every row it left: a run it started under v0.2 is its own, and
// what it writes now reads as those rows read. operator is refused as a login, so nobody created
// later is taken for it.
const BootstrapOperator Principal = "operator"

// The sentences a bearer token that opens nothing is refused with. One for every reason, since
// telling a caller which reason it was tells somebody guessing whether they had a real one, and the
// second only once the bootstrap token has ended, which is when a token that stopped working is most
// likely that one: the line stays in the installation's settings, and a script still presents it.
const (
	noToken = "that token opens nothing: it is no API token this installation issued, or it was revoked or has expired, or its holder is suspended"

	noTokenSinceTheBootstrap = noToken + ". If it is the bootstrap token, that ended when the first administrator enrolled a passkey: sign in with agk login, or use a service account's token"
)

// Principals identifies a request's principal from the credential it presents, and answers what
// that principal holds from its grants. It is what serve hands NewRouter, both halves.
type Principals struct {
	pool *db.Pool
	now  func() time.Time
}

// NewPrincipals builds them over an installation's database. now is the clock tokens and grants
// expire by, the wall clock where it is nil.
func NewPrincipals(pool *db.Pool, now func() time.Time) (*Principals, error) {
	if pool == nil {
		return nil, errors.New("api: no database, and who a request is from is written there")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Principals{pool: pool, now: now}, nil
}

// Identify is the router's: the principal a bearer token belongs to, with the token's scope, or
// the bootstrap operator for the bootstrap token while it has not ended.
//
// A token is looked up by its SHA-256, as it is kept, and only one still live is answered: neither
// expired nor revoked, and not a suspended user's. Its use is recorded, so that a token nobody uses
// can be seen and revoked. The bootstrap token's hash is compared in constant time, since it is one
// value and not an index somebody could time.
//
// A session cookie is the console's credential, and it is identified here beside the bearer token
// once sessions are served. Until then a request with no bearer token carries no credential.
func (p *Principals) Identify(r *http.Request) (Identity, error) {
	presented, ok := bearerOf(r)
	if !ok {
		return Identity{}, nil
	}
	hash := sha256.Sum256([]byte(presented))
	now := p.now()
	var as Identity
	err := p.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		bootstrap, err := w.Bootstrap(ctx)
		if err != nil {
			return err
		}
		if !bootstrap.Ended() && subtle.ConstantTimeCompare(bootstrap.TokenHash, hash[:]) == 1 {
			as = Identity{Principal: BootstrapOperator}
			return nil
		}
		token, err := w.TokenByHash(ctx, hash[:], now)
		if errors.Is(err, db.ErrNoToken) {
			as = Identity{Refused: noToken}
			if bootstrap.Ended() {
				as.Refused = noTokenSinceTheBootstrap
			}
			return nil
		}
		if err != nil {
			return err
		}
		scope, err := access.ParseTokenScope(token.Permissions, token.Within)
		if err != nil {
			return fmt.Errorf("api: the scope of token %s: %w", token.ID, err)
		}
		if err := w.TokenUsed(ctx, token.ID, now); err != nil {
			return err
		}
		as = Identity{Principal: Principal(token.Principal), Scope: scope}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return as, nil
}

// Allow is the router's Authorizer: whether who holds what over one target.
//
// At the installation, which a target naming no namespace is, the one question is whether who
// administers it: "a platform administrator manages users, groups, namespaces, quotas, runners,
// runner policies and the authentication policy", and the routes that do are the ones the page says
// require grant:manage at installation scope. So an administrator holds grant:manage there, and
// nothing else, and nobody else holds anything: no grant is scoped to the installation.
//
// In a namespace, or in one workflow of it, what who holds is what package access resolves from
// the grants of who and its groups there, and nothing more for an administrator: "an administrator
// holds no implicit run:read_data", and reading another namespace's payloads means granting
// themselves access first, which is audited.
//
// The bootstrap operator, while the bootstrap token has not ended, is an administrator, and holds
// owner in every namespace, which is everything the v0.2 operator did: an installation upgraded
// from v0.2 goes on as it did until its first administrator has enrolled, and the runs its operator
// started stay readable. Once it has ended, it holds nothing.
//
// A suspended user holds nothing either, whatever their grants, since "a suspended account opens no
// session", and neither does anybody who is not a user now, a login removed since included.
func (p *Principals) Allow(ctx context.Context, who Principal, what Permission, over Target) (bool, error) {
	if who == "" || !what.Valid() || !nameable(over) {
		return false, nil
	}
	now := p.now()
	principal, admin, bootstrapped, err := p.resolve(ctx, who)
	switch {
	case err != nil:
		return false, err
	case who == BootstrapOperator:
		if over.Namespace == "" {
			return bootstrapped && what == GrantManage, nil
		}
		return bootstrapped && access.Owner.Permissions().Has(what), nil
	case principal.Ref == "":
		return false, nil
	case over.Namespace == "":
		return admin && what == GrantManage, nil
	}

	var grants []access.Grant
	err = p.pool.In(ctx, over.Namespace, func(ctx context.Context, n *db.NS) error {
		var err error
		grants, err = n.AccessGrantsFor(ctx, principal, over.Workflow, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return access.Holds(principal, grants, what, access.Scope{Namespace: over.Namespace, Workflow: over.Workflow}, now)
}

// HeldIn is the router's Holdings: the namespaces who holds a grant in, its own or one of its
// groups', on the namespace or on a workflow of it, not expired, ordered by name. It is what a
// namespace's record is shown to besides an administrator, "a principal holding a grant in it".
//
// A grant here is one carrying a role. A deny gives nothing, and a namespace where who holds
// nothing but denies is one it can do nothing in, which it is answered as one it cannot see; a deny
// beside a role takes nothing from the record either, since a deny names one permission and reading
// the record is none of them. The bootstrap operator holds no grant, and sees every namespace as the
// administrator it is while it has not ended; a suspended user, and a login removed since, hold none.
func (p *Principals) HeldIn(ctx context.Context, who Principal) ([]string, error) {
	if who == "" || who == BootstrapOperator {
		return nil, nil
	}
	principal, _, _, err := p.resolve(ctx, who)
	if err != nil || principal.Ref == "" {
		return nil, err
	}
	now := p.now()
	var grants []access.Grant
	err = p.pool.Installation(ctx, db.Authorisation, func(ctx context.Context, w *db.Wide) error {
		var err error
		grants, err = w.AccessGrantsAcross(ctx, principal, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	var held []string
	for _, g := range grants {
		if g.Role != "" && !slices.Contains(held, g.Scope.Namespace) {
			held = append(held, g.Scope.Namespace)
		}
	}
	slices.Sort(held)
	return held, nil
}

// resolve reads who who is now: the principal its grants are asked for, with its groups, whether
// it administers the installation, and, for the bootstrap operator, whether the bootstrap token has
// not ended. A principal holding nothing, a suspended user or a login removed since, is answered
// with an empty Ref.
func (p *Principals) resolve(ctx context.Context, who Principal) (principal access.Principal, admin, bootstrapped bool, err error) {
	principal = access.Principal{Ref: string(who)}
	err = p.pool.Installation(ctx, db.Identity, func(ctx context.Context, w *db.Wide) error {
		if who == BootstrapOperator {
			bootstrap, err := w.Bootstrap(ctx)
			bootstrapped = err == nil && !bootstrap.Ended()
			return err
		}
		if strings.Contains(string(who), "/") {
			// A service account, NS/NAME, which administers nothing and belongs to no
			// group: a group's members are logins.
			return nil
		}
		user, err := w.User(ctx, string(who))
		if errors.Is(err, db.ErrNoPrincipal) {
			principal.Ref = ""
			return nil
		}
		if err != nil {
			return err
		}
		if user.Suspended {
			principal.Ref = ""
			return nil
		}
		admin = user.Admin
		principal.Groups, err = w.GroupsOf(ctx, user.Login)
		return err
	})
	return principal, admin, bootstrapped, err
}

// nameable says whether a grant could name the target: the installation, or a namespace and a
// workflow on the grammars a grant's scope is written on. One that no grant could name holds
// nothing for anybody, the bootstrap operator included, and is answered so before the database is
// asked about it: a path naming a namespace such as %ff, which PostgreSQL cannot hold as text, is
// the absence it is rather than a 500 for a question the database refused to be asked.
func nameable(over Target) bool {
	if over.Namespace == "" {
		return over.Workflow == ""
	}
	at := over.Namespace
	if over.Workflow != "" {
		at += "/" + over.Workflow
	}
	_, err := access.ParseScope(at)
	return err == nil
}
