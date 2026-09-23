package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/db"
)

// Where a namespace's secrets live, and never what they are.
//
// "Secret declarations: provider and path of each value. Never the values, which no route reads."
// A workflow names the secrets it uses and nothing more, so what a name resolves to is declared
// here, on the namespace, by a principal holding secret:write, and read by anybody holding
// workflow:read, which is what reading the workflows that name them already takes.
//
// One secret per request, GET, PUT and DELETE on /api/v1/{ns}/secrets/{name}, beside the listing.
// A PUT of the whole set would let two Terraform applies each declaring their own secret drop each
// other's change without either of them seeing it happen; one row per request cannot.
//
// A value goes one way only. "A value reaches the built-in store write-only, on the declaration's
// PUT, for provider builtin: the request carries it once, no answer ever returns it, and rotating
// is writing again, never read-then-write." So a PUT may carry one for builtin, handed to Values
// in the transaction the declaration is written in, and nothing here answers a value or reads one.
//
// Nothing here holds a Secrets. The routes answer where a value is kept and never read one, and
// the one route that does read a value is the redemption, which only a runner holding a task's
// grant reaches. A test walks every route with a store that fails if anything else asks it.

// The stores a declaration may name, by the identifiers the routes, the secret_declarations table
// and agentiik_secret share. Naming one is not being given it: which of them an installation
// reads is its configuration, and check says what each needs before a declaration is taken.
const (
	// ProviderBuiltin is the built-in store, which keeps a value sealed in the database under
	// the namespace and the name it is declared by.
	ProviderBuiltin = "builtin"

	// ProviderEnv is the API's own environment, for development, read at a variable the
	// declaration names under a prefix the installation gives the namespace.
	ProviderEnv = "env"

	// ProviderVault is HashiCorp Vault, which no installation reads from until its provider
	// arrives.
	ProviderVault = "vault"
)

// Environment opts an installation in to the env provider, which reads a value out of the API's
// own environment and is for development only. It gives each namespace that may use it the
// prefix its variables begin with, and a namespace it does not name may not declare one.
//
// Prefixes the installation writes rather than one derived from the namespace's name, because no
// derivation confines: a variable's name is letters, digits and underscores, a namespace's may
// hold hyphens and underscores too, so AGENTIIK_SECRET_TEAM_OPS_ would be the prefix of both
// team-ops and team_ops, and the prefix of team, AGENTIIK_SECRET_TEAM_, would begin it. Written
// out, two prefixes of which one begins the other are refused when the routes are built, and when
// the reader of the environment is, so no namespace reaches another's variables, and none reaches
// the API's own unless somebody writes a prefix that does.
type Environment map[string]string

// variableName is what an environment variable is named on: letters, digits and underscores, not
// beginning with a digit, which is what a shell or a Compose file can set.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Check refuses an environment that could not confine a namespace: a prefix that is no variable's
// beginning, or two namespaces of which one's prefix begins the other's.
//
// Exported, as Confines is, for the reader of the environment, which is built apart from the
// routes and has to refuse what they refuse.
func (e Environment) Check() error {
	namespaces := make([]string, 0, len(e))
	for namespace := range e {
		namespaces = append(namespaces, namespace)
	}
	slices.Sort(namespaces)
	for _, a := range namespaces {
		if !variableName.MatchString(e[a]) {
			return fmt.Errorf("api: the environment prefix of %s is %q, and a prefix is the beginning of a variable's name: letters, digits and underscores, not beginning with a digit", a, e[a])
		}
		for _, b := range namespaces {
			if a != b && strings.HasPrefix(e[b], e[a]) {
				return fmt.Errorf("api: the environment prefix of %s, %q, begins the prefix of %s, %q, and %s would read %s's variables", a, e[a], b, e[b], a, b)
			}
		}
	}
	return nil
}

// Confines refuses a variable that is not the namespace's own: one not named as a variable is, or
// not beginning with the prefix this installation gives that namespace. A namespace it gives none
// has no variables at all.
//
// It is the check a declaration is held to when it is written, and exported for the reader of the
// environment to hold it to again when it reads a value, since an installation's prefixes may
// have changed since the declaration was accepted.
func (e Environment) Confines(namespace, variable string) error {
	prefix, ok := e[namespace]
	if !ok {
		return fmt.Errorf("env is not a store this installation reads the secrets of %s from: the API's environment is for development, and an installation opts in to it by giving a namespace the prefix its variables begin with", namespace)
	}
	if !variableName.MatchString(variable) {
		return fmt.Errorf("%q is not the name of an environment variable, which is letters, digits and underscores, not beginning with a digit", variable)
	}
	if !strings.HasPrefix(variable, prefix) {
		return fmt.Errorf("%s is not a variable of %s: a secret it keeps in env is a variable beginning with %s, the prefix this installation gives it, so that no namespace reads another's variables or the API's own", variable, namespace, prefix)
	}
	return nil
}

// Declaration is one secret as the routes answer it: its name, where its value is kept, and where
// a step is given it.
type Declaration struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`

	// Path is where the value sits inside its store. The built-in store keeps a value under the
	// namespace and the name it is declared by, so a declaration naming it has none.
	Path string `json:"path,omitempty"`

	// Mount is where a step is given the value, as a file on tmpfs, when its brick's manifest
	// names no mount of its own. It is answered rather than stored because it follows from the
	// name, and it is answered at all because the file a brick reads is what a person looking at
	// a declaration is usually trying to find.
	Mount string `json:"mount"`

	DeclaredBy string    `json:"declared_by"`
	DeclaredAt time.Time `json:"declared_at"`
}

// Declare is what a PUT carries: which store holds the value, where in it, and for the built-in
// store the value itself.
//
// Decoded closed, so a body carrying anything else is refused rather than half understood: a field
// accepted and dropped would tell whoever sent it that something had been kept.
type Declare struct {
	Provider string `json:"provider"`
	Path     string `json:"path,omitempty"`

	// Value is the secret, for builtin alone, and written only: no answer carries it, now or
	// later. A PUT without one leaves the value stored as it was, and rotating is sending one
	// again. A pointer, so that an empty value is told apart from none and refused, rather than
	// taken for a PUT that keeps the old one.
	Value *string `json:"value,omitempty"`

	// Encoding is how Value is written: utf-8 when absent, or base64 for a value that is not
	// text, as a redemption answers it, since JSON carries no arbitrary bytes and a keystore is
	// a secret too.
	Encoding string `json:"encoding,omitempty"`
}

// valueMaxBytes is the largest value a PUT writes, the bound package secret seals to, repeated
// here because this package cannot import that one.
const valueMaxBytes = 1 << 20

// declareMaxBytes is how large a PUT body may be: the largest value, written as base64, and room
// for the declaration around it.
const declareMaxBytes = 2 << 20

// pathMax is how long a path may be. A path is a variable's name, or a key in a store, and a
// kilobyte is more than either needs; bounded because every listing answers it and every
// Terraform plan prints it, and the room a body leaves for a value is not room for a path.
const pathMax = 1 << 10

// secretName is the grammar a secret is named on, which is the one every name of the workflow file
// is written on: the workflow names the secret by it and a step mounts it by it, so a declaration
// under a name the file cannot write is a declaration nothing can use.
var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// secretNameMax is how long a secret's name may be, which the grammar does not bound and a file
// name does: a step is given the value at /agk/secrets/<name>, and the runner names the file it
// binds there after it, so a longer name is a secret nothing could ever mount.
const secretNameMax = 255

// secretsDir is where a step is given its secrets, "/agk/secrets/<name>", written here rather
// than imported from the driver so that the API links no part of what runs a container.
const secretsDir = "/agk/secrets/"

// Values is the built-in store as the declaration routes reach it: a value goes in, and nothing
// comes back out.
//
// There is no method here that reads, and Secrets, which reads, is held by the redemption alone.
// Each method is handed the namespace's transaction, the one the declaration is written in, so
// that a declaration and its value are written together or not at all: a PUT that failed half
// way leaves neither a declaration with no value nor a value nothing declares.
//
// An interface here and filled elsewhere, because sealing is package secret and cmd/agk imports
// this package: the boundary test holds that only the API reaches the store, and the command line
// reaching it through this package would be the command line linking it. secret.Builtin fills it,
// and secret.Attach wires it in beside the reader of the same store, called by the server's own
// main package, which nothing else imports.
type Values interface {
	// Write seals value as the one the namespace's secret of that name holds, replacing
	// whatever it held before.
	Write(ctx context.Context, ns *db.NS, name string, value []byte) error

	// Forget removes the value of that name where the namespace holds one, and is not an error
	// where it holds none. A secret removed, or moved out of the built-in store, takes its value
	// with it, so that declaring the name again later does not bring back a credential somebody
	// meant to be gone.
	Forget(ctx context.Context, ns *db.NS, name string) error
}

// ErrNoStore is a value written to an installation with no built-in store attached.
var ErrNoStore = errors.New("api: this installation has no built-in secret store attached")

// NoValues is an installation with no built-in store attached. It takes no value and holds none,
// so it never has one to forget.
//
// It is the default, for the reason NoSecrets is: a value sent to an installation that cannot keep
// it is refused in front of whoever sent it, rather than taken and lost.
type NoValues struct{}

// Write takes nothing.
func (NoValues) Write(context.Context, *db.NS, string, []byte) error { return ErrNoStore }

// Forget has nothing to forget.
func (NoValues) Forget(context.Context, *db.NS, string) error { return nil }

// DeclarationOptions are what the declaration routes are given.
type DeclarationOptions struct {
	Pool *db.Pool

	// Environment opts the installation in to the env provider, namespace by namespace. Nil, the
	// default, opts in none, so a declaration naming env is refused everywhere until somebody
	// has said, in the installation's own configuration, that this one is for development.
	Environment Environment

	// Values is where a builtin value is written. Nil, the default, is NoValues: a declaration
	// is still taken, and a value is refused.
	Values Values

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time
}

// DeclarationAPI serves a namespace's secret declarations.
type DeclarationAPI struct {
	pool        *db.Pool
	environment Environment
	values      Values
	now         func() time.Time
}

// NewDeclarations registers the declaration routes on a router.
//
// A constructor of its own rather than routes of the Server, because what it is given is the
// whole of what it may reach: a database, and a store it writes a value into and never reads one
// from. Whoever wires an installation together can read that off the call.
func NewDeclarations(rt *Router, o DeclarationOptions) (*DeclarationAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a declaration is a row")
	}
	if err := o.Environment.Check(); err != nil {
		return nil, err
	}
	if o.Values == nil {
		o.Values = NoValues{}
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &DeclarationAPI{pool: o.Pool, environment: maps.Clone(o.Environment), values: o.Values, now: o.Now}

	reading := Needs{Permission: WorkflowRead, Scope: Namespace}
	writing := Needs{Permission: SecretWrite, Scope: Namespace}
	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"GET", "/api/v1/{namespace}/secrets", reading, s.list},
		{"GET", "/api/v1/{namespace}/secrets/{name}", reading, s.one},
		{"PUT", "/api/v1/{namespace}/secrets/{name}", writing, s.declare},
		{"DELETE", "/api/v1/{namespace}/secrets/{name}", writing, s.undeclare},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *DeclarationAPI) list(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var found []db.Declaration
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		found, err = ns.Declarations(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the secret declarations could not be read")
		return
	}
	out := make([]Declaration, 0, len(found))
	for _, d := range found {
		out = append(out, answered(d))
	}
	write(w, http.StatusOK, map[string]any{"secrets": out})
}

func (s *DeclarationAPI) one(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	name := r.PathValue("name")
	var found db.Declaration
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		found, err = ns.Declaration(ctx, name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoDeclaration):
		// The same answer an inaccessible namespace gets, since a declaration another
		// namespace holds is one this caller may not learn the existence of.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the secret declaration could not be read")
		return
	}
	write(w, http.StatusOK, answered(found))
}

func (s *DeclarationAPI) declare(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := r.PathValue("name")
	if len(name) > secretNameMax {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a secret name is at most %d characters and this one is %d: a step is given the value in a file named after it, and no file name is longer", secretNameMax, len(name)))
		return
	}
	if !secretName.MatchString(name) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a secret name: a secret is named the way the workflow file names it, letters, digits, hyphens and underscores beginning with a letter or a digit, because a workflow names it by that and a step mounts it at /agk/secrets/<name>", name))
		return
	}
	var d Declare
	if err := readAtMost(r, &d, declareMaxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a declaration is a provider, a path and a value of at most %d bytes, and this body is larger than %d", valueMaxBytes, declareMaxBytes))
			return
		}
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.check(over.Namespace, d); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := valueOf(d)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(value) > valueMaxBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a secret is at most %d bytes, the most the built-in store seals, and this one is %d", valueMaxBytes, len(value)))
		return
	}

	// The value is written in the declaration's own transaction, so that a store refusing it
	// leaves the declaration as it was. Each write also belongs in the audit log as a secret
	// write; there is no audit log yet, and the task that builds it names this event.
	declared := db.Declaration{
		Name: name, Provider: d.Provider, Path: d.Path,
		DeclaredBy: string(who), DeclaredAt: s.now(),
	}
	var created bool
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if declared, created, err = ns.Declare(ctx, declared); err != nil {
			return err
		}
		switch {
		case value != nil:
			return s.values.Write(ctx, ns, name, value)
		case d.Provider != ProviderBuiltin:
			return s.values.Forget(ctx, ns, name)
		}
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.Is(err, ErrNoStore):
		fail(w, http.StatusServiceUnavailable, "this installation has no built-in secret store attached, and a value has nowhere to go: nothing was written")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the secret declaration could not be written")
		return
	}
	if created {
		w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/secrets/%s", over.Namespace, name))
		write(w, http.StatusCreated, answered(declared))
		return
	}
	write(w, http.StatusOK, answered(declared))
}

func (s *DeclarationAPI) undeclare(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	name := r.PathValue("name")
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.Undeclare(ctx, name); err != nil {
			return err
		}
		return s.values.Forget(ctx, ns, name)
	})
	switch {
	case errors.Is(err, db.ErrNoDeclaration):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the secret declaration could not be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// check refuses a declaration that names no store this installation reads, or says where its
// value is in a way that store cannot read or this namespace may not name.
//
// A declaration names one of three stores: builtin (the encrypted store), env (the API's
// environment, for development) or vault (HashiCorp Vault). Naming one is not being given it. "A
// namespace is confined to its own paths", so a declaration is taken only where the path it gives
// can be held to its namespace when it is written: builtin always, since that store keys a value
// by the namespace and the name and takes no path at all; env where the installation has opted in
// and given the namespace its prefix; and vault nowhere yet, since nothing gives a namespace its
// prefix in Vault until that provider arrives. A path taken now on the promise of a check later
// would be a row every later reader had to distrust.
func (s *DeclarationAPI) check(namespace string, d Declare) error {
	if len(d.Path) > pathMax {
		return fmt.Errorf("a path is at most %d bytes, more than a variable's name or a key in a store needs, and this one is %d", pathMax, len(d.Path))
	}
	switch d.Provider {
	case ProviderBuiltin:
		if d.Path != "" {
			return errors.New("the built-in store keeps a value under the namespace and the name it is declared by, so a declaration naming it carries no path: one would name something that store does not have")
		}
		return nil
	case ProviderEnv:
		if d.Path == "" {
			return errors.New("a secret kept in env is read from a variable, and this declaration names none")
		}
		return s.environment.Confines(namespace, d.Path)
	case ProviderVault:
		return errors.New("vault is not a store this installation reads secrets from: a namespace is confined to its own paths, nothing gives a namespace its prefix in Vault until that provider arrives, and a path taken before then could name any namespace's secret")
	}
	return fmt.Errorf("%q is not a store a secret can be kept in: a declaration names builtin (the encrypted store), env (the API's environment, for development) or vault", d.Provider)
}

// valueOf is the value a declaration carries, as the bytes a step will be given, or nil where it
// carries none.
func valueOf(d Declare) ([]byte, error) {
	if d.Value == nil {
		if d.Encoding != "" {
			return nil, errors.New("an encoding says how a value is written, and this declaration carries no value")
		}
		return nil, nil
	}
	if d.Provider != ProviderBuiltin {
		return nil, fmt.Errorf("a value is written only into the built-in store, and a secret kept in %s is read where %s keeps it, which is where to set it", d.Provider, d.Provider)
	}
	var value []byte
	switch d.Encoding {
	case "", EncodingUTF8:
		value = []byte(*d.Value)
	case EncodingBase64:
		var err error
		// Refused without the decoder's own error, which points at the byte where the value
		// stopped being base64, and a value is not something to point into.
		if value, err = base64.StdEncoding.DecodeString(*d.Value); err != nil {
			return nil, errors.New("the value is not base64, which its encoding says it is")
		}
	default:
		return nil, fmt.Errorf("%q is not an encoding a value is written in: it is utf-8, the default, or base64 for a value that is not text", d.Encoding)
	}
	if len(value) == 0 {
		return nil, errors.New("the value is empty, and a step would be given a file with nothing in it where it expects a credential; a PUT with no value at all keeps the one stored")
	}
	return value, nil
}

// answered is a declaration as the routes answer it, with the mount its name puts it at.
func answered(d db.Declaration) Declaration {
	return Declaration{
		Name: d.Name, Provider: d.Provider, Path: d.Path,
		Mount:      secretsDir + d.Name,
		DeclaredBy: d.DeclaredBy, DeclaredAt: d.DeclaredAt,
	}
}
