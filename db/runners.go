package db

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// ErrNotThePoolsLabel is a join token asked to permit a label its pool does not carry.
var ErrNotThePoolsLabel = errors.New("db: a join token permits only labels its pool carries")

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

// IssueJoinToken mints one, bound to one pool and one set of labels, issued at one moment and
// good until another.
//
// "bound both to that pool and to the exact set of labels a runner may claim with it", because
// "a machine cannot add zone=lan to itself and start receiving the steps that were kept off the
// internet".
//
// Both moments are the caller's, as a join's is, because the wire writes issued_at "on the clock
// of the API that created it" and the expiry is read against it: a token stamped by one clock and
// timed by another would say it lived a different hour from the one it was given.
func (w *Wide) IssueJoinToken(ctx context.Context, pool string, labels []string, by string, at, until time.Time) (JoinToken, error) {
	switch {
	case pool == "":
		return JoinToken{}, errors.New("db: a join token for no pool")
	case by == "":
		return JoinToken{}, errors.New("db: a join token nobody issued")
	case at.IsZero():
		return JoinToken{}, errors.New("db: a join token issued at no moment")
	case until.IsZero():
		return JoinToken{}, errors.New("db: a join token that never expires, and one only has to survive the minutes between an administrator copying it and a machine presenting it")
	case !until.After(at):
		return JoinToken{}, errors.New("db: a join token that has expired by the moment it is issued")
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
			return JoinToken{}, fmt.Errorf("%w, and pool %s does not carry %s", ErrNotThePoolsLabel, pool, claimed)
		}
	}

	clear, hashed, err := token.New(token.Join, "")
	if err != nil {
		return JoinToken{}, fmt.Errorf("db: %w", err)
	}
	t := JoinToken{
		ID: ulid.New(), Pool: pool, Labels: labels, Clear: clear,
		IssuedBy: by, IssuedAt: at, ExpiresAt: until,
	}
	if _, err := w.tx.Exec(ctx,
		`insert into join_tokens (id, pool, labels, hash, issued_by, issued_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, pool, orEmptyStrings(labels), hashed, by, at, until); err != nil {
		return JoinToken{}, fmt.Errorf("db: the join token could not be recorded: %w", err)
	}
	return t, nil
}

// Joining is what a machine says about itself when it presents a token.
type Joining struct {
	Token  string
	Labels []string

	// PublicKey is the public half of the keypair the host generated, which is the runner's
	// identity across restarts and what a rotation is signed against. The private half never
	// leaves the host.
	PublicKey ed25519.PublicKey

	// What a machine says about itself is what only the machine knows: "the public key, the
	// labels it claims, its AGK_RUNNER_NAMESPACES, its capacity in vCPU, memory and disk, its
	// architecture and its agent version". What it is allowed is its token's and its pool's,
	// and its own namespaces only narrow that.
	CPU          int
	MemoryBytes  int64
	DiskBytes    int64
	Architecture string
	AgentVersion string

	// Namespaces is the host's own narrowing, nil where it narrows nothing. It may name only
	// namespaces its pool accepts: it narrows and never widens.
	Namespaces []string

	// Containment is what the host can prove about how its containers are contained, nil
	// where it said nothing.
	Containment *Containment
}

// Containment is what a host reported at join of how it contains a container: the runtime its
// daemon creates them with, and whether that daemon remaps container root to an unprivileged
// account.
type Containment struct {
	Runtime     string `json:"runtime"`
	UsernsRemap bool   `json:"userns_remap"`
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
	case len(j.PublicKey) != ed25519.PublicKeySize:
		return Joined{}, errors.New("db: a runner is its key, and this one brings no Ed25519 public key")
	case j.CPU < 1 || j.MemoryBytes < 1 || j.DiskBytes < 1:
		return Joined{}, errors.New("db: a runner declares its own capacity, and this one declares none")
	case j.Architecture == "" || j.AgentVersion == "":
		return Joined{}, errors.New("db: a runner says what it is and what it runs")
	case j.Namespaces != nil && len(j.Namespaces) == 0:
		return Joined{}, errors.New("db: a runner narrowed to no namespace could never be handed a task, and one that narrows nothing sends no list")
	case j.Containment != nil && j.Containment.Runtime == "":
		return Joined{}, errors.New("db: a runner reporting its containment names the runtime its daemon uses")
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

	// The host's namespaces narrow its pool's and never widen them, the rule its labels follow:
	// a namespace the pool does not accept is refused with the whole registration rather than
	// dropped, because a machine asking for more than it may have is a machine somebody should
	// look at. A pool that lists none accepts every namespace, so any narrowing of it is one.
	if j.Namespaces != nil {
		var accepted []string
		if err := w.tx.QueryRow(ctx,
			`select accepted_namespaces::text[] from runner_pools where name = $1`, pool).
			Scan(&accepted); err != nil {
			return Joined{}, fmt.Errorf("db: the pool of the join token could not be read: %w", err)
		}
		if len(accepted) > 0 {
			for _, narrowed := range j.Namespaces {
				if !slices.Contains(accepted, narrowed) {
					return Joined{}, fmt.Errorf("%w: it narrows itself to %s, which its pool does not accept", ErrNoJoinToken, narrowed)
				}
			}
		}
	}

	// Minted in the grammar the wire prints a runner in, lowercase, because the identifier is
	// the runner field of every result it publishes and a subject token on the bus, and the
	// result reader holds it to that grammar.
	runner := strings.ToLower(ulid.New())
	clear, credential, err := token.New(token.Runner, "")
	if err != nil {
		return Joined{}, fmt.Errorf("db: %w", err)
	}
	// To the microsecond, which is what PostgreSQL keeps and Authenticate judges by, so that the
	// instant answered is the instant enforced.
	rotate := now.Add(rotateAfter).Truncate(time.Microsecond)

	var runtime *string
	var remap *bool
	if c := j.Containment; c != nil {
		runtime, remap = &c.Runtime, &c.UsernsRemap
	}
	if _, err := w.tx.Exec(ctx,
		`insert into runners (id, pool, labels, public_key, cpu, memory_bytes, disk_bytes,
		                      architecture, agent_version, accepted_namespaces,
		                      containment_runtime, userns_remap,
		                      credential_hash, rotate_by, joined_with)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text[]::namespace_name[], $11, $12, $13, $14, $15)`,
		runner, pool, orEmptyStrings(j.Labels), []byte(j.PublicKey),
		j.CPU, j.MemoryBytes, j.DiskBytes, j.Architecture, j.AgentVersion, j.Namespaces,
		runtime, remap,
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

	// PublicKey is the key the host joined with. It is not answered in the inventory, which
	// lists hosts rather than proves them.
	PublicKey ed25519.PublicKey `json:"-"`

	CPU          int    `json:"cpu"`
	MemoryBytes  int64  `json:"memory_bytes"`
	DiskBytes    int64  `json:"disk_bytes"`
	Architecture string `json:"architecture"`
	AgentVersion string `json:"agent_version"`

	Namespaces  []string     `json:"namespaces,omitempty"`
	Containment *Containment `json:"containment,omitempty"`

	State       string `json:"state"`
	DrainReason string `json:"drain_reason,omitempty"`

	// Who ordered a drain and a revocation, and when, which the row keeps until the audit log
	// records both.
	DrainedBy string    `json:"drained_by,omitempty"`
	DrainedAt time.Time `json:"drained_at,omitzero"`
	RevokedBy string    `json:"revoked_by,omitempty"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`

	// ResultsAcceptedUntil is the end of a revocation's grace: the runner is answered, told to
	// drain, and its results taken until then, and refused everywhere from then on. Zero for a
	// runner nobody revoked.
	ResultsAcceptedUntil time.Time `json:"results_accepted_until,omitzero"`

	// What the runner last said of itself at a heartbeat: what it will do with new work, which
	// is not State (the installation's word on it), and how many tasks it will run at once.
	// Neither is written until its first heartbeat.
	ReportedState string `json:"reported_state,omitempty"`
	Concurrency   int64  `json:"concurrency,omitempty"`

	JoinedAt   time.Time `json:"joined_at"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
	RotateBy   time.Time `json:"rotate_by,omitzero"`
}

// runnerColumns are what scanRunner reads, in its order.
const runnerColumns = `id, pool, labels, public_key, cpu, memory_bytes, disk_bytes,
	architecture, agent_version, accepted_namespaces::text[], containment_runtime, userns_remap,
	state, drain_reason, reported_state, concurrency, joined_at, last_heartbeat_at, rotate_by,
	drained_by, drained_at, revoked_by, revoked_at, results_accepted_until`

// scanRunner reads one row of runnerColumns, and whatever a query selects after them into more.
func scanRunner(row pgx.Row, more ...any) (Runner, error) {
	var r Runner
	var key []byte
	var runtime, reason, reported *string
	var remap *bool
	var concurrency *int64
	var seen, rotate *time.Time
	var drainedBy, revokedBy *string
	var drainedAt, revokedAt, accepted *time.Time
	into := append([]any{&r.ID, &r.Pool, &r.Labels, &key, &r.CPU, &r.MemoryBytes, &r.DiskBytes,
		&r.Architecture, &r.AgentVersion, &r.Namespaces, &runtime, &remap,
		&r.State, &reason, &reported, &concurrency, &r.JoinedAt, &seen, &rotate,
		&drainedBy, &drainedAt, &revokedBy, &revokedAt, &accepted}, more...)
	if err := row.Scan(into...); err != nil {
		return Runner{}, err
	}
	r.PublicKey = ed25519.PublicKey(key)
	if runtime != nil && remap != nil {
		r.Containment = &Containment{Runtime: *runtime, UsernsRemap: *remap}
	}
	if reason != nil {
		r.DrainReason = *reason
	}
	if reported != nil && concurrency != nil {
		r.ReportedState, r.Concurrency = *reported, *concurrency
	}
	if seen != nil {
		r.LastSeenAt = *seen
	}
	if rotate != nil {
		r.RotateBy = *rotate
	}
	if drainedBy != nil && drainedAt != nil {
		r.DrainedBy, r.DrainedAt = *drainedBy, *drainedAt
	}
	if revokedBy != nil && revokedAt != nil && accepted != nil {
		r.RevokedBy, r.RevokedAt, r.ResultsAcceptedUntil = *revokedBy, *revokedAt, *accepted
	}
	return r, nil
}

// Authenticate answers which runner a credential belongs to, and refuses one revoked past its grace
// and one past its rotate_by.
//
// "Revoking a credential never destroys work already done", so a revoked runner is still opened
// until its ResultsAcceptedUntil, as revoked: what that lets it do is each route's to say, and none
// of them lets it take anything new. From that instant it is refused here rather than left to a
// check somewhere else, since after the grace "every call is 401". So is a credential past its
// rotate_by, which is the whole of the rotation window: "A credential past its rotate_by is refused
// everywhere, and that host joins again", because "a machine that has been dark for a month should
// be reconsidered rather than readmitted". The moment is the caller's, as every moment this package
// judges is.
//
// A runner that has rotated holds two credentials until it presents the new one: the one it was
// answered, and the one it rotated with, which is what it still holds if the answer never reached
// it. Either opens the runner, each until its own rotate_by, and the Runner answered carries the
// rotate_by of the one presented. The first time the new one is presented, the old one is forgotten,
// since the runner has then shown it holds the new one and the old one is worth something only to
// whoever else has a copy.
func (w *Wide) Authenticate(ctx context.Context, credential string, now time.Time) (Runner, error) {
	if kind, ok := token.KindOf(credential); !ok || kind != token.Runner {
		return Runner{}, ErrNoRunner
	}
	hashed := token.Hash(credential)
	var current string
	var previousBy *time.Time
	r, err := scanRunner(w.tx.QueryRow(ctx,
		`select `+runnerColumns+`, credential_hash, previous_rotate_by from runners
		 where credential_hash = $1 or previous_credential_hash = $1`, hashed),
		&current, &previousBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, fmt.Errorf("db: the runner could not be read: %w", err)
	}
	isCurrent := current == hashed
	if !isCurrent {
		r.RotateBy = *previousBy
	}
	switch {
	case r.State == "revoked" && !now.Before(r.ResultsAcceptedUntil):
		return Runner{}, ErrNoRunner
	case !now.Before(r.RotateBy):
		return Runner{}, ErrNoRunner
	}

	if isCurrent && previousBy != nil {
		if _, err := w.tx.Exec(ctx,
			`update runners set previous_credential_hash = null, previous_rotate_by = null
			 where id = $1 and credential_hash = $2`, r.ID, hashed); err != nil {
			return Runner{}, fmt.Errorf("db: the credential runner %s rotated from could not be forgotten: %w", r.ID, err)
		}
	}
	return r, nil
}

// ErrNotItsKey is a rotation whose signature is not by the key its runner joined with.
var ErrNotItsKey = errors.New("db: that signature is not by the key the runner joined with")

// ErrRotationReplayed is a rotation signing a request time no later than the last one its runner
// rotated with.
var ErrRotationReplayed = errors.New("db: a rotation signs a later time than the last one did")

// Rotating is what a runner presents to be given a new credential: the credential it holds, and
// proof that it holds the key it joined with.
type Rotating struct {
	// Credential is the one the request was authenticated with.
	Credential string

	// Runner is the runner the request names, which the signature is over.
	Runner string

	// SignedAt is the request time the runner signed, by its own clock.
	SignedAt time.Time

	// Signed is the message the signature is over, as the wire composes it from the runner and
	// the request time, and Signature is the runner's Ed25519 signature of it.
	Signed    []byte
	Signature []byte
}

// Rotated is what it gets back: a credential that exists once, and when that one stops being
// accepted.
type Rotated struct {
	Credential string
	RotateBy   time.Time
}

// Rotate gives a runner a new credential, for the one it holds and a signature by its key.
//
// "The key proves the machine": the credential alone renews nothing, so a copy of it taken off a
// disk or a backup lasts until its rotate_by and no longer, while the host that holds the key keeps
// renewing. The new credential is accepted until now plus rotateAfter, which is how far "rotate_by
// moves". The one presented stays accepted until its own rotate_by or until the new one is first
// presented, whichever comes first, so that an answer lost on the way back locks nobody out: the
// runner rotates again with what it still holds, and the credential it never received is dropped.
// A runner therefore holds at most two, the one it last rotated with and the one that answered.
//
// A revoked runner rotates nothing, in its grace or after it: its credential is ending, and renewing
// it would carry the runner past the grace it was given. A draining one rotates. A drain takes no
// credential away, and a drained runner "stays up", heartbeating for as long as it is left drained,
// which may be longer than its rotate_by: refused, it would be locked out at that instant and have
// to join again, and a drain would be a revocation that took a month.
//
// The runner is locked while it is judged, so that two rotations at once cannot both keep the
// credential they presented: the second waits, and finds what the first left.
func (w *Wide) Rotate(ctx context.Context, ro Rotating, rotateAfter time.Duration, now time.Time) (Rotated, error) {
	if kind, ok := token.KindOf(ro.Credential); !ok || kind != token.Runner {
		return Rotated{}, ErrNoRunner
	}
	hashed := token.Hash(ro.Credential)
	var id, state, current string
	var key []byte
	var rotateBy time.Time
	var previousBy, lastSigned, accepted *time.Time
	err := w.tx.QueryRow(ctx,
		`select id, state, public_key, credential_hash, rotate_by, previous_rotate_by, rotation_signed_at,
		        results_accepted_until
		 from runners
		 where credential_hash = $1 or previous_credential_hash = $1
		 for update`, hashed).
		Scan(&id, &state, &key, &current, &rotateBy, &previousBy, &lastSigned, &accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rotated{}, ErrNoRunner
	}
	if err != nil {
		return Rotated{}, fmt.Errorf("db: the runner could not be read: %w", err)
	}
	if current != hashed {
		rotateBy = *previousBy
	}
	// To the microsecond, which is what PostgreSQL keeps of it. Judged finer than what was kept,
	// the same request sent again would read as a later one, and be good twice.
	signedAt := ro.SignedAt.Truncate(time.Microsecond)
	switch {
	case state == "revoked" && accepted != nil && now.Before(*accepted) && now.Before(rotateBy) && id == ro.Runner:
		// Revoked while the request was under way, and still in its grace: the credential
		// opens what the grace allows, and the runner is told why it renews nothing rather
		// than that it opens nothing, which would send it off to join again.
		return Rotated{}, ErrRunnerRevoked
	case state == "revoked", !now.Before(rotateBy), id != ro.Runner:
		// Everything Authenticate refuses, judged again under the lock, since a rotation
		// that committed after the request was authenticated may have left the credential
		// presented behind.
		return Rotated{}, ErrNoRunner
	case !ed25519.Verify(ed25519.PublicKey(key), ro.Signed, ro.Signature):
		return Rotated{}, ErrNotItsKey
	case lastSigned != nil && !signedAt.After(*lastSigned):
		return Rotated{}, ErrRotationReplayed
	}

	clear, credential, err := token.New(token.Runner, "")
	if err != nil {
		return Rotated{}, fmt.Errorf("db: %w", err)
	}
	rotate := now.Add(rotateAfter).Truncate(time.Microsecond)
	if _, err := w.tx.Exec(ctx,
		`update runners set credential_hash = $2, rotate_by = $3,
		        previous_credential_hash = $4, previous_rotate_by = $5, rotation_signed_at = $6
		 where id = $1`,
		id, credential, rotate, hashed, rotateBy, signedAt); err != nil {
		return Rotated{}, fmt.Errorf("db: runner %s could not be given its new credential: %w", id, err)
	}
	return Rotated{Credential: clear, RotateBy: rotate}, nil
}

// HeartbeatInterval is how often a runner says it is there: "A runner posts one heartbeat every 10
// seconds to the API".
//
// A constant, and not something the answer to a heartbeat tells a runner: the wire's answer is
// closed and carries no interval, and the documentation fixes the figure, so the runner and Lost
// both count in the one the page gives. A runner judged by another interval than the one it
// reports at would be declared lost while it reported on time, or kept long after it had gone.
const HeartbeatInterval = 10 * time.Second

// LostAfter is how long a task in flight may go unaccounted for before Lost moves it: "Three
// missed intervals move a task to lost."
const LostAfter = 3 * HeartbeatInterval

// Beating is what a runner says of itself at every heartbeat, as the wire's request has it: which
// agent it runs, what it will do with new work, how many tasks it will run at once, and the keys it
// is holding.
type Beating struct {
	AgentVersion string

	// State is ready, draining or unhealthy, the runner's own word on what it will do with new
	// work. It is recorded and never obeyed: the installation's word is Runner.State.
	State       string
	Concurrency int64

	// Holding is every key in flight on the host.
	Holding []agk.TaskID
}

// Beaten is what a heartbeat is answered from: the runner as it now stands, and the keys it is to
// stop.
type Beaten struct {
	Runner Runner

	// Cancel is each key the heartbeat named whose dispatch, bound to this runner, the
	// controller has ended as cancelled or timed_out, in the order keys sort in. Never nil, so
	// that the answer is always a list.
	Cancel []agk.TaskID
}

// Beat records that a runner is there and what it says of itself, and answers what it is to stop.
//
// "A runner posts one heartbeat every 10 seconds to the API, listing the idempotency keys it
// currently holds. One request covers every in-flight task on that host." What it writes is the
// moment against every task it named, which is what Lost compares against, and what the runner
// reported of itself, which is what the inventory shows.
//
// What it answers to stop is the backstop for agentiik.stops, which keeps nothing: a runner that
// was disconnected when a stop went out hears it here within one interval, rather than running the
// container to its deadline. So it is each key the runner named whose dispatch bound to it has
// been ended cancelled or timed_out, since those are the two endings the control plane writes
// while a container may still be running. A lost dispatch is never among them, although the host
// may still hold it: a host that was only cut off finishes its key, and the ending it records is
// what answers a requeue that comes back to it. Nor is a dispatch bound to another runner, which
// after a requeue is one more reason the key alone decides nothing.
func (w *Wide) Beat(ctx context.Context, runner string, b Beating, at time.Time) (Beaten, error) {
	switch {
	case b.AgentVersion == "":
		return Beaten{}, errors.New("db: a heartbeat says which agent the runner runs")
	case b.State != "ready" && b.State != "draining" && b.State != "unhealthy":
		return Beaten{}, fmt.Errorf("db: a runner reports itself ready, draining or unhealthy, and not %q", b.State)
	case b.Concurrency < 1:
		return Beaten{}, fmt.Errorf("db: a runner that runs anything runs one task or more at once, and not %d", b.Concurrency)
	}

	// A revoked runner is heard until its grace ends, and its heartbeat keeps its tasks from
	// being declared lost until then, so that what it finishes inside the grace is still
	// taken. From that instant it is heard no more, and three intervals later the sweep finds
	// whatever it was still holding.
	r, err := scanRunner(w.tx.QueryRow(ctx,
		`update runners set last_heartbeat_at = $2, agent_version = $3, reported_state = $4, concurrency = $5
		 where id = $1 and (state <> 'revoked' or results_accepted_until > $2)
		 returning `+runnerColumns,
		runner, at, b.AgentVersion, b.State, b.Concurrency))
	if errors.Is(err, pgx.ErrNoRows) {
		return Beaten{}, ErrNoRunner
	}
	if err != nil {
		return Beaten{}, fmt.Errorf("db: the heartbeat could not be recorded: %w", err)
	}

	beaten := Beaten{Runner: r, Cancel: []agk.TaskID{}}
	if len(b.Holding) == 0 {
		return beaten, nil
	}
	keys := make([]string, len(b.Holding))
	for i, k := range b.Holding {
		keys[i] = string(k)
	}

	// Only the tasks this runner is actually holding. A heartbeat naming somebody else's task
	// is a heartbeat keeping somebody else's task alive, which is the one thing a liveness
	// report must not be able to do.
	if _, err := w.tx.Exec(ctx, `
		update tasks set last_heartbeat_at = $3
		where runner = $1 and idempotency_key = any($2)
		  and state in ('dispatched', 'running', 'publishing')`,
		runner, keys, at); err != nil {
		return Beaten{}, fmt.Errorf("db: what the runner is holding could not be recorded: %w", err)
	}

	// And only this runner's dispatches, for the same reason: a key names every dispatch of it,
	// and one bound elsewhere is no order to this host.
	rows, err := w.tx.Query(ctx, `
		select distinct idempotency_key from tasks
		where runner = $1 and idempotency_key = any($2)
		  and state in ('cancelled', 'timed_out')
		order by idempotency_key`,
		runner, keys)
	if err != nil {
		return Beaten{}, fmt.Errorf("db: what the runner is to stop could not be read: %w", err)
	}
	cancel, err := pgx.CollectRows(rows, pgx.RowTo[agk.TaskID])
	if err != nil {
		return Beaten{}, fmt.Errorf("db: what the runner is to stop could not be read: %w", err)
	}
	if len(cancel) > 0 {
		beaten.Cancel = cancel
	}
	return beaten, nil
}

// ErrRunnerRevoked is an order or a rotation for a runner that is revoked and still in its grace:
// a drain would only undo part of the revocation, and a rotation would carry the runner past it.
var ErrRunnerRevoked = errors.New("db: that runner is revoked")

// Drain tells a runner to stop taking work and finish what it holds, and answers the runner as it
// now stands.
//
// "Results are accepted as usual; the runner takes nothing new but stays up." Who ordered it, when
// and why are written on the row, where they are kept until the audit log records the order, and
// the reason is what the heartbeat hands the runner for its own log. A runner already draining is
// answered as it stands, since the first order is the one it is obeying and a second would change
// nothing it does. A revoked one is refused with ErrRunnerRevoked, because a drain is less than it
// was already told and answering it would read as though the revocation had been lifted, and one
// that is not there with ErrNoRunner.
func (w *Wide) Drain(ctx context.Context, runner, by, why string, at time.Time) (Runner, error) {
	switch {
	case by == "":
		return Runner{}, errors.New("db: a drain nobody ordered")
	case why == "":
		return Runner{}, errors.New("db: a drain for no reason, and the reason is what the runner writes to its own log")
	case at.IsZero():
		return Runner{}, errors.New("db: a drain ordered at no moment")
	}
	r, err := scanRunner(w.tx.QueryRow(ctx,
		`update runners set state = 'draining', drain_reason = $2, drained_by = $3, drained_at = $4
		 where id = $1 and state = 'ready'
		 returning `+runnerColumns,
		runner, why, by, at))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, fmt.Errorf("db: runner %s could not be drained: %w", runner, err)
	}
	if r, err = w.runnerNamed(ctx, runner); err != nil {
		return Runner{}, err
	}
	if r.State == "revoked" {
		return Runner{}, ErrRunnerRevoked
	}
	return r, nil
}

// Revoke stops a runner taking anything new at once, and stops its credential being accepted at all
// once grace has passed, and answers the runner as it now stands.
//
// "Revoking a credential never destroys work already done." Until the revocation plus the grace,
// ResultsAcceptedUntil, the runner is still heard at the heartbeat and told to drain, may publish
// the results of what it holds and hear stops, and redeems nothing; from then on every call it
// makes is refused, and the sweep declares lost whatever it still holds. The end of the grace is
// written as an instant, so that an installation changing its setting afterwards moves no grace
// already given. A runner already revoked is answered as it stands, since revoking it again would
// otherwise be a way of lengthening its grace, and one that is not there is ErrNoRunner.
func (w *Wide) Revoke(ctx context.Context, runner, by, why string, at time.Time, grace time.Duration) (Runner, error) {
	switch {
	case by == "":
		return Runner{}, errors.New("db: a revocation nobody ordered")
	case why == "":
		return Runner{}, errors.New("db: a revocation for no reason, and the reason is what the runner writes to its own log")
	case at.IsZero():
		return Runner{}, errors.New("db: a revocation ordered at no moment")
	}
	// To the microsecond, which is what PostgreSQL keeps and Authenticate judges by, so that the
	// instant the heartbeat answers is the instant enforced.
	at = at.Truncate(time.Microsecond)
	until := at.Add(grace).Truncate(time.Microsecond)
	if !until.After(at) {
		return Runner{}, fmt.Errorf("db: a revocation grace of %s, and revoking a runner never destroys work already done: a grace of no time refuses the results of what it is finishing", grace)
	}
	r, err := scanRunner(w.tx.QueryRow(ctx,
		`update runners set state = 'revoked', drain_reason = $2, revoked_by = $3, revoked_at = $4,
		        results_accepted_until = $5
		 where id = $1 and state <> 'revoked'
		 returning `+runnerColumns,
		runner, why, by, at, until))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, fmt.Errorf("db: runner %s could not be revoked: %w", runner, err)
	}
	return w.runnerNamed(ctx, runner)
}

// runnerNamed reads one runner as the inventory holds it, and answers ErrNoRunner for one that is
// not there.
func (w *Wide) runnerNamed(ctx context.Context, runner string) (Runner, error) {
	r, err := scanRunner(w.tx.QueryRow(ctx, `select `+runnerColumns+` from runners where id = $1`, runner))
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, fmt.Errorf("db: runner %s could not be read: %w", runner, err)
	}
	return r, nil
}

// Runners is the inventory.
func (w *Wide) Runners(ctx context.Context) ([]Runner, error) {
	rows, err := w.tx.Query(ctx, `select `+runnerColumns+` from runners order by pool, id`)
	if err != nil {
		return nil, fmt.Errorf("db: the runners could not be read: %w", err)
	}
	defer rows.Close()

	out := []Runner{}
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
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
// Only a task some runner holds, which is one a runner has redeemed the grant of. A task nobody has
// redeemed is a message waiting on the queue for a runner with room, or one a runner took and has
// not redeemed yet, and so has not acknowledged either, since a runner acknowledges only once it
// has redeemed: should it die there, the bus hands the message to another runner of the pool once
// AckWait has passed. Or it is a requeue that came back to the host which had already ended its
// key, which nobody redeems: that host publishes the ending it recorded before it acknowledges the
// message, so the ending is on the result stream by the time the message leaves the queue, and
// binds the host's runner as it is written. Either way the task is the bus's until it is redeemed
// or answered, and a busy pool or an empty one keeps it waiting as long as it likes; it is not
// lost, because "lost: The runner holding it stopped reporting" and nothing holds it. Declared
// lost, it would be requeued into the queue it was already waiting on, one row and one message on
// every sweep for as long as the pool stayed full, and a step that does not requeue would fail for
// having waited.
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
// requeued while its container ran. A runner that redeemed a task and was never heard from again,
// one that died before it could acknowledge the message included, is still exactly the case this is
// for, counted from its redemption. The redemption is the latest of its grants', since a task may
// have been issued several and a runner redeems one: joined on each, the one nobody redeemed would
// count the task from its dispatch.
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
