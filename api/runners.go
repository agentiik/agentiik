package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// The routes a runner reaches, and no others.
//
// "Serve the runner-facing routes and no others to a runner." What a runner may do is a short
// closed list: join, say it is there, redeem a grant, ship a log. It holds no permission and
// reaches nothing else, which is what makes a compromised one cost "the tasks in its hands and
// the namespaces its policy accepts".

// RunnerOptions are what the runner half of the API is given.
type RunnerOptions struct {
	Pool *db.Pool

	// JoinRotation is how long a runner credential is accepted for. "Rotation is automatic
	// and the agent renews well ahead of it; a runner that was offline past this instant has
	// to join again, which is deliberate, because a machine that has been dark for a month
	// should be reconsidered rather than readmitted." The page fixes no interval; thirty
	// days is what this installs with and what an installation overrides.
	JoinRotation time.Duration

	// What a redemption answers with. Objects and URLs go together: one reads the input
	// envelopes so that the artifacts they name can be resolved, the other mints the URLs
	// that fetch them and the files of the task's tree, and the policy the task writes what
	// it makes with. Without both there is nothing for a runner to redeem into, and the
	// route says so rather than answering an empty object.
	Objects artifact.Objects
	URLs    artifact.Presigner
	Secrets Secrets
	Limits  agk.Limits

	// What a runner reaches the task bus with, and what makes sure its pool has
	// somewhere to pull from.
	BusIssuer    BusIssuer
	BusConsumers BusConsumers

	// Trouble is where a redemption says why a secret was not given. The runner is told only
	// which secret, and the reason, which names the namespace, the secret and the store and
	// never a value, goes to whoever runs the installation, who is the one able to act on it.
	// A field rather than a package level logger for the reason the controller's Trouble is,
	// and one with nowhere to put it drops it rather than choosing for the installation.
	Trouble func(err error)

	Now func() time.Time
}

// RunnerAPI is the runner half of the API.
type RunnerAPI struct {
	pool      *db.Pool
	rotation  time.Duration
	objects   artifact.Objects
	urls      artifact.Presigner
	secrets   Secrets
	limits    agk.Limits
	issuer    BusIssuer
	consumers BusConsumers
	trouble   func(error)
	now       func() time.Time
}

// report says one thing, through whatever Trouble was given.
func (s *RunnerAPI) report(err error) {
	if s.trouble == nil {
		return
	}
	s.trouble(err)
}

// NewRunners registers the runner routes on a router and answers what checks their credentials.
func NewRunners(rt *Router, o RunnerOptions) (*RunnerAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and a runner is a row")
	}
	if o.JoinRotation <= 0 {
		o.JoinRotation = 30 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Secrets == nil {
		o.Secrets = NoSecrets{}
	}
	if o.Limits == (agk.Limits{}) {
		o.Limits = agk.DefaultLimits()
	}
	s := &RunnerAPI{
		pool: o.Pool, rotation: o.JoinRotation,
		objects: o.Objects, urls: o.URLs, secrets: o.Secrets, limits: o.Limits,
		issuer: o.BusIssuer, consumers: o.BusConsumers,
		trouble: o.Trouble, now: o.Now,
	}
	rt.ServeRunners(s)

	// Registration is the one route outside both hooks, and it says why.
	if err := rt.Handle("POST", "/api/v1/runners", Public{
		Why: "registration is authenticated by the join token in its body and by nothing else, because a machine that has not joined holds no credential yet and the token is bound to one pool, one label set and one use",
	}, s.join); err != nil {
		return nil, err
	}
	for _, r := range []struct {
		method  string
		pattern string
		handler RunnerHandler
	}{
		{"POST", "/api/v1/runners/heartbeat", s.beat},
		{"POST", "/api/v1/tasks/redeem", s.redeem},
		{"POST", "/api/v1/bus/token", s.busToken},
	} {
		if err := rt.HandleRunner(r.method, r.pattern, ForRunner{}, r.handler); err != nil {
			return nil, err
		}
	}

	// And the administrator's half: the inventory, which is the installation's rather than a
	// runner's ("A user never learns which host executed a task beyond its runner name and
	// labels"), the pools, and the tokens that let a machine into one.
	admin := Needs{Permission: GrantManage, Scope: Installation}
	for _, r := range []struct {
		method  string
		pattern string
		handler Handler
	}{
		{"GET", "/api/v1/runners", s.inventory},
		{"POST", "/api/v1/runner-pools", s.createPool},
		{"GET", "/api/v1/runner-pools", s.pools},
		{"POST", "/api/v1/runner-pools/{pool}/join-tokens", s.issue},
	} {
		if err := rt.Handle(r.method, r.pattern, admin, r.handler); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Runner says which runner a credential belongs to. It fills the router's Runners.
func (s *RunnerAPI) Runner(ctx context.Context, credential string) (Runner, error) {
	var found db.Runner
	err := s.pool.Installation(ctx, db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		found, err = w.Authenticate(ctx, credential)
		return err
	})
	if errors.Is(err, db.ErrNoRunner) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, err
	}
	return Runner{ID: found.ID, Pool: found.Pool, State: found.State}, nil
}

// Join is what a machine presents.
type Join struct {
	Token  string   `json:"token"`
	Labels []string `json:"labels,omitempty"`

	// "the labels it claims, its capacity in vCPU, memory and disk, its architecture and
	// its agent version". What work the machine will accept is not in that list and is not
	// the machine's to say: it is its pool's.
	CPU          int    `json:"cpu"`
	MemoryBytes  int64  `json:"memory_bytes"`
	DiskBytes    int64  `json:"disk_bytes"`
	Architecture string `json:"architecture"`
	AgentVersion string `json:"agent_version"`
}

func (s *RunnerAPI) join(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var j Join
	if err := read(r, &j); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var joined db.Joined
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		joined, err = wide.Join(ctx, db.Joining{
			Token: j.Token, Labels: j.Labels,
			CPU: j.CPU, MemoryBytes: j.MemoryBytes, DiskBytes: j.DiskBytes,
			Architecture: j.Architecture, AgentVersion: j.AgentVersion,
		}, s.rotation, s.now())
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoJoinToken):
		// Wrong, spent, expired, or claiming a label it may not: one answer for all of
		// them, because a machine that gets a different answer for each is a machine
		// somebody is using to find out which tokens exist.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that join token cannot be redeemed")
		return
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// "A refused registration is an error and never this object with its fields left empty."
	write(w, http.StatusCreated, map[string]any{
		"runner":     joined.Runner,
		"pool":       joined.Pool,
		"credential": joined.Credential,
		"rotate_by":  joined.RotateBy.Format(time.RFC3339Nano),
	})
}

// Beat is what a runner says every interval: the keys it is holding, and nothing else.
type Beat struct {
	Tasks []agk.TaskID `json:"tasks"`
}

func (s *RunnerAPI) beat(w http.ResponseWriter, r *http.Request, runner Runner) {
	var b Beat
	if err := read(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var state db.Runner
	err := s.pool.Installation(r.Context(), db.Heartbeat, func(ctx context.Context, wide *db.Wide) error {
		var err error
		state, err = wide.Beat(ctx, runner.ID, b.Tasks, s.now())
		return err
	})
	if errors.Is(err, db.ErrNoRunner) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that credential opens nothing")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "the heartbeat could not be recorded")
		return
	}

	// "The heartbeat is also how a revoked credential takes effect and how the console knows
	// a runner is present: there is no separate liveness channel to keep in sync."
	answer := map[string]any{"interval_seconds": 10}
	if state.State == "draining" {
		answer["drain"] = true
		if state.DrainReason != "" {
			answer["reason"] = state.DrainReason
		}
	}
	write(w, http.StatusOK, answer)
}

func (s *RunnerAPI) inventory(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var runners []db.Runner
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		runners, err = wide.Runners(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the runners could not be read")
		return
	}
	write(w, http.StatusOK, map[string]any{"runners": runners})
}
