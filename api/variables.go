package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/schema"
)

// A namespace's variables.
//
// "A namespace keeps variables of its own beside those a workflow file writes", each a name, a JSON
// value and a visibility: all, read by every workflow of the namespace, or selected, read by the
// workflows it names. A run reads those shown to its workflow once, when it is created, on the one
// path every run takes (package trigger), a name its file writes taking the file's value, and keeps
// them, so that a controller taking it over and a replay read what it read. These routes keep the
// variables themselves.
//
// Reading takes workflow:read at namespace scope, as reading the secret declarations does: a
// variable serves every workflow it is shown to, and a grant on one workflow reads none. Writing
// takes workflow:write there, since a variable changes what the workflows reading it do, as pushing
// a version does; no permission of its own, since a tenth would change every role and every grant
// for a setting that does what a push already does.
//
// Not a secret: a value is answered to whoever may read it and kept in clear. One variable per
// request, GET, PUT and DELETE on /api/v1/{ns}/variables/{name}, beside the listing, so that two
// Terraform applies each setting their own never drop each other's change.

// NamespaceVariable is one variable as the routes answer it, wire.schema.json's namespaceVariable.
type NamespaceVariable struct {
	Name string `json:"name"`

	// Value is the value as JSON, every number as it was written but for an exponent.
	Value json.RawMessage `json:"value"`

	// Visibility is all or selected, and Workflows the workflows a selected one names, sorted, []
	// for none, and left out for all.
	Visibility string   `json:"visibility"`
	Workflows  []string `json:"workflows,omitzero"`

	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VariableWrite is what a PUT carries, openapi.json's variableWrite: the whole variable, its value,
// its visibility and for selected its workflows, each written every time.
//
// Every time, rather than a field left out keeping what it held or taking a default: a PUT changing
// the value alone would otherwise leave the visibility to a default, which could widen a variable
// selected for one workflow to every workflow of the namespace, or the list to one, which could
// empty it. Decoded closed, as every body is.
type VariableWrite struct {
	// Value is any JSON value, null among them, which is a value and not its absence.
	Value json.RawMessage `json:"value"`

	Visibility string `json:"visibility"`

	// Workflows is nil where the body leaves it out or writes null, and empty where it writes [].
	Workflows []string `json:"workflows,omitzero"`

	// valued says the body wrote a value, null included, and values how many values it holds,
	// counted as the body was read.
	valued bool
	values int
}

func (v *VariableWrite) field(b *body, name string) error {
	switch name {
	case "value":
		raw, err := b.document(variableMaxValues, fmt.Sprintf("the value holds more than the %d values a namespace's variables may hold in all, as many as a run's inputs may: a run's data belongs in an artifact", variableMaxValues))
		if err != nil {
			return err
		}
		v.Value, v.valued, v.values = json.RawMessage(raw), true, variableMaxValues-b.values
		return nil
	case "visibility":
		return text(b, &v.Visibility)
	case "workflows":
		return texts(b, &v.Workflows, namesMax, fmt.Sprintf("workflows names more than the %d workflows a list of names in a body may", namesMax))
	}
	return unknown(name)
}

// variableMaxBytes is how large a PUT body may be: a value of variableValueMaxBytes, written with
// whatever room a person leaves around it, and namesMax workflows of the longest name a workflow
// may have, each quoted, with the JSON around them.
const variableMaxBytes = 512 << 10

// variableValueMaxBytes is the largest value a variable holds, written as compact JSON. A variable
// is a setting, read at every decision on every run that read it, and sixty-four kibibytes is far
// more than an endpoint, a threshold or a table of a few hundred entries takes; data belongs in an
// artifact.
const variableValueMaxBytes = 64 << 10

// variableMaxValues is how many values one value may hold, which is what a namespace's variables may
// hold in all: a value past it is past what the namespace could take, whatever else it holds.
const variableMaxValues = db.VariablesMaxValues

// VariableOptions are what the variable routes are given.
type VariableOptions struct {
	Pool *db.Pool

	// Now is the clock, an argument so that a test has one.
	Now func() time.Time
}

// VariableAPI serves a namespace's variables.
type VariableAPI struct {
	pool *db.Pool
	now  func() time.Time
}

// NewVariables registers the variable routes on a router.
//
// A constructor of its own, as the secret declarations have, because what it is given is the whole
// of what it may reach: the database, and nothing that runs anything.
func NewVariables(rt *Router, o VariableOptions) (*VariableAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a variable is a row")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &VariableAPI{pool: o.Pool, now: o.Now}

	reading := Needs{Permission: WorkflowRead, Scope: Namespace}
	writing := Needs{Permission: WorkflowWrite, Scope: Namespace}
	for _, r := range []struct {
		method  string
		pattern string
		guard   Guard
		handler Handler
	}{
		{"GET", "/api/v1/{namespace}/variables", reading, s.list},
		{"GET", "/api/v1/{namespace}/variables/{name}", reading, s.one},
		{"PUT", "/api/v1/{namespace}/variables/{name}", writing, s.set},
		{"DELETE", "/api/v1/{namespace}/variables/{name}", writing, s.remove},
	} {
		if err := rt.Handle(r.method, r.pattern, r.guard, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *VariableAPI) list(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var found []db.Variable
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		found, err = ns.Variables(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the variables could not be read")
		return
	}
	out := make([]NamespaceVariable, 0, len(found))
	for _, v := range found {
		out = append(out, answeredVariable(v))
	}
	write(w, http.StatusOK, map[string]any{"variables": out})
}

func (s *VariableAPI) one(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	var found db.Variable
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		found, err = ns.Variable(ctx, r.PathValue("name"))
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoVariable):
		// The same answer an inaccessible namespace gets, since a variable another namespace
		// holds is one this caller may not learn the existence of.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the variable could not be read")
		return
	}
	write(w, http.StatusOK, answeredVariable(found))
}

func (s *VariableAPI) set(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	name := r.PathValue("name")
	if err := checkVariableName(name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var v VariableWrite
	if err := readAtMost(r, &v, variableMaxBytes); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a variable is a value of at most %d bytes and the workflows that read it, and this body is larger than %d", variableValueMaxBytes, variableMaxBytes))
			return
		}
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := v.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := compactValue(v.Value)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(value) > variableValueMaxBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a variable's value is at most %d bytes written as compact JSON, and this one is %d: a variable is a setting, read at every decision on every run that read it, and a run's data belongs in an artifact", variableValueMaxBytes, len(value)))
		return
	}

	written := db.Variable{
		Name: name, Value: value, Bytes: len(value), Values: v.values,
		Visibility: v.Visibility, Workflows: v.Workflows,
		UpdatedBy: string(who), UpdatedAt: s.now(),
	}
	sum := sha256.Sum256(value)
	var created bool
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if written, created, err = ns.SetVariable(ctx, written); err != nil {
			return err
		}
		// The SHA-256 of the value and never the value: every run that read it keeps it, and an
		// entry of up to 64 KiB would make the log where values are read.
		detail := map[string]any{"visibility": written.Visibility, "created": created, "sha256": hex.EncodeToString(sum[:])}
		if written.Visibility == db.VisibilitySelected {
			detail["workflows"] = written.Workflows
		}
		return ns.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.VariableWrite, Target: name, Result: audit.Done, Detail: detail,
		})
	})
	var full *db.VariablesFull
	switch {
	case errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.As(err, &full):
		fail(w, http.StatusConflict, full.Error()+": nothing was written")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the variable could not be written")
		return
	}
	if created {
		w.Header().Set("Location", fmt.Sprintf("/api/v1/%s/variables/%s", over.Namespace, name))
		write(w, http.StatusCreated, answeredVariable(written))
		return
	}
	write(w, http.StatusOK, answeredVariable(written))
}

func (s *VariableAPI) remove(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	name := r.PathValue("name")
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		if err := ns.RemoveVariable(ctx, name); err != nil {
			return err
		}
		return ns.Audit(ctx, audit.Record{Actor: string(who), Action: audit.VariableDelete, Target: name, Result: audit.Done})
	})
	switch {
	case errors.Is(err, db.ErrNoVariable):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the variable could not be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkVariableName refuses a name a workflow file's vars could not write, which an expression could
// not read as vars.<name>: the grammar every name of the file is written on, and at most as long as
// any of them.
func checkVariableName(name string) error {
	if len(name) > agk.IdentifierMaxBytes {
		return fmt.Errorf("a variable's name is at most %d characters and this one is %d, as every name a workflow file writes", agk.IdentifierMaxBytes, len(name))
	}
	if !workflowName.MatchString(name) {
		return fmt.Errorf("%.64q is not a variable's name: a variable is named the way a workflow file's vars names its keys, letters, digits, hyphens and underscores beginning with a letter or a digit, since an expression reads it as vars.<name>", name)
	}
	return nil
}

// check refuses a variable written in part: one with no value, no visibility or one that is neither
// of the two, selected naming no workflows or all naming some, and a workflow named off the grammar
// a workflow is named on, or twice.
func (v *VariableWrite) check() error {
	if !v.valued {
		return errors.New("the variable carries no value: a variable is written whole, and null is a value where none is meant")
	}
	switch v.Visibility {
	case "":
		return errors.New("the variable carries no visibility: it is all or selected, written every time, so that a PUT changing the value alone never widens a variable selected for one workflow to every workflow of the namespace")
	case db.VisibilityAll:
		if v.Workflows != nil {
			return errors.New("a variable every workflow reads names no workflows: workflows is written beside selected alone")
		}
		return nil
	case db.VisibilitySelected:
	default:
		return fmt.Errorf("%.32q is not a visibility: a variable is shown to all the namespace's workflows or to those it has selected", v.Visibility)
	}
	if v.Workflows == nil {
		return errors.New("a variable selected for some workflows names them every time, [] for none, so that a PUT forgetting the list does not empty it")
	}
	for _, workflow := range v.Workflows {
		if len(workflow) > agk.IdentifierMaxBytes || !workflowName.MatchString(workflow) {
			return fmt.Errorf("%.64q is not a workflow's name: a workflow is named on the grammar every name of the file is written on, letters, digits, hyphens and underscores beginning with a letter or a digit, at most %d characters", workflow, agk.IdentifierMaxBytes)
		}
	}
	sorted := slices.Sorted(slices.Values(v.Workflows))
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return twice("the workflow", sorted[i])
		}
	}
	return nil
}

// compactValue is a value as a variable keeps it: decoded with every number as it was written,
// each number written with an exponent written out with a point, schema.Canonical's, so that 1e3
// reads back the double it is rather than the int jsonb would write, and written again as compact
// JSON, its keys sorted, and nothing escaped that need not be. That is what a value is measured in
// and what its SHA-256 is taken over, the same however its writer spaced it.
func compactValue(raw json.RawMessage) (json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		// Read already, as one JSON value, so this is a value the body reader let through and
		// should not have.
		return nil, errors.New("the value is not one JSON value")
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if err := e.Encode(schema.Canonical(value)); err != nil {
		return nil, fmt.Errorf("the value could not be written as JSON: %v", err)
	}
	return json.RawMessage(bytes.TrimSuffix(out.Bytes(), []byte("\n"))), nil
}

// answeredVariable is a variable as the routes answer it.
func answeredVariable(v db.Variable) NamespaceVariable {
	out := NamespaceVariable{
		Name: v.Name, Value: v.Value, Visibility: v.Visibility,
		UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt,
	}
	if v.Visibility == db.VisibilitySelected {
		out.Workflows = v.Workflows
		if out.Workflows == nil {
			out.Workflows = []string{}
		}
	}
	return out
}
