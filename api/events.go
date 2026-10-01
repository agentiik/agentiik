package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/trigger"
)

// Events published to a namespace.
//
// "An event is published into a namespace with POST /api/v1/{ns}/events, by a principal holding
// workflow:run there at namespace scope": one CloudEvents 1.0 event, in either of the modes its HTTP
// binding defines. The API matches it in the request against every event trigger armed in the
// namespace, and against those of another namespace that name it where that namespace's built-in
// identity holds workflow:read here at namespace scope; each whose filter accepts it starts a run by
// the one path every run takes, attributed to the built-in identity of the namespace listening, its
// inputs filled by map. A filter that fails, inputs refused and a namespace past max_runs_per_hour
// are recorded on the trigger as skipped firings. The answer counts the runs started and names none,
// since which workflows listen is theirs to know.

// eventMaxBytes bounds the request: one envelope's weight, as a webhook's body is bounded, since the
// event is frozen on every run it starts and read with it at every decision the controller takes.
const eventMaxBytes = trigger.InputsMaxBytes

// eventMemory is how long an event is remembered by its source and its id: "a producer sends one
// again after a network error or when its own queue redelivers after an outage; a day covers both",
// and a row kept longer would answer a producer that reuses an id on purpose with runs it never
// started.
const eventMemory = 24 * time.Hour

// The bounds of the two attributes an event is remembered by, which the row keeps in its key: an id
// as long as a webhook's delivery, and a source, a URI-reference, as long as one is written.
const (
	eventIDMax     = 255
	eventSourceMax = 1024
)

// errEventNotText is data that is neither JSON nor UTF-8 text, which event.data cannot hold.
var errEventNotText = errors.New("the event's data is neither JSON, by its datacontenttype, nor UTF-8 text, and event.data holds one or the other: data_base64 and binary data are refused")

// publish answers POST /api/v1/{ns}/events.
func (s *Server) publish(w http.ResponseWriter, r *http.Request, who Principal, over Target) {
	raw, err := slurp(r, eventMaxBytes)
	if err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	event, err := cloudEvent(r, raw)
	if err != nil {
		var large *trigger.InputsTooLarge
		switch {
		case errors.As(err, &large):
			fail(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, errEventNotText), errors.Is(err, errEventBatch):
			fail(w, http.StatusUnsupportedMediaType, err.Error())
		default:
			fail(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	source, id, kind := event["source"].(string), event["id"].(string), event["type"].(string)

	runs, err := s.deliver(r.Context(), over.Namespace, string(who), source, id, kind, event)
	if err != nil {
		s.report(err)
		fail(w, http.StatusInternalServerError, "the event could not be delivered")
		return
	}
	write(w, http.StatusAccepted, map[string]any{"runs": runs})
}

// heard is one event trigger an event matched, and what it came to before anything is written: the
// run prepared, or why none is, a skipped firing.
type heard struct {
	listening db.Listening
	prepared  trigger.Prepared
	skipped   string
}

// deliver matches the event published into namespace against the triggers that hear it, and starts
// what they start, in one transaction that remembers the event: answered, for an event published
// again within the day, with what the first started.
func (s *Server) deliver(ctx context.Context, namespace, publisher, source, id, kind string, event map[string]any) (int, error) {
	var listening []db.Listening
	if err := s.pool.Installation(ctx, db.EventDelivery, func(ctx context.Context, wide *db.Wide) error {
		var err error
		listening, err = wide.Listening(ctx, namespace, kind, source)
		return err
	}); err != nil {
		return 0, err
	}

	// "Where that namespace's built-in identity holds workflow:read in it at namespace scope at that
	// instant", asked once for each namespace listening, and never of the namespace itself.
	granted := map[string]bool{namespace: true}
	var matched []heard
	for _, l := range listening {
		held, asked := granted[l.Namespace]
		if !asked {
			var err error
			held, err = s.router.auth.Allow(ctx, Principal(l.Namespace+"/"+db.BuiltIn), WorkflowRead, Target{Namespace: namespace})
			if err != nil {
				return 0, err
			}
			granted[l.Namespace] = held
		}
		if !held {
			continue
		}
		h, hears, err := s.hear(ctx, l, namespace, publisher, source, id, event)
		if err != nil {
			return 0, err
		}
		if hears {
			matched = append(matched, h)
		}
	}

	now := s.now()
	runs := 0
	err := s.pool.Installation(ctx, db.EventDelivery, func(ctx context.Context, wide *db.Wide) error {
		runs = 0
		taken := false
		if err := wide.Within(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			runs, taken, err = ns.EventTaken(ctx, source, id, publisher, now, eventMemory)
			return err
		}); err != nil || taken {
			return err
		}
		for _, h := range matched {
			if err := wide.Within(ctx, h.listening.Namespace, func(ctx context.Context, ns *db.NS) error {
				var run agk.RunID
				skipped := h.skipped
				if skipped == "" {
					var err error
					run, err = h.prepared.Create(ctx, ns)
					var reached *db.RunsPerHourReached
					switch {
					case errors.As(err, &reached):
						// "A scheduled or event firing starts no run and is recorded as a
						// skipped firing, with that reason."
						skipped = reached.Reason()
					case errors.Is(err, db.ErrWorkflowMoving):
						skipped = "the workflow is being moved to another namespace, and starts no run until it is"
					case err != nil:
						return err
					default:
						runs++
					}
				}
				return ns.EventFired(ctx, h.listening, now, run, skipped)
			}); err != nil {
				return err
			}
		}
		return wide.Within(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			return ns.EventRuns(ctx, source, id, runs)
		})
	})
	return runs, err
}

// hear reads what one trigger l makes of the event: false where its filter refuses it, which records
// nothing; otherwise the run it starts, prepared, or why it starts none. err is the installation's
// trouble, and not the trigger's.
func (s *Server) hear(ctx context.Context, l db.Listening, published, publisher, source, id string, event map[string]any) (heard, bool, error) {
	h := heard{listening: l}
	// The version that armed it, which passed the rules it is armed by: one that cannot be read is
	// the installation's trouble, and the event is not taken, so that publishing it again once the
	// trouble is past starts what it should.
	g, err := s.versions.Graph(ctx, l.Namespace, l.Workflow, l.Commit)
	if err != nil {
		return h, false, fmt.Errorf("api: the version %s of %s/%s, whose event trigger %d heard an event, could not be read: %w", l.Commit, l.Namespace, l.Workflow, l.Position, err)
	}
	events := g.Workflow().On.Event
	if l.Position >= len(events) {
		return h, false, fmt.Errorf("api: the event trigger %d of %s/%s is armed at %s, which declares %d", l.Position, l.Namespace, l.Workflow, l.Commit, len(events))
	}
	e := events[l.Position]
	// The variables the listening workflow's namespace shows it, which its filter and its map read
	// under vars beside the file's own, and which the run it starts is handed, so that its steps
	// read what its filter and its map read. Its own namespace's, never the publisher's.
	var shown map[string]any
	if err := s.pool.In(ctx, l.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		shown, err = ns.VariablesFor(ctx, l.Workflow)
		return err
	}); err != nil {
		return h, false, fmt.Errorf("api: the variables %s shows %s, whose event trigger %d heard an event, could not be read: %w", l.Namespace, l.Workflow, l.Position, err)
	}
	fired := graph.Fired{
		Commit: l.Commit, Trigger: map[string]any{}, Event: event,
		TriggerKind: agk.TriggerEvent.String(), TriggeredBy: l.Namespace + "/" + db.BuiltIn,
		Vars: shown,
	}
	accepted, err := g.Hears(e, fired)
	if err != nil {
		// "One that fails to evaluate ... is recorded on the trigger as a skipped firing."
		h.skipped = "the filter could not be evaluated over the event: " + err.Error()
		return h, true, nil
	}
	if !accepted {
		return h, false, nil
	}
	filled, err := g.FillFromEvent(e.Map, fired)
	if err == nil {
		filled, err = asSupplied(filled)
	}
	if err != nil {
		h.skipped = "the map could not fill the inputs from the event: " + err.Error()
		return h, true, nil
	}
	h.prepared, err = s.starter.Prepare(ctx, trigger.Request{
		Namespace: l.Namespace, Workflow: l.Workflow, Kind: agk.TriggerEvent, Commit: l.Commit,
		Inputs: filled, Context: db.TriggerContext{Event: event}, NamespaceVars: shown,
		Detail: map[string]any{"source": source, "id": id, "published_in": published, "publisher": publisher},
	})
	if err != nil {
		if ctx.Err() != nil {
			return h, false, ctx.Err()
		}
		h.skipped = "no run could be started: " + err.Error()
	}
	return h, true, nil
}

// jsonKind names what a decoded JSON value is, for a refusal.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "a string"
}

// errEventBatch is a batch of events, which a request carries one of.
var errEventBatch = errors.New("the request is a batch of events, application/cloudevents-batch+json, and one is published to a request, so that each refusal names the event it refuses")

// eventAttributes are the attributes of CloudEvents 1.0 that event exposes, beside data: "id, source,
// specversion, type; optionally subject, time, datacontenttype, dataschema".
var eventAttributes = []string{"id", "source", "specversion", "type", "subject", "time", "datacontenttype", "dataschema"}

// cloudEvent reads the event a request publishes, in the structured mode of the HTTP binding, the
// whole event as application/cloudevents+json, or in the binary mode, its attributes as ce- headers
// and the body its data: the event root as expressions read it. An extension attribute is accepted
// and not kept, since event exposes the attributes the specification defines and no others.
func cloudEvent(r *http.Request, raw []byte) (map[string]any, error) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	event := map[string]any{}
	switch {
	case kind == "application/cloudevents-batch+json":
		return nil, errEventBatch

	case kind == "application/cloudevents+json":
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var doc map[string]any
		if err := d.Decode(&doc); err != nil {
			return nil, fmt.Errorf("the request says it is an event, application/cloudevents+json, and is not one JSON object: %v", err)
		}
		if d.More() {
			return nil, errors.New("the request says it is an event and holds more than one JSON document")
		}
		if _, ok := doc["data_base64"]; ok {
			return nil, errEventNotText
		}
		for _, name := range eventAttributes {
			v, ok := doc[name]
			if !ok {
				continue
			}
			text, isText := v.(string)
			if !isText {
				return nil, fmt.Errorf("the event's %s is %s, and every attribute of an event is a string", name, jsonKind(v))
			}
			event[name] = text
		}
		if data, ok := doc["data"]; ok {
			event["data"] = data
		}

	default:
		// The binary mode: "each attribute is a ce- header, the body its data".
		for _, name := range eventAttributes {
			if name == "datacontenttype" {
				continue
			}
			if v := r.Header.Get("ce-" + name); v != "" {
				event[name] = v
			}
		}
		if len(event) == 0 {
			return nil, errors.New("the request carries no event: publish one CloudEvents 1.0 event, as application/cloudevents+json or with its attributes as ce-id, ce-source, ce-specversion and ce-type headers")
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			contentType := r.Header.Get("Content-Type")
			if contentType != "" {
				event["datacontenttype"] = contentType
			}
			switch {
			case kind == "application/json" || strings.HasSuffix(kind, "+json"):
				d := json.NewDecoder(bytes.NewReader(raw))
				d.UseNumber()
				var data any
				if err := d.Decode(&data); err != nil {
					return nil, fmt.Errorf("the event's data says it is JSON and is not: %v", err)
				}
				if d.More() {
					return nil, errors.New("the event's data says it is JSON and holds more than one document")
				}
				event["data"] = data
			case utf8.Valid(raw):
				event["data"] = string(raw)
			default:
				return nil, errEventNotText
			}
		}
	}

	if event["specversion"] != "1.0" {
		return nil, fmt.Errorf("the event's specversion is %.16q, and this API reads CloudEvents 1.0", fmt.Sprint(event["specversion"]))
	}
	for _, name := range []string{"id", "source", "type"} {
		if v, _ := event[name].(string); v == "" {
			return nil, fmt.Errorf("the event carries no %s, which CloudEvents 1.0 requires of every event", name)
		}
	}
	if id := event["id"].(string); len(id) > eventIDMax {
		return nil, fmt.Errorf("the event's id is %d bytes, and an event is remembered by its id, which is at most %d", len(id), eventIDMax)
	}
	if source := event["source"].(string); len(source) > eventSourceMax {
		return nil, fmt.Errorf("the event's source is %d bytes, and an event is remembered by its source, which is at most %d", len(source), eventSourceMax)
	}
	if at, ok := event["time"].(string); ok {
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("the event's time is %.64q, and CloudEvents writes it as RFC 3339: %v", at, err)
		}
	}
	if data, ok := event["data"]; ok {
		if n := trigger.Values(data); n > trigger.InputsMaxValues {
			return nil, &trigger.InputsTooLarge{Why: fmt.Sprintf("the event's data holds %d values, and event.data holds at most %d, as many as a run's inputs may, since it is frozen on every run it starts", n, trigger.InputsMaxValues)}
		}
	}
	return event, nil
}
