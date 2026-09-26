package access

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The two grammars a scope is written on, as the wire's grantScope writes them: the namespace's,
// and a workflow's name, which is the identifier grammar a workflow file's metadata.name is
// written on. A test holds both to the vendored schema, pattern for pattern.
const (
	namespaceGrammar = `[a-z0-9]+(?:-[a-z0-9]+)*`
	workflowGrammar  = `[A-Za-z0-9][A-Za-z0-9_-]*`
)

var (
	namespaceForm = regexp.MustCompile(`^` + namespaceGrammar + `$`)
	workflowForm  = regexp.MustCompile(`^` + workflowGrammar + `$`)
)

// Scope is where a grant applies, and what a question is asked about: a namespace, whose grants
// are "inherited by every workflow in it", or one workflow of it. It is written NS or NS/NAME, as
// the wire writes a grant's scope.
//
// The zero Scope is the installation. No grant names it, and a question about it resolves to
// nothing.
type Scope struct {
	Namespace string
	Workflow  string
}

// ParseScope reads a scope as the wire writes it: finance, or finance/monthly-invoicing.
func ParseScope(s string) (Scope, error) {
	ns, wf, onWorkflow := strings.Cut(s, "/")
	scope := Scope{Namespace: ns, Workflow: wf}
	if onWorkflow && wf == "" {
		return Scope{}, fmt.Errorf("%.64q names no workflow after its slash: a scope is a namespace, such as finance, or one workflow in it, such as finance/monthly-invoicing", s)
	}
	if err := scope.validate(); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

// validate refuses a scope no grant can name: a namespace outside the namespace grammar, one of
// the words the API routes on, which "cannot name a namespace", or a workflow outside the
// identifier grammar. Both halves are bounded as every name is, since no namespace or workflow
// can be called anything longer.
func (s Scope) validate() error {
	switch {
	case s.Namespace == "":
		return errors.New("a grant's scope names a namespace, such as finance, or one workflow in it, such as finance/monthly-invoicing: the installation is not a scope a grant names")
	case len(s.Namespace) > agk.IdentifierMaxBytes || !namespaceForm.MatchString(s.Namespace):
		return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens, such as finance or team-ops", s.Namespace)
	case agk.IsReservedNamespace(s.Namespace):
		return fmt.Errorf("%s is a word the API routes on, which cannot name a namespace, so no grant is scoped to it", s.Namespace)
	case s.Workflow == "":
		return nil
	case len(s.Workflow) > agk.IdentifierMaxBytes || !workflowForm.MatchString(s.Workflow):
		return fmt.Errorf("%.64q is not a workflow's name: a name is letters, digits, hyphens and underscores, beginning with a letter or a digit", s.Workflow)
	}
	return nil
}

// String writes the scope as the wire does, and the installation as the empty string.
func (s Scope) String() string {
	if s.Workflow == "" {
		return s.Namespace
	}
	return s.Namespace + "/" + s.Workflow
}

// MarshalText writes the scope as the wire does, and refuses one no grant can name, so that a
// grant is never written out with a scope that would not read back.
func (s Scope) MarshalText() ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return []byte(s.String()), nil
}

// UnmarshalText reads the scope as ParseScope does.
func (s *Scope) UnmarshalText(b []byte) error {
	parsed, err := ParseScope(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// covers says whether a grant at s applies to a question at q: a namespace's grants to the
// namespace and to every workflow in it, a workflow's grants to that workflow alone. A question
// about the namespace is not one about any of its workflows, so a grant on one of them never
// answers it.
func (s Scope) covers(q Scope) bool {
	return s.Namespace == q.Namespace && (s.Workflow == "" || s.Workflow == q.Workflow)
}

// Grant is one row of what a principal may do: the wire's accessGrant.
//
// "A grant binds one principal, one scope (a namespace or a single workflow) and one role." A
// deny is a grant row too, naming one permission instead of a role, since the documentation's own
// deny takes run:read_data away from an operator and no role is that one permission. A row
// carries exactly one of the two, so that revoking one row never revokes half of another.
type Grant struct {
	ID string `json:"id"`

	// Principal is who the grant is for: a login, group:NAME for a group, or NS/NAME for a
	// service account.
	Principal string `json:"principal"`

	Scope Scope `json:"scope"`

	// Role is the role granted, on an allow, and empty on a deny.
	Role Role `json:"role,omitempty"`

	// Deny is the permission taken away, on a deny, and empty on an allow.
	Deny Permission `json:"deny,omitempty"`

	// ExpiresAt is when the grant ends by itself, and zero for one that lasts until it is
	// revoked.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
}

// Validate refuses a grant that does not say what it binds: no principal, a scope no grant can
// name, neither a role nor a deny or both, a role that is not one of the four, or a deny that is
// not one of the nine. What the row records about itself, its identifier and who wrote it when,
// is the store's to fill and is not read here.
func (g Grant) Validate() error {
	if g.Principal == "" {
		return errors.New("a grant names the principal it is for: a login, group:NAME or NS/NAME")
	}
	if err := g.Scope.validate(); err != nil {
		return err
	}
	return g.binds()
}

// binds refuses a row that is not exactly one allow of a known role or one deny of a known
// permission. It is the part of Validate that Resolve cannot do without: a row whose meaning is
// unclear cannot be added up.
func (g Grant) binds() error {
	switch {
	case g.Role == "" && g.Deny == "":
		return errors.New("a grant binds a role or denies one permission, and this one does neither")
	case g.Role != "" && g.Deny != "":
		return fmt.Errorf("a grant binds a role or denies one permission, and this one does both, %.64q and %.64q: each is a row of its own, so that revoking one never revokes half of the other", g.Role, g.Deny)
	case g.Role != "" && !g.Role.Valid():
		return fmt.Errorf("%.64q is not a role: a role is viewer, operator, editor or owner", g.Role)
	case g.Deny != "" && Role(g.Deny).Valid():
		return fmt.Errorf("a deny names one permission and %s is a role: deny the permissions it holds one row at a time", g.Deny)
	case g.Deny != "" && !g.Deny.Valid():
		return fmt.Errorf("%.64q is not a permission: a deny names one of the nine, such as run:read_data", g.Deny)
	}
	return nil
}

// Expired says whether the grant has ended by itself as of now. An expiry "ends it without anyone
// remembering to revoke it", at the instant it names: a grant ending at midnight is not held at
// midnight.
func (g Grant) Expired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)
}
