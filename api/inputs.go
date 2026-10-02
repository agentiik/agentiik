package api

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/schema"
	"github.com/agentiik/agentiik/trigger"
	"github.com/agentiik/agentiik/version"
)

// A run's inputs, as a request supplies them. They are bound against the declaration of the version
// the run is pinned to by package trigger, on the one path every run is started by, whatever
// started it: "already held to their declared schemas with required and default applied" is what a
// run's inputs are when they are written, and a request the declaration refuses is answered 422
// naming the input, before any run exists.

// decodeInputs reads the inputs a request supplied, as one object of names and values with every
// number a json.Number, which is how agk run --local reads an input and how the controller reads
// the run back. The body reader counted them as it read them, so this costs no more than the
// controller already pays at every decision it takes on the run.
func decodeInputs(sent jsontext.Value) (map[string]any, error) {
	var supplied map[string]any
	if len(sent) > 0 {
		d := json.NewDecoder(bytes.NewReader(sent))
		d.UseNumber()
		if err := d.Decode(&supplied); err != nil {
			// Read already, as one object of names and values, so this is a document the body
			// reader let through and should not have.
			return nil, errors.New("the inputs are not an object naming each input")
		}
	}
	return supplied, nil
}

// refused answers what the one path a run is started by refused, and says whether it did: every
// route starting a run answers a refusal the same way, whatever it started.
func (s *Server) refused(w http.ResponseWriter, over Target, commit string, err error) bool {
	var ref *db.RefUnresolved
	var input *schema.InputRefusal
	var large *trigger.InputsTooLarge
	var reached *db.RunsPerHourReached
	switch {
	case err == nil:
		return false
	case errors.Is(err, trigger.ErrCommitAndRef):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &ref) && ref.Ambiguous:
		fail(w, http.StatusBadRequest, ref.Error())
	case errors.As(err, &ref):
		fail(w, http.StatusNotFound, ref.Error())
	case errors.Is(err, db.ErrNoWorkflow), errors.Is(err, db.ErrNoVersion):
		// The same answer an inaccessible one gets, for the same reason.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
	case errors.Is(err, version.ErrLibrary):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s at %s is a library: its root agentiik.yaml is written as a fragment, which other workflows include and nothing runs", over.Workflow, commit))
	case errors.Is(err, trigger.ErrNoObjectStore):
		fail(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, trigger.ErrDeclarationRefused):
		// The version's own declaration, which a push refuses since this route binds against it,
		// so only a version pushed before then can hold one.
		fail(w, http.StatusUnprocessableEntity, err.Error())
	case errors.As(err, &input):
		refuseInput(w, input)
	case errors.As(err, &large):
		fail(w, http.StatusRequestEntityTooLarge, large.Error())
	case errors.Is(err, db.ErrWorkflowMoving):
		fail(w, http.StatusConflict, movingSentence(over))
	case errors.As(err, &reached):
		// "Past it the API answers 429 with Retry-After, the whole seconds until one more fits." No
		// run exists, so a client told to come back then is not asking for a second one.
		w.Header().Set("Retry-After", strconv.Itoa(reached.Seconds()))
		fail(w, http.StatusTooManyRequests, reached.Reason())
	default:
		// A caller who went away is not trouble for whoever runs the installation.
		if !errors.Is(err, context.Canceled) {
			s.report(fmt.Errorf("api: a run of %s/%s could not be started: %w", over.Namespace, over.Workflow, err))
		}
		fail(w, http.StatusInternalServerError, "the run could not be created")
	}
	return true
}

// refuseInput answers the input a request gets wrong: 422, with the sentence agk run --local
// prints for it, and the input and the rule apart, so that a form can point at the field without
// reading the sentence. The rule is the key a person finds in the workflow file: required,
// schema, or undeclared for a name the workflow does not declare.
func refuseInput(w http.ResponseWriter, r *schema.InputRefusal) {
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusUnprocessableEntity, map[string]string{
		"error": r.Error(), "input": r.Input, "rule": r.Rule,
	})
}

// declaredInput is one input a version declares, as GET /api/v1/{ns}/workflows/{name}/inputs
// answers it: the schema as the file writes it, absent where it declares none, whether a run
// supplying nothing is refused, and the value standing in for one it does not supply.
type declaredInput struct {
	Schema   json.RawMessage `json:"schema,omitempty"`
	Required bool            `json:"required"`
	Default  any             `json:"default,omitempty"`
}

// runInputs answers GET /api/v1/{ns}/workflows/{name}/inputs, under workflow:run: what a manual
// run of the version ref names takes, the default branch's head where it names none. The inputs
// that version declares and the files of its tree their schemas reach, and nothing else of the
// file, since an operator holds workflow:run without workflow:read so that it starts a job
// without seeing the steps, images, queries and endpoints inside it; the inputs are the boundary
// whoever asks for a run has to fill. Read as a run reads them, so that a version a run would be
// refused at is refused here with the same status.
func (s *Server) runInputs(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	declared, err := s.starter.Declared(r.Context(), trigger.Request{
		Namespace: over.Namespace, Workflow: over.Workflow, Ref: r.URL.Query().Get("ref"),
	})
	var ref *db.RefUnresolved
	switch {
	case err == nil:
	case errors.As(err, &ref) && ref.Ambiguous:
		fail(w, http.StatusBadRequest, ref.Error())
		return
	case errors.As(err, &ref):
		fail(w, http.StatusNotFound, ref.Error())
		return
	case errors.Is(err, db.ErrNoWorkflow), errors.Is(err, db.ErrNoVersion):
		// The same answer an inaccessible one gets, for the same reason.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.Is(err, version.ErrLibrary):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("%s at %s is a library: its root agentiik.yaml is written as a fragment, which other workflows include and nothing runs", over.Workflow, declared.Commit))
		return
	case errors.Is(err, trigger.ErrNoObjectStore):
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, trigger.ErrDeclarationRefused):
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	default:
		if !errors.Is(err, context.Canceled) {
			s.report(fmt.Errorf("api: the inputs of %s/%s could not be read: %w", over.Namespace, over.Workflow, err))
		}
		fail(w, http.StatusInternalServerError, "the inputs could not be read")
		return
	}
	inputs := make(map[string]declaredInput, len(declared.Inputs))
	for name, in := range declared.Inputs {
		d := declaredInput{Required: in.Required, Default: in.Default}
		// The reader writes JSON null for a key that was present and empty, which declares no
		// schema, as DeclaredInputs reads it.
		if raw := bytes.TrimSpace(in.Schema); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			d.Schema = raw
		}
		inputs[name] = d
	}
	files := declared.Files
	if files == nil {
		files = map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, map[string]any{"commit": declared.Commit, "inputs": inputs, "files": files})
}
