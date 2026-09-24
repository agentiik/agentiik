package api

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// Join is what a machine presents, in the shape wire.schema.json gives it:
// $defs/runnerRegistration/request.
//
// "the public key, the labels it claims, its AGK_RUNNER_NAMESPACES, its capacity in vCPU, memory
// and disk, its architecture and its agent version". What work the machine will accept is its
// pool's to say, and the host's namespaces only narrow that. The pool is not claimed at all: it
// travels in the token, "so that a machine cannot join a pool by naming it".
type Join struct {
	Token        string    `json:"token"`
	PublicKey    string    `json:"public_key"`
	Labels       []string  `json:"labels"`
	Capacity     *Capacity `json:"capacity"`
	Architecture string    `json:"architecture"`
	AgentVersion string    `json:"agent_version"`

	// Namespaces is sent "only when it accepts fewer than its pool does", and nil narrows
	// nothing.
	Namespaces []string `json:"namespaces,omitempty"`

	// Containment is optional, as the wire has it: the floor is held by the runner's own
	// refusal to take work on a daemon without the remapping, and this is what the inventory
	// shows of it.
	Containment *Containment `json:"containment,omitempty"`
}

// Capacity is what the machine has, "as the agent measured it on the host rather than as an
// operator typed it", memory and disk in the grammar a step writes its own memory in.
type Capacity struct {
	VCPU   int    `json:"vcpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

// Containment is what the host can prove about how a container will be contained on it.
type Containment struct {
	Runtime     string `json:"runtime"`
	UsernsRemap bool   `json:"userns_remap"`

	// remapWritten is whether userns_remap was written, since false is a value a running
	// runner reports and the wire requires one or the other.
	remapWritten bool
}

// The grammars the wire holds a join to, copied from wire.schema.json and held to the patterns it
// writes by a test, as a pool's are.
var (
	// publicKeyForm is "PEM around a SubjectPublicKeyInfo", one block and nothing around it.
	publicKeyForm = regexp.MustCompile(`^-----BEGIN PUBLIC KEY-----\n[A-Za-z0-9+/\n]+={0,2}\n-----END PUBLIC KEY-----\n?$`)
	// architectureForm is "spelled exactly as the arch= label spells it".
	architectureForm = regexp.MustCompile(`^[a-z0-9]+$`)
	// versionForm is the agent's version "written as the release is tagged".
	versionForm = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	// runtimeForm is the runtime "under the name the daemon knows it by".
	runtimeForm = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
)

func (j *Join) field(b *body, name string) error {
	switch name {
	case "token":
		return text(b, &j.Token)
	case "public_key":
		return text(b, &j.PublicKey)
	case "labels":
		return texts(b, &j.Labels, namesMax, fmt.Sprintf("a machine claims at most %d labels, all of them ones its join token permits", namesMax))
	case "capacity":
		if err := joinNotNull(b, "the capacity the machine measured"); err != nil {
			return err
		}
		var c Capacity
		if err := b.fields(&c); err != nil {
			return err
		}
		j.Capacity = &c
		return nil
	case "architecture":
		return text(b, &j.Architecture)
	case "agent_version":
		return text(b, &j.AgentVersion)
	case "namespaces":
		if err := joinNotNull(b, "the namespaces the host narrows itself to, and a host that narrows nothing leaves namespaces out"); err != nil {
			return err
		}
		return texts(b, &j.Namespaces, namesMax, fmt.Sprintf("a machine narrows itself to at most %d namespaces", namesMax))
	case "containment":
		if err := joinNotNull(b, "what the host can prove of its containment, and a host that reports none leaves containment out"); err != nil {
			return err
		}
		var c Containment
		if err := b.fields(&c); err != nil {
			return err
		}
		j.Containment = &c
		return nil
	case "private_key":
		// Named, because it is the one field somebody might send meaning well: "The private
		// half never leaves the machine and has no field here".
		return errors.New("the request body carries a private key, and the private half of a runner's keypair never leaves its host: a join sends public_key alone. Generate a new keypair, since this one has been sent somewhere")
	}
	return unknown(name)
}

func (c *Capacity) field(b *body, name string) error {
	switch name {
	case "vcpu":
		return integer(b, &c.VCPU)
	case "memory":
		return text(b, &c.Memory)
	case "disk":
		return text(b, &c.Disk)
	}
	return unknown(name)
}

func (c *Containment) field(b *body, name string) error {
	switch name {
	case "runtime":
		return text(b, &c.Runtime)
	case "userns_remap":
		if err := joinNotNull(b, "true or false"); err != nil {
			return err
		}
		c.remapWritten = true
		return flag(b, &c.UsernsRemap)
	}
	return unknown(name)
}

// joinNotNull refuses null where a join writes an object, a list or a boolean, and reads nothing
// otherwise. The wire types each and allows no null, and read as left out, null would be a host
// saying nothing where it meant to say something.
func joinNotNull(b *body, want string) error {
	if b.d.PeekKind() != jsontext.KindNull {
		return nil
	}
	if _, err := b.d.ReadToken(); err != nil {
		return malformed(err)
	}
	return fmt.Errorf("the request body holds null at %.100q, where it holds %s", b.d.StackPointer(), want)
}

// joining checks a join the way the wire would, and answers it as the database takes it.
//
// Everything but the token, which is checked where it is spent and refused there with the one
// answer every bad token gets. What is refused here is the machine's description of itself, and
// saying why costs nothing: none of it says whether a token exists.
func (j Join) joining() (db.Joining, error) {
	switch {
	case j.PublicKey == "":
		return db.Joining{}, errors.New("the join sends no public_key: the host generates an Ed25519 keypair, keeps the private half and sends the public one, which is what makes the runner that comes back after a restart provably the one that joined")
	case j.Labels == nil:
		return db.Joining{}, errors.New("the join claims no labels: a machine claiming none writes \"labels\": [], so that claiming nothing is something the request says")
	case j.Capacity == nil:
		return db.Joining{}, errors.New("the join sends no capacity: a machine says what it has, {\"vcpu\", \"memory\", \"disk\"}, since a runner refuses a task that would take it past what it declared")
	case j.Architecture == "":
		return db.Joining{}, errors.New("the join names no architecture: a machine says what it is, spelled as its arch= label spells it")
	case !architectureForm.MatchString(j.Architecture):
		return db.Joining{}, fmt.Errorf("%.64q is not an architecture: it is spelled as the arch= label spells it, amd64 or arm64, lowercase letters and digits", j.Architecture)
	case j.AgentVersion == "":
		return db.Joining{}, errors.New("the join names no agent_version: a machine says which agent it runs, so that one left behind on a host nobody reimaged can be told from the rest")
	case !versionForm.MatchString(j.AgentVersion):
		return db.Joining{}, fmt.Errorf("%.64q is not an agent version: it is written as the release is tagged, 0.2.0", j.AgentVersion)
	}

	key, err := publicKeyOf(j.PublicKey)
	if err != nil {
		return db.Joining{}, err
	}
	if err := distinct(j.Labels, "label", func(label string) error {
		if !labelForm.MatchString(label) {
			return fmt.Errorf("%.64q is not a label: a label is key=value, the key in lowercase, and a machine claims only labels its token permits", label)
		}
		return nil
	}); err != nil {
		return db.Joining{}, err
	}

	c := *j.Capacity
	if c.VCPU < 1 {
		return db.Joining{}, fmt.Errorf("the machine declares %d vCPU, and one that runs anything has one or more", c.VCPU)
	}
	memory, err := sizeOf("memory", c.Memory)
	if err != nil {
		return db.Joining{}, err
	}
	disk, err := sizeOf("disk", c.Disk)
	if err != nil {
		return db.Joining{}, err
	}

	if j.Namespaces != nil {
		if len(j.Namespaces) == 0 {
			return db.Joining{}, errors.New("the join narrows the host to no namespace, and a runner accepting none could never be handed a task: a host that narrows nothing leaves namespaces out")
		}
		if err := distinct(j.Namespaces, "namespace", func(namespace string) error {
			if !givenName.MatchString(namespace) {
				return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens", namespace)
			}
			return nil
		}); err != nil {
			return db.Joining{}, err
		}
	}

	var contained *db.Containment
	if cn := j.Containment; cn != nil {
		switch {
		case cn.Runtime == "":
			return db.Joining{}, errors.New("the containment names no runtime: it is the one the daemon creates this runner's containers with, runc or runsc, under the name the daemon knows it by")
		case !runtimeForm.MatchString(cn.Runtime):
			return db.Joining{}, fmt.Errorf("%.64q is not a container runtime: it is named as the daemon names it, lowercase, runc or runsc", cn.Runtime)
		case !cn.remapWritten:
			return db.Joining{}, errors.New("the containment does not say whether the daemon remaps container root: userns_remap is true or false, and a host that says nothing would have to be read as meeting the floor")
		}
		contained = &db.Containment{Runtime: cn.Runtime, UsernsRemap: cn.UsernsRemap}
	}

	return db.Joining{
		Token: j.Token, Labels: j.Labels, PublicKey: key,
		CPU: c.VCPU, MemoryBytes: memory, DiskBytes: disk,
		Architecture: j.Architecture, AgentVersion: j.AgentVersion,
		Namespaces: j.Namespaces, Containment: contained,
	}, nil
}

// publicKeyOf reads the host's public key: one PEM block of type PUBLIC KEY around a
// SubjectPublicKeyInfo, holding an Ed25519 key.
//
// Ed25519 and nothing else, because the key is what a rotation is signed with, "an Ed25519
// signature over the runner identifier and the request time", and a key of another algorithm
// would be a runner that joined and could never renew.
func publicKeyOf(written string) (ed25519.PublicKey, error) {
	if !publicKeyForm.MatchString(written) {
		return nil, errors.New("the public_key is not a PEM public key: it is one block, -----BEGIN PUBLIC KEY----- and its base64 and -----END PUBLIC KEY-----, with nothing around it, as x509.MarshalPKIXPublicKey and pem.Encode write one")
	}
	block, _ := pem.Decode([]byte(written))
	if block == nil {
		return nil, errors.New("the public_key is not a PEM public key: its base64 does not decode")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the public_key is not a SubjectPublicKeyInfo, which is what the block around it says it holds")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("the public_key is %s, and a runner's key is Ed25519, the algorithm a rotation is signed with", algorithmOf(parsed))
	}
	return key, nil
}

// algorithmOf names the kind of a key that is not the one asked for, for the sentence refusing it.
func algorithmOf(key any) string {
	switch key.(type) {
	case *rsa.PublicKey:
		return "an RSA key"
	case *ecdsa.PublicKey:
		return "an ECDSA key"
	case *ecdh.PublicKey:
		return "an X25519 key"
	}
	return "a key of another algorithm"
}

// sizeOf reads a size the machine measured, "a whole number above zero with a binary suffix", as
// bytes. One past what 63 bits count is refused, since no host has eight exbibytes of anything and
// the inventory counts in bytes.
func sizeOf(what, written string) (int64, error) {
	if written == "" {
		return 0, fmt.Errorf("the capacity writes no %s: it is a whole number above zero with a binary suffix, as a step writes its own, 64Gi or 1Ti", what)
	}
	if !memoryForm.MatchString(written) {
		return 0, fmt.Errorf("the capacity's %s is %.64q: it is a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so that 64Gi cannot be read as 64 bytes", what, written)
	}
	unit := map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}[written[len(written)-2:]]
	n, err := strconv.ParseInt(written[:len(written)-2], 10, 64)
	if err != nil || n > math.MaxInt64/unit {
		return 0, fmt.Errorf("the capacity's %s is %.64q, which is more than a host has", what, written)
	}
	return n * unit, nil
}

func (s *RunnerAPI) join(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var j Join
	if err := readAtMost(r, &j, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	machine, err := j.joining()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var joined db.Joined
	err = s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		joined, err = wide.Join(ctx, machine, s.rotation, s.now())
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoJoinToken):
		// Wrong, spent, expired, claiming a label it may not or a namespace its pool does
		// not accept: one answer for all of them, because a machine that gets a different
		// answer for each is a machine somebody is using to find out which tokens exist.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that join token cannot be redeemed")
		return
	case err != nil:
		// Everything the machine said was checked above, so what is left is the
		// installation's, and a caller nobody has authenticated is told nothing of it.
		fail(w, http.StatusInternalServerError, "the runner could not be created")
		return
	}

	// "A refused registration is an error and never this object with its fields left empty."
	write(w, http.StatusCreated, map[string]any{
		"runner":     joined.Runner,
		"pool":       joined.Pool,
		"credential": joined.Credential,
		"rotate_by":  joined.RotateBy.UTC().Format(time.RFC3339Nano),
	})
}

// Beat is what a runner says every interval, in the shape wire.schema.json gives it:
// $defs/runnerHeartbeat/request.
//
// "liveness, what the runner will accept, and what it is holding": which runner is speaking, the
// agent it runs, what it will do with new work, how many tasks it will run at once, the keys it is
// holding and when it sent this. What was fixed at the join, its labels, namespaces and capacity,
// is not repeated.
type Beat struct {
	Runner       string       `json:"runner"`
	AgentVersion string       `json:"agent_version"`
	State        string       `json:"state"`
	Concurrency  int64        `json:"concurrency"`
	Tasks        []agk.TaskID `json:"tasks"`
	SentAt       time.Time    `json:"sent_at"`
}

// beatMaxTasks is how many keys one heartbeat may name.
//
// A runner names the tasks it is holding, and it holds the containers it is running: one per vCPU
// to start, raised until memory or disk binds. Four thousand containers at once is past what one
// host runs, and the count is what bounds reading a heartbeat, since a key costs a header of
// sixteen bytes however short it is.
const beatMaxTasks = 4096

// beatMaxBytes is how large a heartbeat may be: beatMaxTasks keys of 256 bytes each, which is
// more than a run's identifier, a step's name, an attempt and a shard come to.
const beatMaxBytes = beatMaxTasks * 256

// The grammars the wire holds a heartbeat to, copied from wire.schema.json and held to the
// patterns it writes by a test, as a join's are.
var (
	// runnerForm is the identifier the API answered at the join, lowercase words joined by
	// hyphens.
	runnerForm = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// keyForm is an idempotency key, run/step/attempt and /index/of where a fan-out produced
	// it. agk.TaskID.Validate is applied too, for what a pattern cannot read: a shard past its
	// cardinality, and a key that does not compose back into itself.
	keyForm = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]+/[A-Za-z0-9][A-Za-z0-9_-]*/[1-9][0-9]*(?:/[1-9][0-9]*/[1-9][0-9]*)?$`)
	// instantForm is an RFC 3339 date-time with its offset, which is what the wire's
	// format: date-time is. Checked before time.Parse, which also takes a comma before the
	// fraction and an hour of one digit in its offset, neither of them RFC 3339.
	instantForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})$`)
)

// runnerStates are what a runner reports it will do with new work, in the order the wire lists
// them: ready pulls and accepts tasks, draining accepts none and finishes what it holds, unhealthy
// accepts none because the runner took itself out of service.
var runnerStates = []string{"ready", "draining", "unhealthy"}

func (bt *Beat) field(b *body, name string) error {
	switch name {
	case "runner":
		return text(b, &bt.Runner)
	case "agent_version":
		return text(b, &bt.AgentVersion)
	case "state":
		return text(b, &bt.State)
	case "concurrency":
		return integer(b, &bt.Concurrency)
	case "tasks":
		return texts(b, &bt.Tasks, beatMaxTasks, fmt.Sprintf("a heartbeat names at most %d tasks, which is more containers than one host runs at once", beatMaxTasks))
	case "sent_at":
		return instant(b, &bt.SentAt)
	}
	return unknown(name)
}

// instant reads an RFC 3339 date-time, the way every instant on the wire is written. Null leaves
// it as it was.
func instant(b *body, into *time.Time) error {
	var written string
	if err := text(b, &written); err != nil {
		return err
	}
	if written == "" {
		return nil
	}
	if !instantForm.MatchString(written) {
		return fmt.Errorf("%.64q is not an instant: it is RFC 3339 with its offset, 2026-09-10T06:41:09.104Z", written)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.ToUpper(written))
	if err != nil {
		return fmt.Errorf("%.64q is not an instant: it is written as one, and names a day or a time no calendar has", written)
	}
	*into = at
	return nil
}

// beating checks a heartbeat the way the wire would, and answers it as the database takes it.
func (bt Beat) beating() (db.Beating, error) {
	switch {
	case bt.Runner == "":
		return db.Beating{}, errors.New("the heartbeat names no runner: it names the one the join answered, so that it says on its own what it is about")
	case !runnerForm.MatchString(bt.Runner):
		return db.Beating{}, fmt.Errorf("%.64q is not a runner: a runner is named as the join answered it, lowercase words joined by hyphens", bt.Runner)
	case bt.AgentVersion == "":
		return db.Beating{}, errors.New("the heartbeat names no agent_version: after the join it is the only message that can say which agent a host now runs")
	case !versionForm.MatchString(bt.AgentVersion):
		return db.Beating{}, fmt.Errorf("%.64q is not an agent version: it is written as the release is tagged, 0.2.0", bt.AgentVersion)
	case bt.State == "":
		return db.Beating{}, errors.New("the heartbeat says no state: a runner says whether it is ready, draining or unhealthy, since one that answers and refuses work is present and useless")
	case !slices.Contains(runnerStates, bt.State):
		return db.Beating{}, fmt.Errorf("%.64q is not a runner's state: it is ready, draining or unhealthy", bt.State)
	case bt.Concurrency < 1:
		return db.Beating{}, fmt.Errorf("the heartbeat reports a concurrency of %d: it is the most tasks the host runs at once, AGK_RUNNER_CONCURRENCY, one or more", bt.Concurrency)
	case bt.Tasks == nil:
		return db.Beating{}, errors.New("the heartbeat names no tasks: a host holding none writes \"tasks\": [], so that holding nothing is something the request says")
	case bt.SentAt.IsZero():
		return db.Beating{}, errors.New("the heartbeat says no sent_at: it is when the runner sent it, by its own clock, for the runner to compare with received_at")
	}

	// A set, as the wire has it, and read into a map rather than compared pairwise, since four
	// thousand keys compared each with the ones before it is eight million comparisons for one
	// heartbeat.
	seen := make(map[agk.TaskID]struct{}, len(bt.Tasks))
	for _, key := range bt.Tasks {
		if !keyForm.MatchString(string(key)) {
			return db.Beating{}, fmt.Errorf("%.100q is not an idempotency key: one is run/step/attempt, and run/step/attempt/index/of where a fan-out produced it", string(key))
		}
		if err := key.Validate(); err != nil {
			return db.Beating{}, fmt.Errorf("the heartbeat names a key that could not have been composed: %w", err)
		}
		if _, twice := seen[key]; twice {
			return db.Beating{}, fmt.Errorf("the heartbeat names the key %.100q twice, and one written twice is refused rather than kept once", string(key))
		}
		seen[key] = struct{}{}
	}
	return db.Beating{
		AgentVersion: bt.AgentVersion, State: bt.State, Concurrency: bt.Concurrency, Holding: bt.Tasks,
	}, nil
}

func (s *RunnerAPI) beat(w http.ResponseWriter, r *http.Request, runner Runner) {
	var b Beat
	if err := readAtMost(r, &b, beatMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	report, err := b.beating()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// "a heartbeat whose named runner is not the one that credential belongs to is refused
	// rather than believed." Nothing of it is recorded, since a host sending one is either
	// configured with another host's credential or speaking for a runner it is not, and
	// neither is a report of anything.
	if b.Runner != runner.ID {
		fail(w, http.StatusForbidden, fmt.Sprintf("the heartbeat speaks for runner %s, and its credential is runner %s's: a runner reports for itself alone", b.Runner, runner.ID))
		return
	}

	at := s.now()
	var beaten db.Beaten
	err = s.pool.Installation(r.Context(), db.Heartbeat, func(ctx context.Context, wide *db.Wide) error {
		var err error
		beaten, err = wide.Beat(ctx, runner.ID, report, at)
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

	// "The heartbeat is also how a drain or a revocation takes effect and how the console knows
	// a runner is present: there is no separate liveness channel to keep in sync." The answer is
	// closed and always carries these three, so that a runner reads a list and a boolean rather
	// than testing whether a field arrived. The interval is not among them: the page fixes it,
	// and db.HeartbeatInterval, which the sweep counts silence in, is held to the page's figure
	// by a test.
	answer := map[string]any{
		"received_at": at.UTC().Format(time.RFC3339Nano),
		"drain":       false,
		"cancel":      beaten.Cancel,
	}
	// reason belongs to a drain order and is written with one alone, as the wire requires.
	if beaten.Runner.State == "draining" {
		answer["drain"] = true
		if beaten.Runner.DrainReason != "" {
			answer["reason"] = beaten.Runner.DrainReason
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
