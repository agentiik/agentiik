package api

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/trigger"
	"github.com/agentiik/agentiik/version"
)

// A principal's collections, under /api/v1/me/collections: "a connector of a principal's own: the
// workflows they choose, each one a tool, served at /mcp/collections/{id}, which is the URL a client
// is given".
//
// Every route takes Own, since what it answers is the caller's own: "the principal that made it, a
// user or a service account, alone lists it, changes it and calls through it. Anybody else, an
// administrator included, is answered 404, as for a collection that does not exist." A collection
// grants nothing, so nothing here is audited: every call through one is a run, which is.
//
// What a member offers is read at every answer, never kept: the entry point at the head of the
// member's ref, and whether the caller may run the workflow, which the authorizer is asked about the
// workflow as every route asks it. A member offering nothing says why, "so that a member missing
// from a client's list is never one its owner cannot account for".

// The reasons a member offers no tool, as the wire spells them.
const (
	// reasonNoBlock is a member whose entry point at the head of its ref declares no mcp block.
	reasonNoBlock = "no_mcp_block"
	// reasonNotRunnable is a member whose workflow the owner may no longer run, deleted ones
	// included, "since a workflow one may not run is one whose fate a collection does not tell".
	reasonNotRunnable = "not_runnable"
)

// collectionDescriptionMax is how long a collection's description is: one line a person reads in
// a list, which a paragraph is not.
const collectionDescriptionMax = 280

// collectionMaxBytes is how large a body writing a collection or a member may be: a name, a
// description of 280 characters and a ref, with room for how JSON escapes them.
const collectionMaxBytes = 16 << 10

// CollectionOptions are what the collection routes are built with.
type CollectionOptions struct {
	Pool     *db.Pool
	Versions *version.Store

	// Objects is where a version's tree is read from, for the files an input's schema reaches,
	// which a tool's inputSchema bundles in.
	Objects artifact.Objects

	// PublicURL is the installation's public URL, which a collection's url is made of.
	PublicURL string

	// Limits are what a sync call's output envelope is read back under, and default to the
	// documentation's.
	Limits agk.Limits
}

// Collections serves the routes under /api/v1/me/collections, and what a collection's MCP endpoint
// offers (collection_mcp.go).
type Collections struct {
	pool      *db.Pool
	versions  *version.Store
	objects   artifact.Objects
	limits    agk.Limits
	starter   *trigger.Starter
	publicURL string

	// kept is what each version's tool takes and gives, read once per version.
	kept publication
}

// NewCollections registers the collection routes on a router, each taking Own.
func NewCollections(rt *Router, o CollectionOptions) (*Collections, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil || o.Versions == nil:
		return nil, errors.New("api: the collection routes need the database and the versions, which a member's tool is read from")
	}
	if _, err := url.Parse(o.PublicURL); err != nil || o.PublicURL == "" {
		return nil, fmt.Errorf("api: a collection's url is made of the public URL, and %q is none", o.PublicURL)
	}
	starter, err := trigger.New(trigger.Options{Pool: o.Pool, Versions: o.Versions, Objects: o.Objects})
	if err != nil {
		return nil, err
	}
	if o.Limits == (agk.Limits{}) {
		o.Limits = agk.DefaultLimits()
	}
	c := &Collections{pool: o.Pool, versions: o.Versions, objects: o.Objects, limits: o.Limits, starter: starter, publicURL: strings.TrimRight(o.PublicURL, "/")}
	for _, r := range []struct {
		method, pattern string
		handler         OwnHandler
	}{
		{"GET", "/api/v1/me/collections", c.list},
		{"POST", "/api/v1/me/collections", c.create},
		{"GET", "/api/v1/me/collections/{id}", c.get},
		{"PATCH", "/api/v1/me/collections/{id}", c.update},
		{"DELETE", "/api/v1/me/collections/{id}", c.remove},
		{"PUT", "/api/v1/me/collections/{id}/members/{ns}/{name}", c.writeMember},
		{"DELETE", "/api/v1/me/collections/{id}/members/{ns}/{name}", c.removeMember},
	} {
		if err := rt.HandleOwn(r.method, r.pattern, Own{}, r.handler); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// CollectionRecord is wire.schema.json's $defs/collection.
type CollectionRecord struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	URL         string                   `json:"url"`
	Members     []CollectionMemberRecord `json:"members"`
}

// CollectionMemberRecord is wire.schema.json's $defs/collectionMember: the workflow as NS/NAME, the
// ref and the name it was written with, and the tool it offers, or null beside why it offers none.
type CollectionMemberRecord struct {
	Workflow string  `json:"workflow"`
	Ref      string  `json:"ref,omitempty"`
	As       string  `json:"as,omitempty"`
	Tool     *string `json:"tool"`
	Reason   string  `json:"reason,omitempty"`
}

// offer is what one member offers as its owner may run it now: the tool's name, the commit its ref
// points at and the graph of that version, or the reason it offers nothing.
type offer struct {
	member db.CollectionMember
	tool   string
	reason string
	commit string
	graph  *graph.Graph
}

// offers reads what each member offers its owner, in the collection's order.
func (c *Collections) offers(ctx context.Context, caller Caller, members []db.CollectionMember) ([]offer, error) {
	out := make([]offer, len(members))
	for i, m := range members {
		o, err := c.offerOf(ctx, caller, m)
		if err != nil {
			return nil, err
		}
		out[i] = o
	}
	return out, nil
}

// offerOf reads what one member offers: not_runnable where the caller may not run its workflow or
// it does not exist, no_mcp_block where the entry point at the head of its ref declares no block,
// its ref holds nothing any more, or the version there cannot be read as a workflow, and otherwise
// the tool, under the name the member gives it or the one its block gives.
func (c *Collections) offerOf(ctx context.Context, caller Caller, m db.CollectionMember) (offer, error) {
	o := offer{member: m}
	runs, err := caller.Holds(ctx, WorkflowRun, Target{Namespace: m.Namespace, Workflow: m.Workflow})
	if err != nil {
		return offer{}, err
	}
	if !runs {
		o.reason = reasonNotRunnable
		return o, nil
	}
	err = c.pool.In(ctx, m.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if m.Ref == "" {
			o.commit, err = ns.DefaultCommit(ctx, m.Workflow)
		} else {
			o.commit, err = ns.ResolveRef(ctx, m.Workflow, m.Ref)
		}
		return err
	})
	var unresolved *db.RefUnresolved
	switch {
	case errors.Is(err, db.ErrNoWorkflow) || errors.Is(err, db.ErrNoNamespace):
		o.reason = reasonNotRunnable
		return o, nil
	case errors.Is(err, db.ErrNoVersion) || errors.As(err, &unresolved):
		o.reason = reasonNoBlock
		return o, nil
	case err != nil:
		return offer{}, err
	}
	g, err := c.versions.Graph(ctx, m.Namespace, m.Workflow, o.commit)
	if err != nil || g.Workflow() == nil || g.Workflow().MCP == nil {
		// A version that cannot be read as a workflow, a library's among them, publishes nothing,
		// and the list says so rather than failing for every other member.
		o.reason = reasonNoBlock
		return o, nil
	}
	o.graph = g
	o.tool = m.As
	if o.tool == "" {
		o.tool = g.Workflow().MCP.Name
	}
	return o, nil
}

// record is a collection as the wire answers it, each member with what it offers.
func (c *Collections) record(ctx context.Context, caller Caller, col db.Collection) (CollectionRecord, error) {
	offers, err := c.offers(ctx, caller, col.Members)
	if err != nil {
		return CollectionRecord{}, err
	}
	out := CollectionRecord{
		ID: col.ID, Name: col.Name, Description: col.Description,
		URL: c.publicURL + "/mcp/collections/" + col.ID, Members: make([]CollectionMemberRecord, len(offers)),
	}
	for i, o := range offers {
		m := CollectionMemberRecord{Workflow: o.member.Namespace + "/" + o.member.Workflow, Ref: o.member.Ref, As: o.member.As, Reason: o.reason}
		if o.reason == "" {
			m.Tool = ptr(o.tool)
		}
		out.Members[i] = m
	}
	return out, nil
}

// owned is the caller as the store keeps an owner: its principal and its kind. The bootstrap token
// is nobody and owns nothing.
func owned(caller Caller) (principal, kind string, ok bool) {
	if caller.Principal == BootstrapOperator {
		return "", "", false
	}
	if strings.Contains(string(caller.Principal), "/") {
		return string(caller.Principal), db.KindServiceAccount, true
	}
	return string(caller.Principal), db.KindUser, true
}

// noCollection is what every refusal of a collection that is not the caller's reads, theirs having
// been removed or never made, "since a collection is its owner's alone and asking must teach
// nothing of another's".
const noCollection = "no collection of yours by that identifier"

// failCollection answers what the store refused about a collection, or a failure of the
// installation's.
func failCollection(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, db.ErrNoCollection):
		fail(w, http.StatusNotFound, noCollection)
	case errors.Is(err, db.ErrCollectionNameHeld):
		fail(w, http.StatusConflict, "another of your collections holds that name, which is how you tell them apart")
	case errors.Is(err, db.ErrTooManyCollections):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("you hold %d collections, the most a principal holds: a client reads a whole tool list into a model's context, and somebody needing more needs a collection fewer as much as one more", db.CollectionsPerPrincipal))
	case errors.Is(err, db.ErrTooManyMembers):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("the collection holds %d members, the most a collection holds: a client reads the whole tool list into a model's context at every list, and a hundred tools is past what a model chooses among well", db.MembersPerCollection))
	default:
		fail(w, http.StatusInternalServerError, what)
	}
}

// list is GET /api/v1/me/collections.
func (c *Collections) list(w http.ResponseWriter, r *http.Request, caller Caller) {
	principal, _, ok := owned(caller)
	answer := struct {
		Collections []CollectionRecord `json:"collections"`
	}{Collections: []CollectionRecord{}}
	if !ok {
		write(w, http.StatusOK, answer)
		return
	}
	var held []db.Collection
	if err := c.pool.Installation(r.Context(), db.Collections, func(ctx context.Context, wide *db.Wide) error {
		var err error
		held, err = wide.Collections(ctx, principal)
		return err
	}); err != nil {
		failCollection(w, err, "the collections could not be read")
		return
	}
	for _, col := range held {
		rec, err := c.record(r.Context(), caller, col)
		if err != nil {
			failCollection(w, err, "what the collections offer could not be read")
			return
		}
		answer.Collections = append(answer.Collections, rec)
	}
	write(w, http.StatusOK, answer)
}

// collectionWrite is what POST and PATCH read: a name and a description, each told apart from
// nothing written, since PATCH sets what it names and keeps the rest.
type collectionWrite struct {
	name, description       string
	named, described, empty bool
}

func (c *collectionWrite) field(b *body, name string) error {
	switch name {
	case "name":
		c.named = true
		return present(b, &c.name, "name")
	case "description":
		c.described = true
		return present(b, &c.description, "description")
	}
	return unknown(name)
}

// present reads a string a route reads written, refusing null rather than reading it as either
// the value left as it was or the empty string, which a client sending a variable it had not set
// means neither of.
func present(b *body, into *string, what string) error {
	if b.d.PeekKind() == jsontext.KindNull {
		_, _ = b.d.ReadToken()
		return fmt.Errorf("%s is null, and it is written as a string or left out", what)
	}
	return text(b, into)
}

// checkCollection refuses a name outside the identifier grammar and a description that is not one
// short line.
func checkCollection(c collectionWrite) error {
	if c.named {
		if c.name == "" || len(c.name) > agk.IdentifierMaxBytes || !workflowName.MatchString(c.name) {
			return fmt.Errorf("%.64q is not a collection's name: a name is letters, digits, hyphens and underscores beginning with a letter or a digit, at most %d characters, as everything is named", c.name, agk.IdentifierMaxBytes)
		}
	}
	if c.described {
		if n := len([]rune(c.description)); n > collectionDescriptionMax {
			return fmt.Errorf("the description is %d characters, and it is one line of at most %d: what the collection is for, read in a list", n, collectionDescriptionMax)
		}
		if strings.ContainsFunc(c.description, unicode.IsControl) {
			return errors.New("the description holds a line break or another control character, and it is one line")
		}
	}
	return nil
}

// create is POST /api/v1/me/collections.
func (c *Collections) create(w http.ResponseWriter, r *http.Request, caller Caller) {
	principal, kind, ok := owned(caller)
	if !ok {
		fail(w, http.StatusForbidden, "the bootstrap token is nobody and owns nothing: a collection is made by the user or the service account that calls through it")
		return
	}
	var in collectionWrite
	if err := readObject(r, &in, collectionMaxBytes, "the collection"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if !in.named {
		fail(w, http.StatusBadRequest, "the request names no name, and a collection is made with one: it is how you tell your collections apart")
		return
	}
	if err := checkCollection(in); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var made db.Collection
	if err := c.pool.Installation(r.Context(), db.Collections, func(ctx context.Context, wide *db.Wide) error {
		var err error
		made, err = wide.CreateCollection(ctx, principal, kind, ulid.New(), in.name, in.description)
		return err
	}); err != nil {
		failCollection(w, err, "the collection could not be made")
		return
	}
	rec, err := c.record(r.Context(), caller, made)
	if err != nil {
		failCollection(w, err, "the collection could not be read")
		return
	}
	w.Header().Set("Location", "/api/v1/me/collections/"+made.ID)
	write(w, http.StatusCreated, rec)
}

// get is GET /api/v1/me/collections/{id}.
func (c *Collections) get(w http.ResponseWriter, r *http.Request, caller Caller) {
	c.answer(w, r, caller, func(ctx context.Context, wide *db.Wide, principal string) (db.Collection, error) {
		return wide.Collection(ctx, principal, r.PathValue("id"))
	})
}

// answer reads or writes one of the caller's collections with do, and answers it as it now stands.
func (c *Collections) answer(w http.ResponseWriter, r *http.Request, caller Caller, do func(context.Context, *db.Wide, string) (db.Collection, error)) {
	principal, _, ok := owned(caller)
	if !ok {
		fail(w, http.StatusNotFound, noCollection)
		return
	}
	var col db.Collection
	if err := c.pool.Installation(r.Context(), db.Collections, func(ctx context.Context, wide *db.Wide) error {
		var err error
		col, err = do(ctx, wide, principal)
		return err
	}); err != nil {
		failCollection(w, err, "the collection could not be read")
		return
	}
	rec, err := c.record(r.Context(), caller, col)
	if err != nil {
		failCollection(w, err, "what the collection offers could not be read")
		return
	}
	write(w, http.StatusOK, rec)
}

// update is PATCH /api/v1/me/collections/{id}.
func (c *Collections) update(w http.ResponseWriter, r *http.Request, caller Caller) {
	var in collectionWrite
	if err := readObject(r, &in, collectionMaxBytes, "what changes"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if !in.named && !in.described {
		fail(w, http.StatusBadRequest, "the request names nothing to change: a collection's name, its description or both")
		return
	}
	if err := checkCollection(in); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var name, description *string
	if in.named {
		name = &in.name
	}
	if in.described {
		description = &in.description
	}
	c.answer(w, r, caller, func(ctx context.Context, wide *db.Wide, principal string) (db.Collection, error) {
		return wide.UpdateCollection(ctx, principal, r.PathValue("id"), name, description)
	})
}

// remove is DELETE /api/v1/me/collections/{id}.
func (c *Collections) remove(w http.ResponseWriter, r *http.Request, caller Caller) {
	if n, _ := io.ReadFull(io.LimitReader(r.Body, 1), make([]byte, 1)); n > 0 {
		fail(w, http.StatusBadRequest, "DELETE reads no body, and this request sends one: what is removed is the collection the path names, whole")
		return
	}
	principal, _, ok := owned(caller)
	if !ok {
		fail(w, http.StatusNotFound, noCollection)
		return
	}
	if err := c.pool.Installation(r.Context(), db.Collections, func(ctx context.Context, wide *db.Wide) error {
		return wide.DeleteCollection(ctx, principal, r.PathValue("id"))
	}); err != nil {
		failCollection(w, err, "the collection could not be removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// memberWrite is what PUT reads: the member whole, ref and as each optional.
type memberWrite struct {
	ref, as string
}

func (m *memberWrite) field(b *body, name string) error {
	switch name {
	case "ref":
		return present(b, &m.ref, "ref")
	case "as":
		return present(b, &m.as, "as")
	}
	return unknown(name)
}

// checkMember refuses a ref git could not name as a branch or a tag, a whole commit, which a member
// is never read at since it follows its workflow, and a name outside the identifier grammar.
func checkMember(m memberWrite) error {
	if m.ref != "" {
		full := m.ref
		if !strings.HasPrefix(full, "refs/") {
			full = "refs/heads/" + full
		}
		switch {
		case wholeCommit.MatchString(m.ref):
			return fmt.Errorf("%.64q is a commit, and a member is read at a branch or a tag, so that it follows the workflow as its triggers follow the default branch", m.ref)
		case strings.HasPrefix(m.ref, "-") || m.ref == "@":
			return fmt.Errorf("%.64q is not a branch or a tag git could name", m.ref)
		}
		if err := db.CheckRef(full); err != nil {
			return err
		}
	}
	if m.as != "" && (len(m.as) > agk.IdentifierMaxBytes || !workflowName.MatchString(m.as)) {
		return fmt.Errorf("%.64q is not a tool's name: clients hold the name and send it back in every call, so it is letters, digits, hyphens and underscores beginning with a letter or a digit", m.as)
	}
	return nil
}

// writeMember is PUT /api/v1/me/collections/{id}/members/{ns}/{name}.
func (c *Collections) writeMember(w http.ResponseWriter, r *http.Request, caller Caller) {
	var in memberWrite
	if err := readObject(r, &in, collectionMaxBytes, "the member"); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	if err := checkMember(in); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	principal, _, ok := owned(caller)
	if !ok {
		fail(w, http.StatusNotFound, noCollection)
		return
	}
	ctx := r.Context()
	m := db.CollectionMember{Namespace: r.PathValue("ns"), Workflow: r.PathValue("name"), Ref: in.ref, As: in.as}
	id := r.PathValue("id")

	// The collection first, so that one that is not the caller's is answered as such whatever the
	// workflow it names.
	var col db.Collection
	if err := c.pool.Installation(ctx, db.Collections, func(ctx context.Context, wide *db.Wide) error {
		var err error
		col, err = wide.Collection(ctx, principal, id)
		return err
	}); err != nil {
		failCollection(w, err, "the collection could not be read")
		return
	}

	// "A collection holds only what its owner may already run": a workflow the caller cannot run is
	// answered as one that does not exist, and so is one that does not.
	runs, err := caller.Holds(ctx, WorkflowRun, Target{Namespace: m.Namespace, Workflow: m.Workflow})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the request could not be authorised")
		return
	}
	if runs && m.Ref != "" {
		err = c.pool.In(ctx, m.Namespace, func(ctx context.Context, ns *db.NS) error {
			_, err := ns.ResolveRef(ctx, m.Workflow, m.Ref)
			return err
		})
	} else if runs {
		err = c.pool.In(ctx, m.Namespace, func(ctx context.Context, ns *db.NS) error {
			_, err := ns.WorkflowRecord(ctx, m.Workflow)
			return err
		})
	}
	var unresolved *db.RefUnresolved
	switch {
	case !runs || errors.Is(err, db.ErrNoWorkflow) || errors.Is(err, db.ErrNoNamespace):
		fail(w, http.StatusNotFound, "no workflow by that name you may run, or not yours: a collection holds what its owner may already run")
		return
	case errors.As(err, &unresolved) && unresolved.Ambiguous:
		fail(w, http.StatusBadRequest, err.Error()+": write it in full, refs/heads/ or refs/tags/")
		return
	case errors.As(err, &unresolved):
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("the repository of %s/%s holds no branch or tag %.64q", m.Namespace, m.Workflow, m.Ref))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the workflow could not be read")
		return
	}

	// "A tool's name is unique in its collection: a member whose name another member already gives
	// is refused with 409, naming the other, and as resolves it." Judged against what the others
	// offer now, under the collection's lock, so that two members written at once are judged one
	// after the other.
	written, err := c.offerOf(ctx, caller, m)
	if err != nil {
		fail(w, http.StatusInternalServerError, "what the member offers could not be read")
		return
	}
	var stored db.Collection
	err = c.pool.Installation(ctx, db.Collections, func(ctx context.Context, wide *db.Wide) error {
		held, err := wide.HoldCollection(ctx, principal, col.ID)
		if err != nil {
			return err
		}
		if written.reason == "" {
			for _, other := range held.Members {
				if other.Namespace == m.Namespace && other.Workflow == m.Workflow {
					continue
				}
				o, err := c.offerOf(ctx, caller, other)
				if err != nil {
					return err
				}
				if o.reason == "" && o.tool == written.tool {
					return &toolNameHeld{tool: written.tool, by: other.Namespace + "/" + other.Workflow}
				}
			}
		}
		stored, err = wide.WriteCollectionMember(ctx, principal, col.ID, m)
		return err
	})
	var clash *toolNameHeld
	if errors.As(err, &clash) {
		fail(w, http.StatusConflict, clash.Error())
		return
	}
	if err != nil {
		failCollection(w, err, "the member could not be written")
		return
	}
	rec, err := c.record(ctx, caller, stored)
	if err != nil {
		failCollection(w, err, "what the collection offers could not be read")
		return
	}
	write(w, http.StatusOK, rec)
}

// toolNameHeld is a member whose tool would go by a name another member already gives.
type toolNameHeld struct{ tool, by string }

func (e *toolNameHeld) Error() string {
	return fmt.Sprintf("the tool would go by %s, which %s already gives in this collection: clients hold the name, and a model told of two tools of one name cannot choose between them. Give this member another with as", e.tool, e.by)
}

// removeMember is DELETE /api/v1/me/collections/{id}/members/{ns}/{name}.
func (c *Collections) removeMember(w http.ResponseWriter, r *http.Request, caller Caller) {
	if n, _ := io.ReadFull(io.LimitReader(r.Body, 1), make([]byte, 1)); n > 0 {
		fail(w, http.StatusBadRequest, "DELETE reads no body, and this request sends one: what is taken out is the workflow the path names")
		return
	}
	c.answer(w, r, caller, func(ctx context.Context, wide *db.Wide, principal string) (db.Collection, error) {
		return wide.RemoveCollectionMember(ctx, principal, r.PathValue("id"), r.PathValue("ns"), r.PathValue("name"))
	})
}
