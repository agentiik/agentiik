package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/internal/tlsfloor"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream is the one stream every task travels on.
const Stream = "AGENTIIK_TASKS"

// Subject is where a task for one runner pool goes.
//
// One subject per pool, so that a runner's durable consumer filters on the work it can take
// rather than reading everything and discarding most of it. The pool is the name an administrator
// gave it, and is held to a grammar here for the reason every name is: a pool carrying a dot or a
// wildcard would be a pool that could read another pool's subject.
func Subject(pool string) string { return "agentiik.tasks." + pool }

// DefaultPool is where a task with no runs_on goes, and the pool every installation is created
// with, by the migration that creates it.
//
// A step that names no label asks for nothing, and every pool carries nothing, so it needs a pool
// named for it rather than one found by its labels.
const DefaultPool = "default"

// Bus is a connection to the task bus.
type Bus struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	stream  jetstream.Stream
	results jetstream.Stream

	// Trouble is where a message goes that no delivery would ever change: one nobody can
	// read, and a result that reads well and that no controller could ever record, which
	// the function Reports hands it to answers with Drop, as package bus/control does for
	// controller.ErrNotAResult. Such a message is taken off the queue,
	// because redelivering it for ever would cost the pool, and that is exactly why it has
	// to be said out loud: a queue that quietly swallowed it would hide a wire that stopped
	// matching, or a runner answering with what is not a result. The two point at different
	// components, and errors.Is is what tells them apart.
	Trouble func(subject string, err error)
}

// report says one thing, through whatever Trouble was given.
func (b *Bus) report(subject string, err error) {
	if b.Trouble == nil {
		return
	}
	b.Trouble(subject, err)
}

// Options are how a connection is opened.
type Options struct {
	URL string

	// Name is what this connection calls itself, which is what an operator reads in the
	// server's own connection list.
	Name string

	// Credentials are what this side authenticates with, where the bus asks for any: the
	// control plane's for Open, the one the API minted for a runner's pool for OpenRunner.
	// Nil is a bus that takes none.
	Credentials *Credentials
}

// Open connects, and makes sure the stream is there.
//
// Creating it here rather than in a deployment step is deliberate: the stream's retention is part
// of what the engine promises, not part of how an operator chose to install it. An installation
// that had configured a different retention would have a bus that kept work after it was done.
func Open(ctx context.Context, o Options) (*Bus, error) {
	conn, js, err := connect(o, "")
	if err != nil {
		return nil, err
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
		Description: "One result per attempt, and the running and publishing of each dispatch before it, removed once the controller has taken them.",
		Subjects:    []string{ResultSubject("*")},
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

// OpenRunner connects as a runner, which takes work from its pool and says what became of it.
//
// It creates nothing and makes sure of nothing, which is not a courtesy: the credential a runner
// holds cannot create a stream or a consumer, and is refused the request that would ask whether
// one is there. The streams are Open's and the pool's consumer is Consumer's, both on the control
// plane, and a runner finds them there or finds out at its first Take that they are missing.
//
// Its replies come back under the inbox of the runner the credential was minted for, which is
// the one inbox that credential may subscribe to. The name is read off the credential rather than
// asked of the caller, so that the two cannot disagree.
func OpenRunner(o Options) (*Bus, error) {
	var inbox string
	if o.Credentials != nil {
		runner, err := runnerOf(*o.Credentials)
		if err != nil {
			return nil, err
		}
		inbox = Inbox(runner)
	}
	conn, js, err := connect(o, inbox)
	if err != nil {
		return nil, err
	}
	return &Bus{conn: conn, js: js}, nil
}

// runnerOf reads which runner a credential was minted for, which is the name its JWT carries.
func runnerOf(c Credentials) (string, error) {
	claims, err := jwt.DecodeUserClaims(c.JWT)
	if err != nil {
		return "", fmt.Errorf("bus: the credential could not be read: %w", err)
	}
	if err := validRunner(claims.Name); err != nil {
		return "", fmt.Errorf("bus: the credential is not a runner's: %w", err)
	}
	return claims.Name, nil
}

// connect opens the connection both sides share, with replies under inbox where one is named.
func connect(o Options, inbox string) (*nats.Conn, jetstream.JetStream, error) {
	if o.URL == "" {
		return nil, nil, errors.New("bus: no bus address")
	}
	if err := CheckURL(o.URL); err != nil {
		return nil, nil, err
	}
	options := []nats.Option{
		nats.Name(o.Name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		// So that a subscription read by hand carries the server's refusal of it, which
		// is how Stops tells a runner that may not hear stops from one that heard none
		// yet. It changes nothing for a subscription that is not read by hand, and the
		// JetStream client reads none by hand.
		nats.PermissionErrOnSubscribe(true),
	}
	if o.Name == "" {
		options[0] = nats.Name("agentiik")
	}
	if o.Credentials != nil {
		// A credential rather than a password, and one that expires. An installation
		// whose bus takes no credential at all is profile A, where the bus is on the
		// same host and reachable by nothing else.
		options = append(options, nats.UserJWTAndSeed(o.Credentials.JWT, o.Credentials.Seed))
	}
	if inbox != "" {
		options = append(options, nats.CustomInboxPrefix(inbox))
	}
	// The floor, on the configuration nats.go uses for a tls:// or wss:// server and for a
	// server that insists on TLS. A loopback address reached in plaintext takes no server
	// another one gossips, which could be anywhere and would be reached in plaintext too.
	options = append(options, func(n *nats.Options) error { n.TLSConfig = tlsfloor.Config(); return nil })
	if !secure(o.URL) {
		options = append(options, nats.IgnoreDiscoveredServers())
	}
	conn, err := nats.Connect(o.URL, options...)
	if err != nil {
		return nil, nil, fmt.Errorf("bus: the bus at that address could not be reached: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("bus: JetStream could not be reached: %w", err)
	}
	return conn, js, nil
}

// Close releases the connection.
func (b *Bus) Close() { b.conn.Close() }

// AckWait is how long a pool's consumer waits for a runner to acknowledge a task it was handed,
// before handing it to another runner of the pool.
//
// It bounds a take and a redemption, and not a task. A runner writes the key down, redeems the
// grant and acknowledges, and pulls the image and starts the container only after that, so what the
// wait has to hold is one write to the host's disk and one round trip to the API, which reads the
// task's input envelopes and secret values from their stores before it answers. A minute is many
// times that, so that a redemption slowed by a loaded API or a slow store is not handed to a second
// runner halfway through, and short enough that a task whose runner died before redeeming it waits
// no longer than that for the next one.
//
// A wait that proved too short runs nothing twice. The message goes to a second runner as well, the
// two redemptions bind the task once, and the runner refused it acknowledges the message and starts
// nothing: what it costs is a redemption. A wait too long costs a task whose runner died before
// redeeming it that much more time on the queue. Nothing depends on how it compares with
// db.LostAfter either: a message whose runner died after redeeming it is refused to the next runner
// whether the heartbeat's sweep has declared the task lost yet or the silent runner still holds it.
// A runner that redeemed and never heard the answer does not wait on the message coming round,
// which would find the task already lost, since the sweep counts it from the redemption and
// db.LostAfter is the shorter of the two: it keeps the key, names it in its heartbeat and redeems
// again itself, as Taken.Refused says.
const AckWait = time.Minute

// Consumer makes sure the one durable consumer a pool's runners share is there.
//
// The control plane creates it because a runner's credential cannot, and that is the point: a
// machine able to create a consumer is a machine able to create one with no filter and take every
// pool's work.
func (b *Bus) Consumer(ctx context.Context, pool string) error {
	return b.consumer(ctx, pool, AckWait)
}

// consumer is Consumer with the wait said, which is how a test sees what follows the wait without
// waiting AckWait for it.
func (b *Bus) consumer(ctx context.Context, pool string, wait time.Duration) error {
	if err := validPool(pool); err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	_, err := b.js.CreateOrUpdateConsumer(ctx, Stream, jetstream.ConsumerConfig{
		Durable:       Durable(pool),
		Description:   "Every runner of the " + pool + " pool, sharing one queue.",
		FilterSubject: Subject(pool),
		AckPolicy:     jetstream.AckExplicitPolicy,
		// A runner acknowledges once it has redeemed the grant, before it pulls or starts
		// anything, and not when the container finishes: the package documentation says
		// why. So this bounds a take and a redemption, AckWait says how, and a task that
		// runs for an hour is not redelivered halfway through it. What notices a host that
		// died after the redemption is the heartbeat, and what it produces is lost rather
		// than a second delivery.
		AckWait: wait,
		// Without limit, because a message comes round again only when a runner took it and
		// never redeemed it, redeemed it and never got its acknowledgement through, is
		// still redeeming it after an answer that never came, left it for its key to end on
		// its host, or put it back, and none of them is a reason to give up on the task. The
		// one that waits on a key to end comes round for as long as the container runs.
		MaxDeliver:    -1,
		MaxAckPending: -1,
	})
	if err != nil {
		return fmt.Errorf("bus: the consumer for pool %s could not be created: %w", pool, err)
	}
	return nil
}

// Publish puts one task message on the queue of the pool Route chose for it.
//
// The pool is given rather than read off the message's runs_on, because choosing one needs the
// pools, which the controller holds and the bus does not, and because the pool whose policy the
// controller applied has to be the pool whose runners are handed the task. It is still held to
// what a subject token can be, since it becomes one.
//
// It is the control plane's, and package bus/control is what calls it, once it has written what the
// controller decided as the wire describes it: a runner's credential may not publish on a task
// subject at all. The task_id is given to JetStream as its deduplication key, read off the message
// rather than asked of the caller so that the two cannot disagree. A bus is at-least-once and the
// runner is what makes that safe, but a publish retried by this process inside the duplicate window
// is a retry this process knows about and there is no reason to make somebody else pay for it. A
// task a later pass planned again, because the pass that published it could not record the
// dispatch, is deduplicated the same way, and it carries a grant of its own: the message that stays
// is the first, which is why a grant once issued is never replaced.
//
// The task_id and not the idempotency key, because "a requeue after loss keeps the idempotency
// key and takes a new task_id". Deduplicated on the key, a task lost within two minutes of being
// published would be requeued into a stream that answers it was already there, and the requeue
// would go nowhere while the controller recorded it as handed out.
func (b *Bus) Publish(ctx context.Context, pool string, m TaskMessage) error {
	if err := validPool(pool); err != nil {
		return fmt.Errorf("bus: task %s: %w", m.IdempotencyKey, err)
	}
	if m.TaskID == "" {
		return fmt.Errorf("bus: task %s names no task_id, and a publish is deduplicated on it", m.IdempotencyKey)
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("bus: task %s could not be written: %w", m.IdempotencyKey, err)
	}
	msg := &nats.Msg{
		Subject: Subject(pool),
		Data:    body,
		Header: nats.Header{
			jetstream.MsgIDHeader: []string{m.TaskID},
			"Agentiik-Namespace":  []string{m.Namespace},
			"Agentiik-Run":        []string{m.RunID},
		},
	}
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("bus: task %s could not be published: %w", m.IdempotencyKey, err)
	}
	return nil
}

// Pool is a runner pool as a task is routed to one: its name, which is its subject, and the labels
// its runners may claim, which are what a step's runs_on selects it by.
type Pool struct {
	Name   string
	Labels []string
}

// Unrouted is a task that no pool, or more than one, is selected by.
//
// It carries the pools rather than a sentence, because what a person should be told depends on
// which it is: no pool is a pool nobody created, two are pools nobody told apart, and the
// controller writes that into the step's reason in its own words.
type Unrouted struct {
	// RunsOn is what the task asked for, empty where it asked for nothing.
	RunsOn []string

	// Pools are the pools that carry every label of it, none or more than one, by name.
	Pools []string
}

func (u *Unrouted) Error() string {
	switch {
	case len(u.RunsOn) == 0 && len(u.Pools) == 0:
		return fmt.Sprintf("a task that names no label goes to the runner pool %s, which does not exist", DefaultPool)
	case len(u.Pools) == 0:
		return fmt.Sprintf("no runner pool carries every label of [%s]", strings.Join(u.RunsOn, ", "))
	default:
		return fmt.Sprintf("the runner pools %s each carry every label of [%s], and a task goes to one pool", strings.Join(u.Pools, ", "), strings.Join(u.RunsOn, ", "))
	}
}

// Route is the one pool a task's runs_on selects among the pools given, which are the pools the
// run's namespace may reach.
//
// A task goes to the pool whose labels include every label it names: a pool's labels are the most
// any of its runners may claim, and the pool is what a step is written against. One
// pool and never each pool that matches, because the pool is the queue: a task on two queues runs
// twice, and a choice made here between two would be a choice nobody wrote down. A task that names
// no label goes to DefaultPool, which the installation creates, rather than to whichever pool
// matches it, since every pool does.
//
// The controller calls it on the pools it read in the transaction that issues the task's grant,
// and hands the answer to Publish with the message, so that the pool whose policy was applied and
// the pool whose runners are handed the task are one pool.
func Route(runsOn []string, pools []Pool) (string, error) {
	for _, label := range runsOn {
		key, value, ok := strings.Cut(label, "=")
		if !ok || key == "" || value == "" {
			return "", fmt.Errorf("%q is not a runner label: one is written key=value", label)
		}
	}
	var matching []string
	for _, p := range pools {
		switch {
		case len(runsOn) == 0:
			if p.Name == DefaultPool {
				matching = append(matching, p.Name)
			}
		case carries(p.Labels, runsOn):
			matching = append(matching, p.Name)
		}
	}
	if len(matching) != 1 {
		slices.Sort(matching)
		return "", &Unrouted{RunsOn: runsOn, Pools: matching}
	}
	return matching[0], nil
}

// carries says whether labels include every one of asked.
func carries(labels, asked []string) bool {
	for _, label := range asked {
		if !slices.Contains(labels, label) {
			return false
		}
	}
	return true
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
	if !isToken(pool) {
		return fmt.Errorf("%q is not a runner pool: letters, digits, hyphens and underscores, because a pool name is a subject token and a dot or a wildcard in one would reach another pool's work", pool)
	}
	return nil
}

// validRunner holds a runner's name to the grammar the wire writes one in, which is also what can
// be a subject token: a runner's results go on a subject of its own, and a name with a wildcard in
// it would be a credential allowed to publish as every runner at once.
//
// The wire's grammar rather than any subject token, because the name is the runner field of every
// result it publishes and the API mints it in that grammar, lowercase words joined by hyphens. A
// runner the API could not have minted is a credential somebody wrote by hand, and a result naming
// one is taken off the queue before the controller reads it as a host.
func validRunner(runner string) error {
	if runner == "" {
		return errors.New("a runner with no name, and a result is taken from the runner that sent it")
	}
	if !runnerName.MatchString(runner) {
		return fmt.Errorf("%.64q is not a runner: a runner is named in lowercase words joined by hyphens, the name the API minted it at join, and it is a subject token, so a dot or a wildcard in one would reach another runner's results", runner)
	}
	return nil
}

// runnerName is the wire's pattern for a runner, wire.schema.json $defs/taskResult/runner, held
// to it by a test.
var runnerName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// isToken says whether a name can be one token of a subject and nothing more.
func isToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
