package secret

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// Reading a value, from whichever store the namespace keeps it in.
//
// "The namespace declares where a secret lives (provider and path)", and a workflow only names the
// secrets it uses. So a read is two steps: the namespace's declaration of the name says which store
// and where, and that store says what. The first is the same whatever the store and is Providers;
// the second is a Provider, one for each store an installation reads.
//
// A value is bytes from the store to the wire. A secret is a file's worth of bytes and not always
// text, and a provider answering a string would be one that had already decided how a keystore
// gets mangled; the redemption decides how each value travels, and base64 is how one that is not
// text does.
//
// Every refusal here names the namespace and the secret and never carries a value. A refusal is
// read by whoever runs the installation and is written to wherever they keep what went wrong, and
// a value in one would be a value in a log.

// Provider is one store a value is read from.
type Provider interface {
	// Read answers the value a declaration of the namespace points at, as the bytes a step is
	// given. An error names the namespace and the secret, and wraps api.ErrNoSecret where the
	// store holds nothing there.
	Read(ctx context.Context, namespace string, d db.Declaration) ([]byte, error)
}

// Providers reads a namespace's secret through its declaration, from the store the declaration
// names. It fills api.Secrets.
type Providers struct {
	pool *db.Pool
	by   map[string]Provider
}

// NewProviders reads through the stores it is given, each under the identifier a declaration names
// it by. A store it is not given is one this installation does not read, and a declaration naming
// it is refused when it is read, saying which secret.
func NewProviders(pool *db.Pool, by map[string]Provider) (*Providers, error) {
	if pool == nil {
		return nil, errors.New("secret: no database, and a declaration is a row")
	}
	for id, p := range by {
		if !known(id) {
			return nil, fmt.Errorf("secret: %q is not a store a secret can be kept in: a declaration names builtin, env or vault", id)
		}
		if p == nil {
			return nil, fmt.Errorf("secret: %s is given no provider, and a store that is named and not there is one every read of it would fail on", id)
		}
	}
	return &Providers{pool: pool, by: maps.Clone(by)}, nil
}

// Value reads one secret of one namespace.
//
// The declaration is read in a transaction of its own and the store after it, rather than both in
// one, because a store other than the built-in one is somewhere else: a transaction held open
// while Vault answers is a connection the whole API is short of while it does.
func (p *Providers) Value(ctx context.Context, namespace, name string) ([]byte, error) {
	var d db.Declaration
	err := p.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		d, err = ns.Declaration(ctx, name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoDeclaration):
		return nil, fmt.Errorf("secret: %s declares no secret %s: %w", namespace, name, api.ErrNoSecret)
	case err != nil:
		return nil, fmt.Errorf("secret: the declaration of %s/%s could not be read: %w", namespace, name, err)
	}

	provider, ok := p.by[d.Provider]
	switch {
	case !known(d.Provider):
		// The table holds the three identifiers there are, so this is a row written by a
		// later version of Agentiik and read by this one after a downgrade.
		return nil, fmt.Errorf("secret: %s/%s is declared in %q, which is not a store this version of Agentiik reads", namespace, name, d.Provider)
	case !ok:
		return nil, fmt.Errorf("secret: %s/%s is kept in %s, and this installation reads no secret from %s", namespace, name, d.Provider, d.Provider)
	}
	return provider.Read(ctx, namespace, d)
}

// known is whether a store is one a declaration can name.
func known(id string) bool {
	switch id {
	case api.ProviderBuiltin, api.ProviderEnv, api.ProviderVault:
		return true
	}
	return false
}

// Options are the secret half of an installation's configuration.
type Options struct {
	Pool *db.Pool

	// Keys is what the built-in store seals under and opens with. Nil attaches no built-in
	// store: a builtin declaration is still taken, a value sent with one is refused, a value
	// already kept is still forgotten when its declaration goes, and a task naming one fails
	// saying which secret.
	Keys *Keyring

	// Environment opts the installation in to env, namespace by namespace, and is what
	// api.DeclarationOptions.Environment is given. Nil, the default, opts in none, so env is a
	// store nobody reads until the installation's own configuration says it is for development.
	Environment api.Environment

	// Lookup reads one variable of the API's environment, and is os.LookupEnv when nil. An
	// argument so that a test has an environment of its own.
	Lookup func(string) (string, bool)
}

// Attach fills the secret half of the API's options: the store a declaration's value is written
// into, the environment a declaration may name, and what a redemption reads a value through.
//
// It is how the providers reach the API's process, and the only way: package api holds the
// interfaces and none of what fills them, because cmd/agk links it. The server's own main package
// calls this, which puts it on the short list of what may reach the store and keeps the command
// line off it.
//
// One call for both halves, so that they cannot disagree: a declaration the routes take is one the
// redemption can read, and the store a value is written into is the store it is read from. Options
// that already name any of the three are refused, since two stores in one process would be a value
// written into one and read from the other.
func Attach(o Options, declarations *api.DeclarationOptions, runners *api.RunnerOptions) error {
	switch {
	case declarations == nil || runners == nil:
		return errors.New("secret: there are no options to attach the providers to")
	case declarations.Values != nil || declarations.Environment != nil || runners.Secrets != nil:
		return errors.New("secret: the API's options already name a secret store, and two in one process would be a value written into one and read from the other")
	}

	by := map[string]Provider{}
	var values api.Values
	if o.Keys != nil {
		builtin, err := NewBuiltin(o.Pool, o.Keys)
		if err != nil {
			return err
		}
		by[api.ProviderBuiltin], values = builtin, builtin
	}
	if len(o.Environment) > 0 {
		env, err := NewEnv(o.Environment, o.Lookup)
		if err != nil {
			return err
		}
		by[api.ProviderEnv] = env
	}
	providers, err := NewProviders(o.Pool, by)
	if err != nil {
		return err
	}

	declarations.Values = values
	declarations.Environment = maps.Clone(o.Environment)
	runners.Secrets = providers
	return nil
}
