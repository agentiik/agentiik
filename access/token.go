package access

import (
	"fmt"
	"slices"
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
