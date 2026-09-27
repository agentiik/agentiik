package access

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// TokenScope is what an API token is narrowed to: the wire's apiToken.scope.
//
// "A token's optional scope is {permissions, within}, at least one of the two present: permissions
// the permissions it keeps, within the namespaces and workflows it reaches, written as a grant's
// scope is. On every request its rights are the principal's intersected with both, so a scope can
// only narrow, and naming a permission the principal lacks grants nothing." So a scope is asked
// after Resolve, never instead of it: it answers whether a token keeps what its principal holds,
// and holds nothing of its own.
//
// The zero TokenScope narrows nothing, which is what a token minted with no scope is, and what a
// credential other than a token is.
type TokenScope struct {
	// Permissions are the permissions the token keeps, and nil keeps every one.
	Permissions []Permission

	// Within are the namespaces and workflows the token reaches, and nil reaches everywhere
	// its principal does.
	Within []Scope
}

// Narrows says whether the scope narrows anything: whether a token of it keeps less than its
// principal holds somewhere. A token that does is refused what reaches past the nine, as minting
// another token does, since "a scope can only narrow" and a credential it made would not be
// narrowed by it.
func (s TokenScope) Narrows() bool { return s.Permissions != nil || s.Within != nil }

// TokenHolder refuses a principal no token can be minted for, on the wire's apiToken.principal: a
// login, on the namespace grammar and none of its reserved words since each user's personal
// namespace is named after it, and never operator or installation; or NS/NAME for a service
// account, both halves on the namespace grammar. Never a group, which "holds no credential of its
// own": a group's token would be a way in that belongs to nobody.
//
// A reserved word in either half of a service account is left to whoever looks it up, as the wire
// leaves it: a reserved word never names a namespace, so the reference names nobody, which is what
// it is answered as.
func TokenHolder(ref string) error {
	if strings.HasPrefix(ref, "group:") {
		return fmt.Errorf("%.64q is a group, which holds no token of its own: a token belongs to a user or to a service account, written NS/NAME", ref)
	}
	if ns, name, ok := strings.Cut(ref, "/"); ok {
		for _, half := range []string{ns, name} {
			if len(half) > agk.IdentifierMaxBytes || !namespaceForm.MatchString(half) {
				return fmt.Errorf("%.64q names no service account: one is written NS/NAME, both in lowercase words joined by hyphens, such as finance/nightly-sync", ref)
			}
		}
		return nil
	}
	if ref == "installation" {
		return errors.New("installation names the installation itself, as the author of the pool default, and holds no token")
	}
	return principalRef(ref)
}

// ParseTokenScope reads a scope as the store keeps it, each half as the wire writes it, and nil
// for a half that is absent. A permission or a scope that does not read is an error rather than a
// narrowing passed over, since a token whose narrowing cannot be read would otherwise reach more
// than it was minted to.
func ParseTokenScope(permissions, within []string) (TokenScope, error) {
	var s TokenScope
	if permissions != nil {
		s.Permissions = []Permission{}
		for _, p := range permissions {
			if !Permission(p).Valid() {
				return TokenScope{}, fmt.Errorf("%.64q is not a permission, and a token's scope keeps some of the nine", p)
			}
			s.Permissions = append(s.Permissions, Permission(p))
		}
	}
	if within != nil {
		s.Within = []Scope{}
		for _, w := range within {
			at, err := ParseScope(w)
			if err != nil {
				return TokenScope{}, fmt.Errorf("a token's scope reaches %.64q, which does not read: %w", w, err)
			}
			s.Within = append(s.Within, at)
		}
	}
	return s, nil
}

// Keeps says whether a token of this scope keeps one permission at one scope: whether the
// permission is one it keeps and the scope one it reaches. A namespace it reaches reaches every
// workflow of it, as a grant on the namespace does; a workflow it reaches is not its namespace,
// so a question about the namespace is outside it.
//
// The installation, the zero Scope, is kept by a token with no scope alone: "an administrator's
// powers pass through a token only where its scope does not narrow them away. A token with no scope
// carries them. A within takes them away, since it names namespaces and workflows and administering
// is the installation's. A permissions list keeps only the permissions it names, and administering
// is none of the nine. So a token an administrator mints to run one workflow never creates a user."
func (s TokenScope) Keeps(what Permission, at Scope) bool {
	if at.Namespace == "" {
		return s.Permissions == nil && s.Within == nil
	}
	if s.Permissions != nil && !slices.Contains(s.Permissions, what) {
		return false
	}
	if s.Within == nil {
		return true
	}
	return slices.ContainsFunc(s.Within, func(w Scope) bool { return w.covers(at) })
}

// Reaches says whether a token of this scope reaches anything in one namespace: the namespace
// itself, or a workflow of it. It is what a namespace's record is read through, to an administrator
// and to "a principal holding a grant in it": reading it is none of the nine, so the permissions a
// scope keeps do not narrow it, and a within naming one workflow of the namespace reaches its
// record, since the workflow cannot be reached without it.
func (s TokenScope) Reaches(namespace string) bool {
	if namespace == "" {
		return false
	}
	if s.Within == nil {
		return true
	}
	return slices.ContainsFunc(s.Within, func(w Scope) bool { return w.Namespace == namespace })
}
