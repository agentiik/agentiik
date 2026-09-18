package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream is the one stream every task travels on.
const Stream = "AGENTIIK_TASKS"

// Subject is where a task for one runner pool goes.
//
// One subject per pool, so that a runner's durable consumer filters on the work it can take
// rather than reading everything and discarding most of it. The pool is a label value and is
// held to a grammar here for the reason every name is: a pool carrying a dot or a wildcard would
// be a pool that could read another pool's subject.
func Subject(pool string) string { return "agentiik.tasks." + pool }

// DefaultPool is where a task with no runs_on goes.
//
// "runs_on: [] or absent means the default pool", so a step that asks for nothing asks for the
// pool an installation configured to take anything.
const DefaultPool = "default"

// Bus is a connection to the task bus.
type Bus struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	stream  jetstream.Stream
	results jetstream.Stream

	// Trouble is where a message nobody can read goes. Such a message is taken off the
	// queue, because it will never become readable and redelivering it for ever would cost
	// the pool, and that is exactly why it has to be said out loud: a wire that stopped
	// matching would otherwise be a queue that quietly swallowed everything on it.
	Trouble func(subject string, err error)
}

// report says one thing, through whatever Trouble was given.
func (b *Bus) report(subject string, err error) {
	if b.Trouble == nil {
		return
	}
	b.Trouble(subject, err)
}

// Open connects, and makes sure the stream is there.
//
// Creating it here rather than in a deployment step is deliberate: the stream's retention is part
// of what the engine promises, not part of how an operator chose to install it. An installation
// that had configured a different retention would have a bus that kept work after it was done.
func Open(ctx context.Context, url string) (*Bus, error) {
	conn, err := nats.Connect(url,
		nats.Name("agentiik"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second))
	if err != nil {
		return nil, fmt.Errorf("bus: the bus at that address could not be reached: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bus: JetStream could not be reached: %w", err)
	}

	// Both streams, here rather than where each is first used. A result published to a
	// subject no stream covers is answered with "no response from stream", which is a
	// runner discovering at the worst moment that the far end was never set up.
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        Stream,
		Description: "One task message per shard of per attempt of per step, removed once a runner has taken it.",
		Subjects:    []string{Subject(">")},
		// "WorkQueue retention, where a message is removed as soon as it has been
		// consumed, which is precisely what work distribution needs." A stream that kept
		// them would be a stream a second consumer could take the same work from.
		Retention: jetstream.WorkQueuePolicy,
		// A task that nobody takes is a task whose run will time out anyway, and a bus
		// that filled up because one pool had no runners would stop every other pool too.
		Discard:    jetstream.DiscardOld,
		Storage:    jetstream.FileStorage,
		MaxAge:     7 * 24 * time.Hour,
		Replicas:   1,
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bus: the task stream could not be created: %w", err)
	}

	results, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        Results,
		Description: "One result per attempt, removed once the controller has recorded it.",
		Subjects:    []string{ResultSubject},
		Retention:   jetstream.WorkQueuePolicy,
		Discard:     jetstream.DiscardOld,
		Storage:     jetstream.FileStorage,
		MaxAge:      7 * 24 * time.Hour,
		Replicas:    1,
		Duplicates:  2 * time.Minute,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bus: the result stream could not be created: %w", err)
	}
	return &Bus{conn: conn, js: js, stream: stream, results: results}, nil
}

// Close releases the connection.
func (b *Bus) Close() { b.conn.Close() }

// Publish puts one task on the queue its labels select.
//
// This is controller.Queue's half. The message carries the task as the wire describes it, and
// the identifier is given to JetStream as its deduplication key: a bus is at-least-once and the
// runner is what makes that safe, but a publish retried by this process inside the duplicate
// window is a retry this process knows about and there is no reason to make somebody else pay
// for it.
func (b *Bus) Publish(ctx context.Context, d controller.Dispatch) error {
	t := d.Task
	pool, err := PoolOf(t.RunsOn)
	if err != nil {
		return fmt.Errorf("bus: task %s: %w", t.ID, err)
	}
	m, err := messageOf(d)
	if err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("bus: task %s could not be written: %w", t.ID, err)
	}
	msg := &nats.Msg{
		Subject: Subject(pool),
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{string(t.ID)},
			"Agentiik-Namespace":  []string{t.Namespace},
			"Agentiik-Run":        []string{string(t.Run)},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("bus: task %s could not be published: %w", t.ID, err)
	}
	return nil
}

// Stop asks for a task in flight to be stopped.
//
// A stop is not a queue message. The task it names is held by a runner that already took it, so
// putting a stop on the work queue would be putting it where nobody holding that task is looking
// and where a runner with room would take it as work. It goes out as a plain subject a runner
// subscribes to for as long as it holds anything, which is the one thing the bus does that is
// not work distribution.
func (b *Bus) Stop(ctx context.Context, s graph.Stop) error {
	body, err := json.Marshal(struct {
		Task   agk.TaskID `json:"task"`
		Reason string     `json:"reason"`
	}{Task: s.Task, Reason: s.Reason.String()})
	if err != nil {
		return fmt.Errorf("bus: the stop for %s could not be written: %w", s.Task, err)
	}
	if err := b.conn.Publish(StopSubject, body); err != nil {
		return fmt.Errorf("bus: the stop for %s could not be published: %w", s.Task, err)
	}
	// Flushed, because a plain publish is fire and forget and a stop that never left the
	// buffer is a container that runs to its deadline. The flush is given a bound of its
	// own: the client refuses a context with no deadline, and a caller passing one that has
	// none is asking for a stop rather than asking to wait for ever.
	flush, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := b.conn.FlushWithContext(flush); err != nil {
		return fmt.Errorf("bus: the stop for %s was published and not flushed: %w", s.Task, err)
	}
	return nil
}

// StopSubject is where a stop goes. Not a stream: a stop is worth nothing to a runner that was
// not holding the task, and worth nothing later.
const StopSubject = "agentiik.stops"

// PoolOf is the runner pool a task's labels select.
//
// "runs_on: [arch=amd64] ... Restricted to the runner pools the namespace is allowed to use." A
// label is written key=value, and the pool is what a pool= label names; a task that names none
// goes to the default pool.
func PoolOf(runsOn []string) (string, error) {
	pool := DefaultPool
	for _, label := range runsOn {
		key, value, ok := strings.Cut(label, "=")
		if !ok || key == "" || value == "" {
			return "", fmt.Errorf("%q is not a runner label: one is written key=value", label)
		}
		if key != "pool" {
			continue
		}
		if err := validPool(value); err != nil {
			return "", err
		}
		pool = value
	}
	return pool, nil
}

// validPool holds a pool name to what can be a subject token.
//
// A dot would make one pool's subject a prefix of another's, and a wildcard would make it every
// pool's, so both are refused here rather than at the far end where the damage is a runner
// reading work it was never offered.
func validPool(pool string) error {
	if pool == "" {
		return errors.New("a runner pool with no name")
	}
	for _, r := range pool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf("%q is not a runner pool: letters, digits, hyphens and underscores, because a pool name is a subject token and a dot or a wildcard in one would reach another pool's work", pool)
		}
	}
	return nil
}
