package api

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
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
	"unicode"
	"unicode/utf8"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
)

// The routes a runner reaches, and no others.
//
// "Serve the runner-facing routes and no others to a runner." What a runner may do is a short
// closed list: join, say it is there, redeem a grant, ship a log. It holds no permission and
// reaches nothing else, which is what makes a compromised one cost "the tasks in its hands and
// the namespaces its policy accepts". Renewing its credential is one more of them, and proves the
// key the runner joined with.

// RunnerOptions are what the runner half of the API is given.
type RunnerOptions struct {
	Pool *db.Pool

	// JoinRotation is how long a runner credential is accepted for. "Rotation is automatic
	// and the agent renews well ahead of it; a runner that was offline past this instant has
	// to join again, which is deliberate, because a machine that has been dark for a month
	// should be reconsidered rather than readmitted." The page fixes no interval; thirty
	// days is what this installs with and what an installation overrides.
	JoinRotation time.Duration

	// RevocationGrace is how long a revoked runner's results are still taken: "an installation
	// setting defaulting to the task ceiling (one hour), since no task legitimately runs longer".
	// An hour where it is unset, the ceiling an installation that says nothing runs with.
	RevocationGrace time.Duration

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
	grace     time.Duration
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
	if o.RevocationGrace <= 0 {
		o.RevocationGrace = time.Hour
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
		pool: o.Pool, rotation: o.JoinRotation, grace: o.RevocationGrace,
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
		{"POST", "/api/v1/runners/rotate", s.rotate},
		{"POST", "/api/v1/tasks/redeem", s.redeem},
		{"POST", "/api/v1/bus/token", s.busToken},
	} {
		if err := rt.HandleRunner(r.method, r.pattern, ForRunner{}, r.handler); err != nil {
			return nil, err
		}
	}

	// And the administrator's half: the inventory, which is the installation's rather than a
	// runner's ("A user never learns which host executed a task beyond its runner name and
	// labels"), the pools, the tokens that let a machine into one, and the two orders that take
	// one out of service.
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
		{"POST", "/api/v1/runners/{runner}/drain", s.drain},
		{"POST", "/api/v1/runners/{runner}/revoke", s.revoke},
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
		found, err = w.Authenticate(ctx, credential, s.now())
		return err
	})
	if errors.Is(err, db.ErrNoRunner) {
		return Runner{}, ErrNoRunner
	}
	if err != nil {
		return Runner{}, err
	}
	return Runner{
		ID: found.ID, Pool: found.Pool, State: found.State, RotateBy: found.RotateBy,
		ResultsAcceptedUntil: found.ResultsAcceptedUntil,
	}, nil
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
	at, err := instantOf(written)
	if err != nil {
		return err
	}
	*into = at
	return nil
}

// instantOf reads one instant as the wire writes it.
func instantOf(written string) (time.Time, error) {
	if !instantForm.MatchString(written) {
		return time.Time{}, fmt.Errorf("%.64q is not an instant: it is RFC 3339 with its offset, 2026-09-10T06:41:09.104Z", written)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.ToUpper(written))
	if err != nil {
		return time.Time{}, fmt.Errorf("%.64q is not an instant: it is written as one, and names a day or a time no calendar has", written)
	}
	return at, nil
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
	// A drain and a revocation are one order to a runner, "take nothing new; finish what is
	// held", and a revocation adds how long it has to finish in. reason and
	// results_accepted_until belong to a drain order and are written with one alone, as the
	// wire requires.
	switch r := beaten.Runner; r.State {
	case "draining", "revoked":
		answer["drain"] = true
		if r.DrainReason != "" {
			answer["reason"] = r.DrainReason
		}
		if r.State == "revoked" {
			// The instant the credential this heartbeat carried is refused from, which is
			// its rotate_by where that comes first: a revoked runner rotates nothing, so
			// its grace ends there too, and "the grace period is stated on the wire rather
			// than left to each side's arithmetic".
			until := r.ResultsAcceptedUntil
			if !runner.RotateBy.IsZero() && runner.RotateBy.Before(until) {
				until = runner.RotateBy
			}
			answer["results_accepted_until"] = until.UTC().Format(time.RFC3339Nano)
		}
	}
	write(w, http.StatusOK, answer)
}

// Rotation is what a runner sends to be given a new credential, in the shape wire.schema.json gives
// it: $defs/runnerRotation/request. The credential it rotates is the one the request carries, as
// every runner route's does.
//
// "The key proves the machine: a stolen credential without it cannot be renewed." So the request
// is signed, with the private half of the key the host generated at join, over which runner is
// asking and when.
type Rotation struct {
	Runner string `json:"runner"`

	// At is when the runner signed the request, by its own clock, kept as written because the
	// characters written are what was signed.
	At string `json:"at"`

	// Signature is the Ed25519 signature of RotationSigned(Runner, At), in standard base64.
	Signature string `json:"signature"`
}

func (ro *Rotation) field(b *body, name string) error {
	switch name {
	case "runner":
		return text(b, &ro.Runner)
	case "at":
		return text(b, &ro.At)
	case "signature":
		return text(b, &ro.Signature)
	}
	return unknown(name)
}

// RotationSigned is the message a rotation's signature is over: a line saying what it is for,
// then the runner and the request time, each on a line of its own and exactly as the request
// writes them.
//
// The first line is there so that a signature made for this can never be read as one made for
// anything else the key might one day sign, and the other two can hold no line break, since the
// wire's grammars for a runner and an instant admit none, so no two requests share a message.
// The time is signed as written rather than as a parsed instant, because two spellings of one
// instant are two messages, and a runner and the API that had to agree on a canonical one would
// be one more thing to keep in step.
func RotationSigned(runner, at string) []byte {
	return []byte("agentiik runner rotation\n" + runner + "\n" + at)
}

// RotationSkew is how far a rotation's request time may be from the API's clock, either way.
//
// The signature proves the key, and the time is what stops a signed request from being a proof
// forever: a copy of one, from a proxy's log or a disk, is worth nothing five minutes later, the
// window webhooks are held to. Inside it, a request is good for one rotation, since each has to
// sign a later time than the last. A runner whose clock is further out than this cannot rotate
// until it is corrected, and the heartbeat's received_at is how it finds out.
const RotationSkew = 5 * time.Minute

// signatureForm is an Ed25519 signature, sixty-four bytes, in standard base64 with its padding.
//
// The last character before the padding holds two bits of the signature and four of nothing, and
// is one of the four characters whose four are zero: the decoder refuses the rest, so the grammar
// does too, rather than taking in a spelling it then calls no signature.
var signatureForm = regexp.MustCompile(`^[A-Za-z0-9+/]{85}[AQgw]==$`)

// rotating checks a rotation the way the wire would, and answers the request time and the
// signature it carries.
func (ro Rotation) rotating() (time.Time, []byte, error) {
	switch {
	case ro.Runner == "":
		return time.Time{}, nil, errors.New("the rotation names no runner: it names the one the join answered, which is half of what the signature is over")
	case !runnerForm.MatchString(ro.Runner):
		return time.Time{}, nil, fmt.Errorf("%.64q is not a runner: a runner is named as the join answered it, lowercase words joined by hyphens", ro.Runner)
	case ro.At == "":
		return time.Time{}, nil, errors.New("the rotation says no at: it is when the runner signed it, by its own clock, and the other half of what the signature is over")
	case ro.Signature == "":
		return time.Time{}, nil, errors.New("the rotation carries no signature: a credential is renewed by the host that holds the key it joined with, and the signature is how it shows that")
	case !signatureForm.MatchString(ro.Signature):
		return time.Time{}, nil, errors.New("the signature is not an Ed25519 signature: it is sixty-four bytes in standard base64, eighty-eight characters ending in ==")
	}
	at, err := instantOf(ro.At)
	if err != nil {
		return time.Time{}, nil, err
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(ro.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return time.Time{}, nil, errors.New("the signature is not an Ed25519 signature: it is sixty-four bytes in standard base64, eighty-eight characters ending in ==")
	}
	return at, signature, nil
}

func (s *RunnerAPI) rotate(w http.ResponseWriter, r *http.Request, runner Runner) {
	var ro Rotation
	if err := readAtMost(r, &ro, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	at, signature, err := ro.rotating()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// The heartbeat's rule, for the same reason: a host rotating for another runner is either
	// configured with another host's credential or speaking for a runner it is not.
	if ro.Runner != runner.ID {
		fail(w, http.StatusForbidden, fmt.Sprintf("the rotation speaks for runner %s, and its credential is runner %s's: a runner renews its own credential alone", ro.Runner, runner.ID))
		return
	}
	// A revoked runner is still opened in its grace, and is told why it renews nothing rather
	// than that its credential opens nothing, which it still does until then. A draining one
	// rotates, for the reason db.Wide.Rotate gives.
	if runner.State == "revoked" {
		refuseRevokedRotation(w, runner.ID)
		return
	}
	now := s.now()
	if skew := now.Sub(at); skew > RotationSkew || skew < -RotationSkew {
		fail(w, http.StatusBadRequest, fmt.Sprintf("the rotation was signed at %s, and the API's clock reads %s: a rotation is signed within %s of it, so that a copy of one is not a way to rotate later. Correct the host's clock, which the heartbeat's received_at is there to compare with, and sign again",
			at.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), RotationSkew))
		return
	}

	// The credential is read again from the request rather than carried by the hook, since the
	// runner is one thing and which of its credentials was presented is another: it is the one
	// that stays accepted until the new one is first used.
	credential, _ := bearerOf(r)
	var rotated db.Rotated
	err = s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		rotated, err = wide.Rotate(ctx, db.Rotating{
			Credential: credential, Runner: ro.Runner, SignedAt: at,
			Signed: RotationSigned(ro.Runner, ro.At), Signature: signature,
		}, s.rotation, now)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRunner):
		// Superseded or expired between the hook and the lock, which is what the hook would
		// have answered a moment later.
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "that credential opens nothing")
		return
	case errors.Is(err, db.ErrRunnerRevoked):
		// Revoked between the hook and the lock, and still in its grace.
		refuseRevokedRotation(w, runner.ID)
		return
	case errors.Is(err, db.ErrNotItsKey):
		fail(w, http.StatusForbidden, fmt.Sprintf("the signature is not by the key runner %s joined with: the key proves the machine, and a host whose key is gone is a new runner, which joins again", runner.ID))
		return
	case errors.Is(err, db.ErrRotationReplayed):
		fail(w, http.StatusConflict, fmt.Sprintf("runner %s last rotated with a request signed at that time or later: each rotation signs a later time than the last, so that a copy of one rotates nothing. Sign again", runner.ID))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the credential could not be rotated")
		return
	}

	// It exists in this answer and on the machine that asked, and nowhere else.
	w.Header().Set("Cache-Control", "no-store")
	write(w, http.StatusOK, map[string]any{
		"credential": rotated.Credential,
		"rotate_by":  rotated.RotateBy.UTC().Format(time.RFC3339Nano),
	})
}

// Order is what an administrator sends to drain or revoke a runner: why, which the runner is
// handed at its next heartbeat "for the runner to write to its own log", since "the owner of the
// machine is rarely the person who revoked the credential in the console".
type Order struct {
	Reason string `json:"reason"`
}

func (o *Order) field(b *body, name string) error {
	if name == "reason" {
		return text(b, &o.Reason)
	}
	return unknown(name)
}

// reasonMax is how long a reason may be, in characters: one line of a journal, which is where the
// runner writes it, and short enough for the console to show beside the runner.
const reasonMax = 256

// reason checks an order's reason the way the wire holds the heartbeat's: "in one line", and there.
func (o Order) reason() (string, error) {
	switch {
	case o.Reason == "":
		return "", errors.New("the order gives no reason: it is one line saying why, which the runner writes to its own log, since whoever owns the machine is rarely whoever gave the order")
	case utf8.RuneCountInString(o.Reason) > reasonMax:
		return "", fmt.Errorf("the reason is %d characters, and it is one line of at most %d, for a journal and the console", utf8.RuneCountInString(o.Reason), reasonMax)
	case strings.ContainsFunc(o.Reason, unicode.IsControl):
		return "", errors.New("the reason holds a line break or another control character, and it is one line, written as it is into the runner's log")
	}
	return o.Reason, nil
}

// orderOf reads an order and the runner its path names, and answers false once it has refused it.
func orderOf(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var o Order
	if err := readAtMost(r, &o, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return "", "", false
	}
	why, err := o.reason()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	// A name no runner can have is no runner, and is answered as one that is not there rather
	// than looked up.
	runner := r.PathValue("runner")
	if !runnerForm.MatchString(runner) {
		fail(w, http.StatusNotFound, "no runner of that identifier")
		return "", "", false
	}
	return runner, why, true
}

// drain orders a runner to take nothing new and finish what it holds, and answers it as the
// inventory lists it.
//
// "Drain sets the state to draining and answers drain: true at the heartbeat. Results are accepted
// as usual; the runner takes nothing new but stays up." Who ordered it is recorded on the runner
// until the audit log records it, and a runner already draining is answered as it stands.
func (s *RunnerAPI) drain(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	runner, why, ok := orderOf(w, r)
	if !ok {
		return
	}
	var drained db.Runner
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		drained, err = wide.Drain(ctx, runner, string(who), why, s.now())
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRunner):
		// The inventory is the administrator's already, so there is nothing to hide: 404 here
		// means the runner, not the route.
		fail(w, http.StatusNotFound, "no runner of that identifier")
		return
	case errors.Is(err, db.ErrRunnerRevoked):
		fail(w, http.StatusConflict, fmt.Sprintf("runner %s is revoked, which already takes nothing new and ends its credential with its grace: a drain would only undo part of that, and a revocation is not undone", runner))
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the runner could not be drained")
		return
	}
	write(w, http.StatusOK, drained)
}

// revoke orders a runner out: it takes nothing new at once, and is refused everywhere once its
// grace has passed. It answers the runner as the inventory lists it.
//
// "Revoke sets the state to revoked with a grace that ends at the revocation plus
// revocation_grace." Until then it is told to drain at its heartbeat and its results are taken,
// because "revoking a credential never destroys work already done". A runner already revoked is
// answered as it stands, so that revoking it again gives it no more time.
func (s *RunnerAPI) revoke(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	runner, why, ok := orderOf(w, r)
	if !ok {
		return
	}
	var revoked db.Runner
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		revoked, err = wide.Revoke(ctx, runner, string(who), why, s.now(), s.grace)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRunner):
		fail(w, http.StatusNotFound, "no runner of that identifier")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the runner could not be revoked")
		return
	}
	write(w, http.StatusOK, revoked)
}

// refuseRevokedRotation answers a rotation by a runner that is revoked and still in its grace.
func refuseRevokedRotation(w http.ResponseWriter, runner string) {
	fail(w, http.StatusForbidden, fmt.Sprintf("runner %s is revoked, and a revoked credential is not renewed: it is accepted until the end of its grace, which the heartbeat answers as results_accepted_until, and refused everywhere after", runner))
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
