package access

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Principal is who a question is asked about, as grants name it.
type Principal struct {
	// Ref is how a grant names the principal itself: a login, or NS/NAME for a service
	// account.
	Ref string

	// Groups are the groups the principal belongs to, by name. A grant names a group as
	// group:NAME, and applies to every member.
	Groups []string
}

// named says whether a grant naming ref is one of p's own or one of its groups'. Nothing is named
// by the empty string, so a principal read with no name holds no grant written with none.
func (p Principal) named(ref string) bool {
	if ref == "" {
		return false
	}
	if ref == p.Ref {
		return true
	}
	group, ok := strings.CutPrefix(ref, "group:")
	return ok && group != "" && slices.Contains(p.Groups, group)
}

// Resolve is what p holds at one scope as of now, given the grants that may apply to it.
//
// "Effective permissions are the union of every applying grant: the principal's own and its
// groups', at both scopes. A workflow-scope grant only adds. Only an explicit deny removes, and it
// wins over any allow at any scope." So a grant applies when it names p or one of its groups, its
// scope covers at, and it has not expired; the allows among them add their roles' permissions up,
// and every permission a deny among them names is taken away, whichever scope gave it. A question
// about a namespace is answered by the namespace's grants alone, and one about a workflow by the
// namespace's and that workflow's.
//
// secret:use and secret:write "count at namespace scope only, since a declaration serves every
// workflow of the namespace", so a grant on one workflow never gives them, whatever its role. A
// deny of either on a workflow still takes it away there, since a deny wins at any scope.
//
// grants may hold rows that do not apply, which are passed over: the caller may hand over every
// grant of a namespace rather than work out which ones are p's. A row that applies and cannot be
// read, neither one allow nor one deny, or naming a role or a permission that does not exist, is
// an error rather than a row passed over, because a deny nobody can read would otherwise deny
// nothing, and the question it was written to answer cannot be answered without it.
//
// at is the installation when it names no namespace, and the installation resolves to nothing:
// no grant is scoped to it. now is never the zero time, which is before every expiry and would
// bring back every grant that has lapsed; a caller that read no time is refused rather than
// answered.
func Resolve(p Principal, grants []Grant, at Scope, now time.Time) (Set, error) {
	if now.IsZero() {
		return Set{}, errors.New("grants are resolved as of a time, and none was given: every grant that has lapsed would hold again at the zero time")
	}
	if at.Namespace == "" {
		return Set{}, nil
	}
	var allowed, denied Set
	for _, g := range grants {
		if !p.named(g.Principal) || !g.Scope.covers(at) || g.Expired(now) {
			continue
		}
		if err := g.binds(); err != nil {
			return Set{}, fmt.Errorf("grant %s to %s on %s cannot be read: %w", g.ID, g.Principal, g.Scope, err)
		}
		if g.Deny != "" {
			denied = denied.union(SetOf(g.Deny))
			continue
		}
		gives := g.Role.Permissions()
		if g.Scope.Workflow != "" {
			gives = gives.without(namespaceOnly)
		}
		allowed = allowed.union(gives)
	}
	return allowed.without(denied), nil
}

// Holds says whether p holds one permission at one scope as of now, as Resolve answers it.
func Holds(p Principal, grants []Grant, what Permission, at Scope, now time.Time) (bool, error) {
	held, err := Resolve(p, grants, at, now)
	if err != nil {
		return false, err
	}
	return held.Has(what), nil
}

// Gives says whether g, were it live, would give p what at at: an allow written for p or one of its
// groups, at a scope covering at, of a role holding what, and not one of the permissions a grant on
// one workflow never gives. Its expiry is not read, and neither is whether it was revoked since.
//
// It is for saying why p does not hold what, once Holds has answered that it does not: "a trigger
// armed months ago can fire long after the grant that armed it", and the reason a run is refused
// names the grant that lapsed, which is the one that gave it, found among those that have ended by
// their expiry or been revoked. It is Resolve's rule for one allow, and a test holds the two to one
// answer.
func (g Grant) Gives(p Principal, what Permission, at Scope) bool {
	if g.Role == "" || g.Deny != "" || at.Namespace == "" || !p.named(g.Principal) || !g.Scope.covers(at) {
		return false
	}
	gives := g.Role.Permissions()
	if g.Scope.Workflow != "" {
		gives = gives.without(namespaceOnly)
	}
	return gives.Has(what)
}

// Takes says whether g denies p what at at: a deny of what written for p or one of its groups, at a
// scope covering at. Its expiry is not read either, and a deny past it takes nothing away, which
// Expired answers. It is Resolve's rule for one deny, held to it as Gives is.
func (g Grant) Takes(p Principal, what Permission, at Scope) bool {
	return g.Role == "" && g.Deny != "" && g.Deny == what && at.Namespace != "" && p.named(g.Principal) && g.Scope.covers(at)
}

// Owns says whether p owns a namespace as of now: "one on which it holds the owner role, by a grant
// of its own or of one of its groups, on the namespace rather than on one of its workflows". A
// grant on one workflow owns nothing, and a grant past its expiry nothing either. A deny beside the
// role leaves it owning the namespace, since a deny names one permission and never a role, and what
// it takes away is taken wherever that permission is asked.
func Owns(p Principal, grants []Grant, namespace string, now time.Time) bool {
	if namespace == "" {
		return false
	}
	return slices.ContainsFunc(grants, func(g Grant) bool {
		return g.Role == Owner && g.Scope == Scope{Namespace: namespace} && p.named(g.Principal) && !g.Expired(now)
	})
}
