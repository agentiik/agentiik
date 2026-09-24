package api

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/agentiik/agentiik/db"
)

// The administrator's half of the runner surface: the pools, and the tokens that let a machine
// into one.
//
// "An administrator creates a runner pool with its labels, its accepted namespaces and its
// resource ceilings, then issues a join token." Both of those are here, and both are refused to
// everybody but an administrator, for the reason the inventory is: a pool is installation-wide
// and a namespace has no business reading another's.
//
// Both are asked and answered in the shape wire.schema.json gives them, $defs/runnerPool, which
// is what a console, a Terraform provider and a runner are written against: a pool is created by
// sending the document with no token and answered with the same document, a token is answered as
// the document with both halves, "because neither is legible alone", and the listing is the
// documents with no token.

// TokenLife is the longest a join token may be asked to live.
//
// "One hour after it was issued is the default, and it is short because the token only has to
// survive the minutes between an administrator copying it and a machine presenting it." A day is
// already generous for that, and a token good for a week is a credential lying around in a
// terminal history for a week.
const TokenLife = 24 * time.Hour

// TokenDefaultLife is how long a join token lives when nobody asked for longer: "One hour after it
// was issued is the default".
const TokenDefaultLife = time.Hour

// poolNameMax is the longest name a pool may be given. A pool's durable consumer on the bus is
// named after it, and NATS names nothing longer than 255 characters, so a longer name would be a
// pool whose work no runner could ever take.
const poolNameMax = 255

// The grammars the wire holds a pool to, copied from wire.schema.json, and held to the patterns it
// writes by a test, so that the two cannot drift without a test saying so.
var (
	// givenName is a name given rather than minted, a pool's or a namespace's: "lowercase
	// words joined by hyphens".
	givenName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// labelForm is a label: "A key=value a runner claims and a step selects on."
	labelForm = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*=[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$`)
	// cpuForm is "Cores, as a quoted decimal above zero."
	cpuForm = regexp.MustCompile(`^(?:0*[1-9][0-9]*(?:\.[0-9]+)?|0*\.[0-9]*[1-9][0-9]*)$`)
	// memoryForm is "A whole number above zero with a binary suffix".
	memoryForm = regexp.MustCompile(`^[1-9][0-9]*(?:Ki|Mi|Gi|Ti)$`)
)

// RunnerPool is the document the wire calls runnerPool: a pool, and the join token issued from
// it where one is being issued.
type RunnerPool struct {
	Pool      Pool       `json:"pool"`
	JoinToken *JoinToken `json:"join_token,omitempty"`
}

func (d *RunnerPool) field(b *body, name string) error {
	switch name {
	case "pool":
		return b.fields(&d.Pool)
	case "join_token":
		return errors.New("a join token is issued from a pool that exists, at /api/v1/runner-pools/{pool}/join-tokens, and never written with it: it is shown once, to whoever asked for that one")
	}
	return unknown(name)
}

// Pool is a runner pool as an administrator writes it and as it is answered.
//
// Labels, Namespaces and Ceilings are required, as the wire requires them, and an empty one is
// written rather than left out. Each of the three means the widest thing when empty: a pool that
// grants no label, one that accepts every namespace, one with no ceiling. A request that left them
// out would be read as saying that, and a pool accepting every namespace should be one somebody
// said so of.
type Pool struct {
	Name       string    `json:"name"`
	Labels     []string  `json:"labels"`
	Namespaces []string  `json:"namespaces"`
	Ceilings   *Ceilings `json:"resource_ceilings"`

	// Containment is the tier, hardened, sandboxed or separated, and absent is hardened. A
	// pool is always answered with it written, so that nobody reading one has to know the
	// default.
	Containment string `json:"containment,omitempty"`
}

func (p *Pool) field(b *body, name string) error {
	switch name {
	case "name":
		return text(b, &p.Name)
	case "labels":
		return texts(b, &p.Labels, namesMax, fmt.Sprintf("a pool carries at most %d labels", namesMax))
	case "namespaces":
		return texts(b, &p.Namespaces, namesMax, fmt.Sprintf("a pool lists at most %d namespaces it accepts, and one listing none accepts every namespace", namesMax))
	case "resource_ceilings":
		if b.d.PeekKind() == jsontext.KindNull {
			_, err := b.d.ReadToken()
			return malformed(err)
		}
		var c Ceilings
		if err := b.fields(&c); err != nil {
			return err
		}
		p.Ceilings = &c
		return nil
	case "containment":
		if err := notNull(b, "a tier of containment"); err != nil {
			return err
		}
		if err := text(b, &p.Containment); err != nil {
			return err
		}
		return tier(p.Containment)
	}
	return unknown(name)
}

// Ceilings are the most one task may be given on a pool, "whatever its step asked for", written
// as a step writes them. One left out is no ceiling of that kind.
//
// Each is held to its grammar as it is read, because only then can one written empty be told from
// one left out: "" is no number of cores, and read as absent it would be a pool with no ceiling
// that nobody wrote as one.
type Ceilings struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	PIDs   int    `json:"pids,omitempty"`
}

func (c *Ceilings) field(b *body, name string) error {
	switch name {
	case "cpu":
		if err := notNull(b, "a number of cores written as a string"); err != nil {
			return err
		}
		if err := text(b, &c.CPU); err != nil {
			return err
		}
		if !cpuForm.MatchString(c.CPU) {
			return fmt.Errorf("the pool's cpu ceiling is %.64q: it is a decimal number of cores above zero, written as a string, \"0.5\" or \"4\"", c.CPU)
		}
		return nil
	case "memory":
		if err := notNull(b, "a size written as a string"); err != nil {
			return err
		}
		if err := text(b, &c.Memory); err != nil {
			return err
		}
		if !memoryForm.MatchString(c.Memory) {
			return fmt.Errorf("the pool's memory ceiling is %.64q: it is a whole number above zero with a binary suffix, Ki, Mi, Gi or Ti, so that 512Mi cannot be read as 512 bytes", c.Memory)
		}
		return nil
	case "pids":
		if err := notNull(b, "a whole number of processes"); err != nil {
			return err
		}
		if err := integer(b, &c.PIDs); err != nil {
			return err
		}
		if c.PIDs < 1 {
			return fmt.Errorf("the pool's pids ceiling is %d, and it is a whole number of processes, one or more: a pool with no ceiling of processes leaves pids out", c.PIDs)
		}
		return nil
	case "disk":
		return errors.New("a pool has no disk ceiling, because nothing can enforce one: a task writes into a working directory on its host, and no setting of a container bounds what it writes there. The ceilings are cpu, memory and pids")
	}
	return unknown(name)
}

// check refuses a pool the wire would refuse, in what can only be judged once all of it is read.
// Its ceilings and its tier are judged as they are read, where one written empty or null can
// still be told from one left out.
func (p Pool) check() error {
	switch {
	case p.Name == "":
		return errors.New("the request names no pool: a pool is created as {\"pool\": {\"name\": ...}}, and the name is its only identity")
	case len(p.Name) > poolNameMax:
		return fmt.Errorf("a pool's name is at most %d characters and this one is %d: the pool's consumer on the bus is named after it, and the bus names nothing longer", poolNameMax, len(p.Name))
	case !givenName.MatchString(p.Name):
		return fmt.Errorf("%.64q is not a pool name: a pool is named in lowercase words joined by hyphens, the grammar of every name an administrator gives, since it is what a namespace's allowed_runner_pools lists and what a person types", p.Name)
	case p.Labels == nil:
		return errors.New("the pool lists no labels: a pool writes every label its runners may claim, and one that grants none writes \"labels\": []")
	case p.Namespaces == nil:
		return errors.New("the pool lists no namespaces: \"namespaces\": [] accepts every namespace, and is written rather than assumed, so that a pool open to everybody is one somebody said so of")
	case p.Ceilings == nil:
		return errors.New("the pool writes no resource_ceilings: a pool with no ceiling writes \"resource_ceilings\": {}, so that one is never uncapped by omission")
	}
	if err := distinct(p.Labels, "label", func(label string) error {
		if !labelForm.MatchString(label) {
			return fmt.Errorf("%.64q is not a label: a label is key=value, the key in lowercase, because a step selects on it and a half written one could be claimed by nothing", label)
		}
		return nil
	}); err != nil {
		return err
	}
	return distinct(p.Namespaces, "namespace", func(namespace string) error {
		if !givenName.MatchString(namespace) {
			return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens, and a pool accepting one named otherwise would accept nothing", namespace)
		}
		return nil
	})
}

// tier refuses a tier of containment the wire does not name, and one this installation cannot
// give yet. It is held as it is read, like a ceiling, so that "" is refused as no tier rather than
// read as containment left out, which is hardened.
func tier(containment string) error {
	switch containment {
	case db.ContainmentHardened:
		return nil
	case db.ContainmentSandboxed, db.ContainmentSeparated:
		return fmt.Errorf("%s is a tier of containment this installation cannot give yet: it ships in v1.0.0, and until then nothing checks that a pool's hosts give it, so a pool saying so would be trusted for an isolation it does not have. Every pool is hardened until then", containment)
	}
	return fmt.Errorf("%.64q is not a tier of containment: a pool is hardened, sandboxed or separated, and one that names none leaves containment out and is hardened", containment)
}

// notNull refuses null where a pool writes a ceiling or a tier, and reads nothing otherwise.
//
// The wire types each of them and allows no null, and a pool that sets none leaves it out. Read as
// left out, null would give a pool no ceiling, or the default tier, from a value somebody meant
// to set, which is what a client sends for a variable it left unset.
func notNull(b *body, want string) error {
	if b.d.PeekKind() != jsontext.KindNull {
		return nil
	}
	if _, err := b.d.ReadToken(); err != nil {
		return malformed(err)
	}
	return fmt.Errorf("the request body holds null at %.100q, where it holds %s: a pool that sets none leaves it out, and null is refused rather than read as that, so that a pool goes without a ceiling, or takes the default tier, only where somebody left one out", b.d.StackPointer(), want)
}

// distinct refuses a list naming one entry twice, and anything each refuses. The wire holds a
// pool's labels and namespaces to uniqueItems, and one written twice is refused rather than kept
// once, for the reason a field written twice is.
func distinct(list []string, what string, each func(string) error) error {
	for i, entry := range list {
		if slices.Contains(list[:i], entry) {
			return fmt.Errorf("the request names the %s %.64q twice, and one written twice is refused rather than kept once", what, entry)
		}
		if err := each(entry); err != nil {
			return err
		}
	}
	return nil
}

// poolOf is a pool as it is answered: every required part written, the empty ones as empty, and
// the tier named.
func poolOf(p db.RunnerPool) Pool {
	return Pool{
		Name:       p.Name,
		Labels:     orEmpty(p.Labels),
		Namespaces: orEmpty(p.AcceptedNamespaces),
		Ceilings: &Ceilings{
			CPU: p.Ceilings.CPU, Memory: p.Ceilings.Memory, PIDs: p.Ceilings.PIDs,
		},
		Containment: p.Containment,
	}
}

func (s *RunnerAPI) createPool(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask RunnerPool
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	p := ask.Pool
	if err := p.check(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var created db.RunnerPool
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		if err := wide.CreateRunnerPool(ctx, db.RunnerPool{
			Name: p.Name, Labels: p.Labels, AcceptedNamespaces: p.Namespaces,
			Ceilings: db.Ceilings{
				CPU: p.Ceilings.CPU, Memory: p.Ceilings.Memory, PIDs: p.Ceilings.PIDs,
			},
			Containment: p.Containment,
			CreatedBy:   string(who),
		}); err != nil {
			return err
		}
		var err error
		created, err = wide.RunnerPoolNamed(ctx, p.Name)
		return err
	})
	switch {
	case errors.Is(err, db.ErrRunnerPoolExists):
		fail(w, http.StatusConflict, "a runner pool of that name exists already, and a pool's name is its only identity")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the runner pool could not be created")
		return
	}
	write(w, http.StatusCreated, RunnerPool{Pool: poolOf(created)})
}

func (s *RunnerAPI) pools(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	var pools []db.RunnerPool
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		pools, err = wide.RunnerPools(ctx)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the runner pools could not be read")
		return
	}
	// "The token half is absent whenever no token is being issued, which is what a listing
	// of pools is."
	listed := make([]RunnerPool, len(pools))
	for i, p := range pools {
		listed[i] = RunnerPool{Pool: poolOf(p)}
	}
	write(w, http.StatusOK, map[string]any{"runner_pools": listed})
}

// Issue asks for a join token: which labels the machine it is meant for may claim, and how long
// the administrator needs to get it there.
type Issue struct {
	Labels           []string `json:"labels,omitempty"`
	ExpiresInSeconds int      `json:"expires_in_seconds,omitempty"`
}

func (i *Issue) field(b *body, name string) error {
	switch name {
	case "labels":
		return texts(b, &i.Labels, namesMax, fmt.Sprintf("a join token permits at most %d labels, all of them its pool's", namesMax))
	case "expires_in_seconds":
		return integer(b, &i.ExpiresInSeconds)
	}
	return unknown(name)
}

// JoinToken is a join token as it is answered. Token is its secret, which is written once, in the
// answer to the request that issued it, and nowhere else.
type JoinToken struct {
	ID     string   `json:"id"`
	Token  string   `json:"token,omitempty"`
	Pool   string   `json:"pool"`
	Labels []string `json:"labels"`

	// SingleUse is always true, and "written into the message rather than left to prose
	// because the whole exchange rests on it".
	SingleUse bool `json:"single_use"`

	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
}

func (s *RunnerAPI) issue(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	var ask Issue
	if err := readAtMost(r, &ask, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	// Compared in seconds before it is made a duration, because a count of seconds larger than a
	// duration holds wraps when it is multiplied, to a token dead as it is issued or one that
	// lives a fraction of a second, rather than being refused as longer than a day.
	if ask.ExpiresInSeconds > int(TokenLife/time.Second) {
		fail(w, http.StatusBadRequest, "a join token only has to survive the minutes between an administrator copying it and a machine presenting it, and this one asks for longer than a day")
		return
	}
	life := TokenDefaultLife
	if ask.ExpiresInSeconds > 0 {
		life = time.Duration(ask.ExpiresInSeconds) * time.Second
	}
	if err := distinct(ask.Labels, "label", func(string) error { return nil }); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// One moment for both ends of its life, read once, so that the expiry the token is
	// answered with is the hour it was given and not an hour less the time between two reads
	// of a clock.
	now := s.now()
	pool := r.PathValue("pool")
	var issued db.JoinToken
	var from db.RunnerPool
	err := s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		if issued, err = wide.IssueJoinToken(ctx, pool, ask.Labels, string(who), now, now.Add(life)); err != nil {
			return err
		}
		from, err = wide.RunnerPoolNamed(ctx, pool)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoRunnerPool):
		// A pool is installation-wide and the caller is already an administrator, so there
		// is nothing to hide: 404 here means the pool, not the route.
		fail(w, http.StatusNotFound, "no runner pool of that name")
		return
	case errors.Is(err, db.ErrNotThePoolsLabel):
		fail(w, http.StatusBadRequest, "a join token permits only labels its pool carries, and this one asks for a label the pool does not: a label reaches a machine only where an administrator wrote it on a pool first")
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the join token could not be issued")
		return
	}

	// "It is shown once, here", which is the whole reason this answer exists.
	write(w, http.StatusCreated, RunnerPool{
		Pool: poolOf(from),
		JoinToken: &JoinToken{
			ID:        issued.ID,
			Token:     issued.Clear,
			Pool:      issued.Pool,
			Labels:    orEmpty(issued.Labels),
			SingleUse: true,
			IssuedAt:  issued.IssuedAt.UTC().Format(time.RFC3339Nano),
			ExpiresAt: issued.ExpiresAt.UTC().Format(time.RFC3339Nano),
		},
	})
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
