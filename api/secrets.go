package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
// Nothing here holds a Secrets. The routes answer where a value is kept and never read one, and
// the one route that does read a value is the redemption, which only a runner holding a task's
// grant reaches. A test walks every route with a store that fails if anything else asks it.

// Providers are the stores a declaration may name, as the declaration spells them: the built-in
// encrypted store, the API's environment for development, and HashiCorp Vault.
//
// Whether this installation has configured the one a declaration names is a question for the
// moment a value is read, and not for the declaration: a namespace may declare its secrets before
// the store that will hold them is wired in.
var Providers = []string{"builtin", "env", "vault"}

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

// Declare is what a PUT carries: which store holds the value, and where in it.
//
// Decoded closed, so a body carrying anything else is refused rather than half understood, and a
// value above all: accepted and dropped, it would tell whoever sent it that the value had been
// kept somewhere.
type Declare struct {
	Provider string `json:"provider"`
	Path     string `json:"path,omitempty"`
}

// declareMaxBytes is how large a PUT body may be. A declaration is a provider and a path, and a
// body many times larger than any path a store uses is not a declaration.
const declareMaxBytes = 8 << 10

// secretName is the grammar a secret is named on, which is the one every name of the workflow file
// is written on: the workflow names the secret by it and a step mounts it by it, so a declaration
// under a name the file cannot write is a declaration nothing can use.
var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// secretsDir is where a step is given its secrets, "/agk/secrets/<name>", written here rather
// than imported from the driver so that the API links no part of what runs a container.
const secretsDir = "/agk/secrets/"

// DeclarationOptions are what the declaration routes are given.
type DeclarationOptions struct {
	Pool *db.Pool

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time
}

// DeclarationAPI serves a namespace's secret declarations.
type DeclarationAPI struct {
	pool *db.Pool
	now  func() time.Time
}

// NewDeclarations registers the declaration routes on a router.
//
// A constructor of its own rather than routes of the Server, because what it is given is the
// whole of what it may reach: a database, and no secret store. Whoever wires an installation
// together can read that off the call.
func NewDeclarations(rt *Router, o DeclarationOptions) (*DeclarationAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a declaration is a row")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &DeclarationAPI{pool: o.Pool, now: o.Now}

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
	if !secretName.MatchString(name) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("%q is not a secret name: a secret is named the way the workflow file names it, letters, digits, hyphens and underscores beginning with a letter or a digit, because a workflow names it by that and a step mounts it at /agk/secrets/<name>", name))
		return
	}
	var d Declare
	if err := readAtMost(r, &d, declareMaxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a declaration is a provider and a path, and this body is larger than %d bytes", declareMaxBytes))
			return
		}
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := checkDeclare(d); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	declared := db.Declaration{
		Name: name, Provider: d.Provider, Path: d.Path,
		DeclaredBy: string(who), DeclaredAt: s.now(),
	}
	var created bool
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		created, err = ns.Declare(ctx, declared)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
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
		return ns.Undeclare(ctx, name)
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

// checkDeclare refuses a declaration that names no store this installation knows, or that says
// where its value is in a way that store cannot read.
func checkDeclare(d Declare) error {
	if !slices.Contains(Providers, d.Provider) {
		return fmt.Errorf("%q is not a store a secret can be kept in: a declaration names builtin, the encrypted store, env, the API's environment for development, or vault", d.Provider)
	}
	if d.Provider == "builtin" {
		if d.Path != "" {
			return errors.New("the built-in store keeps a value under the namespace and the name it is declared by, so a declaration naming it carries no path: one would name something that store does not have")
		}
		return nil
	}
	if d.Path == "" {
		return fmt.Errorf("a secret kept in %s is read at a path, and this declaration gives none", d.Provider)
	}
	// Answered in every listing, and so printed in a terminal and in a Terraform plan, where a
	// control character is an escape sequence rather than part of a path.
	if !utf8.ValidString(d.Path) || strings.ContainsFunc(d.Path, unicode.IsControl) {
		return fmt.Errorf("the path %q holds a control character or a byte that is not UTF-8, and a path is text a person reads", d.Path)
	}
	return nil
}

// answered is a declaration as the routes answer it, with the mount its name puts it at.
func answered(d db.Declaration) Declaration {
	return Declaration{
		Name: d.Name, Provider: d.Provider, Path: d.Path,
		Mount:      secretsDir + d.Name,
		DeclaredBy: d.DeclaredBy, DeclaredAt: d.DeclaredAt,
	}
}
