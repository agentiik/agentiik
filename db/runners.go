package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// The runner inventory, which belongs to the installation.
//
// A runner serves several namespaces, its inventory is administrator only, and a heartbeat
// "covers every in-flight task on that host", which is one host across however many namespaces it
// is working for. So all of this is on the installation door.

// ErrNoRunner is nothing of that identifier, or nothing that credential opens. One error for
// both, for the reason ErrNoGrant is one error for three.
var ErrNoRunner = errors.New("db: no runner of that identifier")

// ErrNoJoinToken is a join token that is wrong, spent or expired.
var ErrNoJoinToken = errors.New("db: that join token cannot be redeemed")

// JoinToken is what an administrator hands to a machine that is about to become a runner.
type JoinToken struct {
	ID     string
	Pool   string
	Labels []string

	// Clear exists once, in the answer to the request that created it.
	Clear     string
	IssuedBy  string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// IssueJoinToken mints one, bound to one pool and one set of labels.
//
// "bound both to that pool and to the exact set of labels a runner may claim with it", because
// "a machine cannot add zone=lan to itself and start receiving the steps that were kept off the
// internet".
func (w *Wide) IssueJoinToken(ctx context.Context, pool string, labels []string, by string, until time.Time) (JoinToken, error) {
	switch {
	case pool == "":
		return JoinToken{}, errors.New("db: a join token for no pool")
	case by == "":
		return JoinToken{}, errors.New("db: a join token nobody issued")
	case until.IsZero():
		return JoinToken{}, errors.New("db: a join token that never expires, and one only has to survive the minutes between an administrator copying it and a machine presenting it")
	}

	// A token draws its labels from its pool, which is where somebody wrote them down. A
	// token permitting a label the pool does not carry would be a way of granting a label
	// without ever having created it, and the subset check at join time would then be
	// checking against nothing.
	p, err := w.RunnerPoolNamed(ctx, pool)
	if err != nil {
		return JoinToken{}, err
	}
	for _, claimed := range labels {
		if !slices.Contains(p.Labels, claimed) {
			return JoinToken{}, fmt.Errorf("db: pool %s does not carry the label %s", pool, claimed)
		}
	}

	clear, hashed, err := token.New(token.Join, "")
	if err != nil {
		return JoinToken{}, fmt.Errorf("db: %w", err)
	}
	t := JoinToken{
		ID: ulid.New(), Pool: pool, Labels: labels, Clear: clear,
		IssuedBy: by, IssuedAt: time.Now().UTC(), ExpiresAt: until,
	}
	if _, err := w.tx.Exec(ctx,
		`insert into join_tokens (id, pool, labels, hash, issued_by, expires_at)
		 values ($1, $2, $3, $4, $5, $6)`,
		t.ID, pool, orEmptyStrings(labels), hashed, by, until); err != nil {
		return JoinToken{}, fmt.Errorf("db: the join token could not be recorded: %w", err)
	}
	return t, nil
}

// Joining is what a machine says about itself when it presents a token.
type Joining struct {
	Token  string
	Labels []string

	// What a machine says about itself is what only the machine knows. What it is allowed
	// is its pool's: "the labels it claims, its capacity in vCPU, memory and disk, its
	// architecture and its agent version" is the whole of what registration carries.
	CPU          int
	MemoryBytes  int64
	DiskBytes    int64
	Architecture string
	AgentVersion string
}

// Joined is what it gets back: an identity, and a credential that exists once.
type Joined struct {
	Runner     string
	Pool       string
	Credential string
	RotateBy   time.Time
}

// Join redeems a token and creates the runner.
//
// Everything about it is checked against the token rather than believed: the pool is the token's,
// and the labels have to be a subset of what the token permits. A machine that claimed more is
// refused rather than trimmed, because trimming would let it join with less than it asked for and
// then wonder why it is being offered nothing.
func (w *Wide) Join(ctx context.Context, j Joining, rotateAfter time.Duration, now time.Time) (Joined, error) {
	switch {
	case j.CPU < 1 || j.MemoryBytes < 1 || j.DiskBytes < 1:
		return Joined{}, errors.New("db: a runner declares its own capacity, and this one declares none")
	case j.Architecture == "" || j.AgentVersion == "":
		return Joined{}, errors.New("db: a runner says what it is and what it runs")
	}

	var id, pool, hashed string
	var permitted []string
	var expires time.Time
	var redeemed *time.Time
	err := w.tx.QueryRow(ctx,
		`select id, pool, labels, hash, expires_at, redeemed_at from join_tokens
		 where hash = $1 for update`, token.Hash(j.Token)).
		Scan(&id, &pool, &permitted, &hashed, &expires, &redeemed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Joined{}, ErrNoJoinToken
	}
	if err != nil {
		return Joined{}, fmt.Errorf("db: the join token could not be read: %w", err)
	}
	switch {
	case redeemed != nil:
		// Spent. "one machine, one token, and a reimaged host joins again."
		return Joined{}, ErrNoJoinToken
	case !now.Before(expires):
		return Joined{}, ErrNoJoinToken
	}

	for _, claimed := range j.Labels {
		if !slices.Contains(permitted, claimed) {
			return Joined{}, fmt.Errorf("%w: it claims %s, which its token does not permit", ErrNoJoinToken, claimed)
		}
	}

	runner := ulid.New()
	clear, credential, err := token.New(token.Runner, "")
	if err != nil {
		return Joined{}, fmt.Errorf("db: %w", err)
	}
	rotate := now.Add(rotateAfter)

	if _, err := w.tx.Exec(ctx,
		`insert into runners (id, pool, labels, cpu, memory_bytes, disk_bytes,
		                      architecture, agent_version, credential_hash, rotate_by, joined_with)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		runner, pool, orEmptyStrings(j.Labels),
		j.CPU, j.MemoryBytes, j.DiskBytes, j.Architecture, j.AgentVersion,
		credential, rotate, id); err != nil {
		return Joined{}, fmt.Errorf("db: the runner could not be created: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`update join_tokens set redeemed_at = $2, redeemed_by = $3 where id = $1`,
		id, now, runner); err != nil {
		return Joined{}, fmt.Errorf("db: the join token could not be spent: %w", err)
	}
	return Joined{Runner: runner, Pool: pool, Credential: clear, RotateBy: rotate}, nil
}

// Runner is one host, as the inventory holds it.
type Runner struct {
	ID     string   `json:"runner"`
	Pool   string   `json:"pool"`
	Labels []string `json:"labels"`

	CPU          int    `json:"cpu"`
	MemoryBytes  int64  `json:"memory_bytes"`
	DiskBytes    int64  `json:"disk_bytes"`
	Architecture string `json:"architecture"`
	AgentVersion string `json:"agent_version"`

	State       string    `json:"state"`
	DrainReason string    `json:"drain_reason,omitempty"`
	JoinedAt    time.Time `json:"joined_at"`
	LastSeenAt  time.Time `json:"last_seen_at,omitzero"`
	RotateBy    time.Time `json:"rotate_by,omitzero"`
}

// Authenticate answers which runner a credential belongs to, and refuses a revoked one.
//
// "revoking it from the console stops the runner at its next heartbeat", so a revoked credential
// is refused here rather than left to a check somewhere else.
func (w *Wide) Authenticate(ctx context.Context, credential string) (Runner, error) {
	if kind, ok := token.KindOf(credential); !ok || kind != token.Runner {
		return Runner{}, ErrNoRunner
	}
	var r Runner
	var reason *string
	var seen, rotate *time.Time
	err := w.tx.QueryRow(ctx, `
		select id, pool, labels, cpu, memory_bytes, disk_bytes,
		       architecture, agent_version, state, drain_reason, joined_at, last_heartbeat_at, rotate_by
		from runners where credential_hash = $1`, token.Hash(credential)).
		Scan(&r.ID, &r.Pool, &r.Labels, &r.CPU, &r.MemoryBytes, &r.DiskBytes,
			&r.Architecture, &r.AgentVersion, &r.State, &reason, &r.JoinedAt, &seen, &rotate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, fmt.Errorf("db: the runner could not be read: %w", err)
	}
	if r.State == "revoked" {
		return Runner{}, ErrNoRunner
	}
	if reason != nil {
		r.DrainReason = *reason
	}
	if seen != nil {
		r.LastSeenAt = *seen
	}
	if rotate != nil {
		r.RotateBy = *rotate
	}
	return r, nil
}

// HeartbeatInterval is how often a runner says it is there: "A runner posts one heartbeat every 10
// seconds to the API".
//
// One constant, because two things read it and they must not disagree. The API tells a runner the
// interval in the answer to every heartbeat, and Lost counts silence in it: a runner told one
// interval and judged by another would be declared lost while it reported on time, or kept long
// after it had gone.
const HeartbeatInterval = 10 * time.Second

// LostAfter is how long a task in flight may go unaccounted for before Lost moves it: "Three
// missed intervals move a task to lost."
const LostAfter = 3 * HeartbeatInterval

// Beat records that a runner is there and says what it is holding.
//
// "A runner posts one heartbeat every 10 seconds to the API, listing the idempotency keys it
// currently holds. One request covers every in-flight task on that host." What it writes is the
// moment against every task it named, which is what Lost compares against, and what it answers is
// whether the runner should be draining.
func (w *Wide) Beat(ctx context.Context, runner string, holding []agk.TaskID, at time.Time) (Runner, error) {
	var r Runner
	var reason *string
	var seen, rotate *time.Time
	err := w.tx.QueryRow(ctx, `
		update runners set last_heartbeat_at = $2 where id = $1 and state <> 'revoked'
		returning id, pool, labels, cpu, memory_bytes, disk_bytes,
		          architecture, agent_version, state, drain_reason, joined_at, last_heartbeat_at, rotate_by`,
		runner, at).
		Scan(&r.ID, &r.Pool, &r.Labels, &r.CPU, &r.MemoryBytes, &r.DiskBytes,
			&r.Architecture, &r.AgentVersion, &r.State, &reason, &r.JoinedAt, &seen, &rotate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, fmt.Errorf("db: the heartbeat could not be recorded: %w", err)
	}
	if reason != nil {
		r.DrainReason = *reason
	}
	if seen != nil {
		r.LastSeenAt = *seen
	}
	if rotate != nil {
		r.RotateBy = *rotate
	}

	if len(holding) > 0 {
		keys := make([]string, len(holding))
		for i, k := range holding {
			keys[i] = string(k)
		}
		// Only the tasks this runner is actually holding. A heartbeat naming somebody
		// else's task is a heartbeat keeping somebody else's task alive, which is the one
		// thing a liveness report must not be able to do.
		if _, err := w.tx.Exec(ctx, `
			update tasks set last_heartbeat_at = $3
			where runner = $1 and idempotency_key = any($2)
			  and state in ('dispatched', 'running', 'publishing')`,
			runner, keys, at); err != nil {
			return Runner{}, fmt.Errorf("db: what the runner is holding could not be recorded: %w", err)
		}
	}
	return r, nil
}

// Drain tells a runner to stop taking work and finish what it holds.
func (w *Wide) Drain(ctx context.Context, runner, why string) error {
	if _, err := w.tx.Exec(ctx,
		`update runners set state = 'draining', drain_reason = $2 where id = $1 and state = 'ready'`,
		runner, nilIfEmpty(why)); err != nil {
		return fmt.Errorf("db: runner %s could not be drained: %w", runner, err)
	}
	return nil
}

// Revoke stops a runner's credential being accepted at all.
func (w *Wide) Revoke(ctx context.Context, runner, why string) error {
	if _, err := w.tx.Exec(ctx,
		`update runners set state = 'revoked', drain_reason = $2 where id = $1`,
		runner, nilIfEmpty(why)); err != nil {
		return fmt.Errorf("db: runner %s could not be revoked: %w", runner, err)
	}
	return nil
}

// Runners is the inventory.
func (w *Wide) Runners(ctx context.Context) ([]Runner, error) {
	rows, err := w.tx.Query(ctx, `
		select id, pool, labels, cpu, memory_bytes, disk_bytes,
		       architecture, agent_version, state, drain_reason, joined_at, last_heartbeat_at, rotate_by
		from runners order by pool, id`)
	if err != nil {
		return nil, fmt.Errorf("db: the runners could not be read: %w", err)
	}
	defer rows.Close()

	out := []Runner{}
	for rows.Next() {
		var r Runner
		var reason *string
		var seen, rotate *time.Time
		if err := rows.Scan(&r.ID, &r.Pool, &r.Labels, &r.CPU, &r.MemoryBytes,
			&r.DiskBytes, &r.Architecture, &r.AgentVersion, &r.State, &reason, &r.JoinedAt, &seen, &rotate); err != nil {
			return nil, err
		}
		if reason != nil {
			r.DrainReason = *reason
		}
		if seen != nil {
			r.LastSeenAt = *seen
		}
		if rotate != nil {
			r.RotateBy = *rotate
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Lost moves the tasks of runners that stopped reporting, as of now, and answers how many it
// moved.
//
// "Three missed intervals move a task to lost, which is the state whose consequences Lifecycle and
// replay describes." A task is lost rather than failed, and the distinction is the whole of why
// this exists: "A failed task is charged to the brick and follows the retry policy; a lost task is
// charged to the infrastructure and is requeued only when the step is declared idempotent, since
// it may well have completed without the result coming back."
//
// It is the controller's sweep and judges by the controller's clock, though not everything it
// judges was stamped by that clock: the dispatch was, and the heartbeat and the redemption were
// stamped by the API's. So it rests on the two agreeing, as a grant already does, whose expiry the
// controller sets and the API judges. A heartbeat is at most one interval old when the next one
// lands, so a controller less than two intervals ahead of the API still finds a runner reporting
// on time, and one behind it declares a loss that much later. Judging by the database's clock
// would not have removed the pairing, only added a third clock to it.
//
// Only a task some runner holds, which is one a runner has redeemed the grant of. A task nobody
// has redeemed is a message waiting on the queue for a runner with room, and a busy pool or an
// empty one keeps it waiting as long as it likes; it is not lost, because "lost: The runner
// holding it stopped reporting" and nothing holds it. Declared lost, it would be requeued into the
// queue it was already waiting on, one row and one message on every sweep for as long as the pool
// stayed full, and a step that does not requeue would fail for having waited.
//
// Held is read as bound to a runner, and a task in flight is bound by a redemption and by nothing
// else. A runner is also bound to a task it reports never reached a container, and that binding
// is written with the ending, in one transaction, so the task is over by the time it is bound and
// never in flight with a runner that did not redeem it.
//
// A task that is held counts from the last thing its runner said about it: its last heartbeat, or
// its redemption where no heartbeat has named it yet, and the dispatch only where neither is
// recorded. Not from the dispatch alone, which is when the message went on the queue: a task
// redeemed after a long wait would be lost between its redemption and its first heartbeat, and
// requeued while its container ran. A runner that took work and was never heard from again is
// still exactly the case this is for, counted from when it took it. The redemption is the latest
// of its grants', since a task may have been issued several and a runner redeems one: joined on
// each, the one nobody redeemed would count the task from its dispatch.
//
// The runs are locked before their tasks, which is the order a decision takes them in:
// SaveDecision updates the run and then writes each of its tasks. Taken the other way round, a
// sweep holding a task and waiting on its run, while a decision held the run and reached the task,
// would be a deadlock, and PostgreSQL would end one of the two. Neither is waited on where somebody
// else holds it: a run being decided is passed over with its tasks, a task that a redemption, a
// heartbeat or a runner's own loss holds is passed over alone, and each is judged again on the next
// sweep. A sweep that waited would wait on whatever that was, and every run behind it with it.
//
// The tasks are locked by one statement and judged by the next. PostgreSQL rechecks a row that
// changed before it could be locked against the row as it now stands and every other table as it
// stood, so a redemption that committed in between would be judged with its binding and without its
// grant, and counted from the dispatch. Judged once they are locked, a task redeemed before the
// lock counts from its redemption, and a redemption after it waits and finds the task lost.
//
// What it writes is the dispatch's row and the run's wake, and nothing about a requeue. Whether
// the task is handed out again is the evaluator's to say, and the controller hears of the loss
// through Losses on the pass the wake brings round.
func (w *Wide) Lost(ctx context.Context, now time.Time, batch int) (int, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-LostAfter)

	namespaces, runs, err := w.pairs(ctx, `
		select r.namespace, r.id from runs r
		where (r.namespace, r.id) in (
		  select t.namespace, t.run_id from tasks t
		  where `+held+` and `+heardFrom+` < $1
		  order by `+heardFrom+`
		  limit $2)
		for update of r skip locked`, cutoff, batch)
	if err != nil {
		return 0, fmt.Errorf("db: the runs of the lost tasks could not be locked: %w", err)
	}
	if len(runs) == 0 {
		return 0, nil
	}

	namespaces, ids, err := w.pairs(ctx, `
		select t.namespace, t.id from tasks t
		where (t.namespace, t.run_id) in (select * from unnest($1::text[], $2::text[]))
		  and `+held+` and `+heardFrom+` < $3
		order by `+heardFrom+`
		limit $4
		for update of t skip locked`, namespaces, runs, cutoff, batch)
	if err != nil {
		return 0, fmt.Errorf("db: the lost tasks could not be found: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var lost int
	if err := w.tx.QueryRow(ctx, `
		with gone as (
		  update tasks t set state = 'lost', finished_at = $3
		  where (t.namespace, t.id) in (select * from unnest($1::text[], $2::text[]))
		    and `+held+` and `+heardFrom+` < $4
		  returning t.namespace, t.run_id
		), woken as (
		  update runs r set wake_at = $3
		  from (select distinct namespace, run_id from gone) g
		  where r.namespace = g.namespace and r.id = g.run_id
		    and r.state in ('queued', 'running', 'waiting')
		  returning 1
		)
		select (select count(*) from gone)::int`,
		namespaces, ids, now, cutoff).Scan(&lost); err != nil {
		return 0, fmt.Errorf("db: the lost tasks could not be moved: %w", err)
	}
	return lost, nil
}

// held is a task in flight that a runner is bound to, which is the only kind Lost moves.
const held = `t.state in ('dispatched', 'running', 'publishing') and t.runner is not null`

// heardFrom is the last moment the runner holding a task said anything of it, which is what Lost
// counts from: its last heartbeat, or its latest redemption where none has named it yet, and the
// dispatch only where neither is recorded.
const heardFrom = `coalesce(greatest(t.last_heartbeat_at,
	(select max(g.redeemed_at) from task_grants g
	 where g.namespace = t.namespace and g.task_id = t.id)),
	t.dispatched_at)`

// pairs reads a query answering a namespace and an identifier per row, as the two arrays the next
// statement takes them back as.
func (w *Wide) pairs(ctx context.Context, query string, args ...any) ([]string, []string, error) {
	rows, err := w.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var namespaces, ids []string
	for rows.Next() {
		var namespace, id string
		if err := rows.Scan(&namespace, &id); err != nil {
			return nil, nil, err
		}
		namespaces, ids = append(namespaces, namespace), append(ids, id)
	}
	return namespaces, ids, rows.Err()
}
