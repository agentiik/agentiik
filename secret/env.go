package secret

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The API's own environment, as a store, for development.
//
// "API environment variables for development." It is a store with nothing sealed and nothing
// namespaced about it: every variable of the process is in one flat list, the database's address
// among them. So it is read only where the installation has said, in its own configuration, that
// a namespace may use it and under which prefix, and the prefix is held again at every read, since
// the configuration may have changed since the declaration was taken. That opt-in is what makes it
// for development: an installation that has not written one reads nothing from its environment,
// whatever a declaration says.

// Env reads a value out of the API's environment, at a variable under the prefix the installation
// gives the namespace.
type Env struct {
	environment api.Environment
	lookup      func(string) (string, bool)
}

// NewEnv reads the variables of the namespaces the environment gives a prefix, through lookup,
// which is os.LookupEnv when nil.
func NewEnv(environment api.Environment, lookup func(string) (string, bool)) (*Env, error) {
	if len(environment) == 0 {
		return nil, errors.New("secret: the environment gives no namespace a prefix, and env is read only where the installation has opted a namespace in")
	}
	if err := environment.Check(); err != nil {
		return nil, err
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &Env{environment: maps.Clone(environment), lookup: lookup}, nil
}

// Read answers the variable the declaration names, once it is held to the namespace's prefix.
func (e *Env) Read(_ context.Context, namespace string, d db.Declaration) ([]byte, error) {
	if d.Provider != api.ProviderEnv {
		return nil, fmt.Errorf("secret: %s/%s is declared in %s, and the environment reads only a secret declared in env", namespace, d.Name, d.Provider)
	}
	if err := e.environment.Confines(namespace, d.Path); err != nil {
		return nil, fmt.Errorf("secret: %s/%s is not read: %w", namespace, d.Name, err)
	}
	value, set := e.lookup(d.Path)
	switch {
	case !set:
		return nil, fmt.Errorf("secret: %s/%s is read from %s, which the API's environment does not set: %w", namespace, d.Name, d.Path, api.ErrNoSecret)
	case value == "":
		return nil, fmt.Errorf("secret: %s/%s is read from %s, which the API's environment sets to nothing, and a step would be given an empty file where it expects a credential", namespace, d.Name, d.Path)
	}
	return []byte(value), nil
}
