package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// Reading runs and what they made, by the identifiers a client holds rather than by namespace.
//
// "Agentiik sends the push service an identifier and a state", and the application a notification
// opens holds that identifier and nothing else; a mobile application opens on every run its person
// can read, wherever it is. So a run is listed across namespaces and read by its identifier alone,
// and its outputs and artifacts are reached the same way, each authorised against the workflow of
// the run it belongs to.

// artifactURLLifetime is how long the presigned URL an artifact is redirected to works.
//
// "A presigned URL covers one artifact of one run and expires in minutes." Long enough for a client
// to follow a redirect it was just given, over a slow mobile network included, and short enough
// that a URL copied out of a log or a browser's history is worth nothing by the time anybody reads
// it. It is the object's URL and not the reference's, so an artifact that expires or is collected
// in the meantime stays fetchable through it until then, and never longer.
const artifactURLLifetime = 5 * time.Minute

// across lists the runs of every workflow its caller holds run:read on, newest first.
//
// The filters narrow what is asked about rather than what is answered: a namespace or a workflow
// the caller cannot read lists nothing, which is what one that does not exist lists, so a listing
// is no way of learning which exist. Each workflow is asked about in turn, because a permission is
// held on a whole namespace or on a single workflow and asking about the workflow answers both.
func (s *Server) across(w http.ResponseWriter, r *http.Request, who Principal, holds Holds) {
	q, err := runQuery(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	namespace, workflow := r.URL.Query().Get("namespace"), r.URL.Query().Get("workflow")
	if !storable(namespace) || !storable(workflow) {
		// No namespace or workflow is named with bytes PostgreSQL cannot hold, so the
		// filter names nothing, and nothing is what it lists.
		write(w, http.StatusOK, map[string]any{"runs": []db.RunSummary{}})
		return
	}

	var workflows []db.Workflow
	err = s.pool.Installation(r.Context(), db.RunListing, func(ctx context.Context, wide *db.Wide) error {
		var err error
		workflows, err = wide.Workflows(ctx, namespace, workflow)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the runs could not be read")
		return
	}
	// Asked outside the transaction that found them, since an authorizer may read the database
	// itself, and a request holding one connection while it waits for another is how a pool runs
	// dry under load.
	readable := make([]db.Workflow, 0, len(workflows))
	for _, wf := range workflows {
		allowed, err := holds(r.Context(), Target{Namespace: wf.Namespace, Workflow: wf.Name})
		if err != nil {
			refuse(w, http.StatusInternalServerError, "the request could not be authorised")
			return
		}
		if allowed {
			readable = append(readable, wf)
		}
	}

	var runs []db.RunSummary
	err = s.pool.Installation(r.Context(), db.RunListing, func(ctx context.Context, wide *db.Wide) error {
		var err error
		runs, err = wide.Runs(ctx, readable, q)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the runs could not be read")
		return
	}
	write(w, http.StatusOK, map[string]any{"runs": runs})
}

// runQuery reads what a listing across namespaces is narrowed by, besides the namespace and the
// workflow: a state, a limit, and when the runs were created, since and until, both included and
// written in RFC 3339.
func runQuery(r *http.Request) (db.RunQuery, error) {
	query := r.URL.Query()
	q := db.RunQuery{State: query.Get("state"), Limit: intOr(query.Get("limit"), 50)}
	if q.State != "" {
		var state agk.RunState
		if err := state.UnmarshalText([]byte(q.State)); err != nil {
			return db.RunQuery{}, fmt.Errorf("%q is not a run state", q.State)
		}
	}
	for _, c := range []struct {
		name string
		into *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		written := query.Get(c.name)
		if written == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, written)
		if err != nil {
			return db.RunQuery{}, fmt.Errorf("%s is %q, which is not a time: one is written in RFC 3339, 2026-09-24T06:00:00Z", c.name, written)
		}
		*c.into = at
	}
	return q, nil
}

// output answers one workflow output of a run: the envelope the step port it is a view of
// published.
//
// Guarded by run:read_data rather than run:read, since an envelope is "envelope contents", which
// run:read does not see. The envelope is read back from the store and held to its digest before a
// byte of it is answered, as everything read back by a digest is.
func (s *Server) output(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	run, name := agk.RunID(r.PathValue("run")), r.PathValue("name")
	if !storable(name) {
		fail(w, http.StatusNotFound, "the run records no output of that name")
		return
	}
	var out db.Output
	err := s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		out, err = ns.Output(ctx, run, name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRun):
		// Found by the router a moment ago and not there now.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	case errors.Is(err, db.ErrNoOutput):
		// One answer for a name the workflow does not declare and a run that has not ended,
		// since both are an output there is nothing of yet.
		fail(w, http.StatusNotFound, "the run records no output of that name: a run's outputs are recorded when it ends with every one of them published")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the output could not be read")
		return
	}
	if !out.Envelope.PurgedAt.IsZero() {
		// 410 rather than 404, as an artifact past its retention answers: the output existed
		// and is finished, and the run still shows its digest.
		fail(w, http.StatusGone, fmt.Sprintf("the envelope of %s was purged at %s with the run's other envelopes, past the retention its workflow declared, and its digest is all that is kept", name, out.Envelope.PurgedAt.UTC().Format(time.RFC3339)))
		return
	}
	if s.objects == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and an envelope is read from nowhere else")
		return
	}
	e, err := artifact.GetEnvelope(r.Context(), s.objects, over.Namespace, out.Envelope.Digest, s.limits)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the output's envelope could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	e.Encode(w)
}

// fetchSettling is how long recording the end of a transfer against a budget may take once the
// bytes have gone. On a context of its own, because the request's is cancelled the moment a client
// that has everything closes its connection, which is what a client that has everything does.
const fetchSettling = 30 * time.Second

// fetchTransfer is how long the bytes of an artifact with a fetch budget may take to go, and
// fetchHold how long the fetch is held for them.
//
// An hour carries artifact_max_bytes, 5 GiB at its default, at twelve megabits a second, which is
// a slow link rather than a fast one. The hold outlasts the transfer and its settling, so that a
// transfer still going never finds its fetch taken by another. It is also how long a fetch stays
// held where the API serving it died before giving it back: an hour and five minutes of 409, after
// which the next transfer takes it again.
const (
	fetchTransfer = time.Hour
	fetchHold     = fetchTransfer + fetchSettling + 4*time.Minute + 30*time.Second
)

// artifactOf answers GET /api/v1/artifacts/{uri}, as How long an artifact lives sets it out: a
// redirect to a short-lived presigned URL where the artifact has no fetch budget, the bytes
// themselves where it has one, 410 once it has expired or its budget is spent, and 404 where it
// never existed.
//
// A budget is served rather than redirected because "a redirect ends when issued, so a client that
// never arrived would have spent its fetch". One fetch is held before the bytes go, spent if the
// whole of them went and matched their digest and given back otherwise, so the last fetch is
// served once however many ask for it at the same moment, the others being told it is being
// served, and a transfer that did not complete spends nothing. Nothing of the database is held
// while the bytes go, and the bytes have fetchTransfer to go in. A HEAD is answered what a GET
// would be, bytes aside, and holds nothing, since nothing is fetched. A Range is not honoured: the
// whole artifact is answered, which is the one transfer that can count.
func (s *Server) artifactOf(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	u, err := agk.ParseURI(r.PathValue("uri"))
	if err != nil || !utf8.ValidString(u.Name) {
		// The router refused a URI that does not parse, and a name that is not UTF-8 is none
		// PostgreSQL could hold, so none was ever written.
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}

	var got db.Resolved
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		got, err = ns.Resolve(ctx, u)
		return err
	})
	if !s.fetchable(w, err) {
		return
	}

	if !got.Budgeted {
		if s.urls == nil {
			fail(w, http.StatusServiceUnavailable, "this installation mints no presigned URLs, and an artifact with no fetch budget is fetched through one")
			return
		}
		redirect, err := s.urls.Presign(r.Context(), artifact.MethodGet, got.Key, u.Run, s.now().Add(artifactURLLifetime))
		if err != nil {
			fail(w, http.StatusInternalServerError, "the artifact could not be fetched")
			return
		}
		// A presigned URL is a credential for as long as it works, and no cache in between
		// has any business keeping one.
		w.Header().Set("Location", redirect)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
		return
	}
	if s.objects == nil {
		fail(w, http.StatusServiceUnavailable, "this installation has no object store attached, and an artifact is read from nowhere else")
		return
	}
	if r.Method == http.MethodHead {
		if got.Held >= got.Fetches {
			s.fetchable(w, db.ErrInFlight)
			return
		}
		bytesOf(w, u, got.Size)
		return
	}

	var held time.Time
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		got, held, err = ns.Reserve(ctx, u, fetchHold)
		return err
	})
	if !s.fetchable(w, err) {
		return
	}
	// From here the fetch is held, and every way out either spends it or gives it back, on a
	// context the request going away does not cancel. One that does neither, the API dying
	// first, is given back when its hold lapses.
	delivered := false
	defer func() {
		settle, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), fetchSettling)
		defer cancel()
		s.pool.In(settle, over.Namespace, func(ctx context.Context, ns *db.NS) error {
			if delivered {
				return ns.Delivered(ctx, u, held)
			}
			return ns.Release(ctx, u, held)
		})
	}()
	// Bounded, so that the transfer ends before its hold does. A writer that cannot be given a
	// deadline is one no connection stands behind.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fetchTransfer)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		fail(w, http.StatusInternalServerError, "the artifact could not be fetched")
		return
	}

	rc, err := s.objects.Open(r.Context(), got.Key)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the artifact could not be fetched")
		return
	}
	defer rc.Close()
	bytesOf(w, u, got.Size)
	sum := sha256.New()
	sent, err := io.Copy(w, io.TeeReader(rc, sum))
	if err != nil {
		return
	}
	// Flushed, so that a connection that is gone says so here rather than after the fetch was
	// counted. A writer that cannot flush has written through already.
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}
	delivered = sent == got.Size && hex.EncodeToString(sum.Sum(nil)) == got.Digest
}

// fetchable answers a resolution or a reservation that found nothing to fetch, and says whether
// there is something.
func (s *Server) fetchable(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, db.ErrNoArtifact):
		fail(w, http.StatusNotFound, "no such thing, or not yours")
	case errors.Is(err, db.ErrGone):
		fail(w, http.StatusGone, "that artifact has expired or its fetches are spent: it existed, and is finished")
	case errors.Is(err, db.ErrInFlight):
		fail(w, http.StatusConflict, "every fetch left of that artifact is being served to somebody else, and one comes back if its transfer does not complete: ask again later")
	default:
		fail(w, http.StatusInternalServerError, "the artifact could not be fetched")
	}
	return false
}

// bytesOf writes the headers an artifact's bytes are served with: as bytes, which a browser
// neither renders nor sniffs, since they are served from the API's own origin, to be saved under
// the artifact's name, and kept by no cache, since a budget is counted here.
func bytesOf(w http.ResponseWriter, u agk.URI, size int64) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": u.Name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

// storable says whether PostgreSQL could hold a string as text: UTF-8, with no U+0000. One it
// could not is no name anything was ever written under, and asking about it would be an error from
// the database rather than an answer.
func storable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
