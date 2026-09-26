package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/internal/token"
)

// Joining is what join is given: its command line, the environment, and where the host keeps
// what it writes.
type Joining struct {
	// API is --api, and where it is empty, AGK_API from the environment.
	API string

	// Token is --token, the join token an administrator issued. It is never repeated.
	Token Secret

	// Labels is --labels, the labels this runner claims written as AGK_RUNNER_LABELS writes
	// them, and where it is empty, AGK_RUNNER_LABELS from the environment. Neither claims no
	// label, which is what a runner of the pool default claims.
	Labels string

	// Lookup is the environment, which also gives AGK_RUNNER_NAMESPACES, the host's narrowing,
	// and AGK_RUNNER_WORKDIR, whose filesystem is the disk measured.
	Lookup Lookup

	// Replace lets join replace an identity the host already has, with a new key.
	Replace bool

	// Owner is the account the key and runner.env are given to, and nil is whoever runs join.
	Owner *Owner

	// EnvPath, KeyPath and MemInfo are runner.env, the key and /proc/meminfo.
	EnvPath, KeyPath, MemInfo string

	// CredentialPath is where serve keeps the credential it renewed to, which join takes away.
	// Empty is none.
	CredentialPath string

	// Socket is the daemon's, where empty is the one docker finds itself.
	Socket string

	// HTTP is what reaches the API, and nil is a client that follows no redirect.
	HTTP *http.Client
}

// Joined is what a join came to.
type Joined struct {
	Runner, Pool string

	// Labels are the labels it claimed, and nil is none.
	Labels []string

	// RotateBy is when the credential stops being accepted.
	RotateBy time.Time
}

// joinRequest is $defs/runnerRegistration/request of the wire.
type joinRequest struct {
	Token        string       `json:"token"`
	PublicKey    string       `json:"public_key"`
	Labels       []string     `json:"labels"`
	Capacity     Capacity     `json:"capacity"`
	Architecture string       `json:"architecture"`
	AgentVersion string       `json:"agent_version"`
	Namespaces   []string     `json:"namespaces,omitempty"`
	Containment  *Containment `json:"containment,omitempty"`
}

// joinAnswer is $defs/runnerRegistration/response.
type joinAnswer struct {
	Runner     string    `json:"runner"`
	Pool       string    `json:"pool"`
	Credential Secret    `json:"credential"`
	RotateBy   time.Time `json:"rotate_by"`
}

// Join trades a join token for this runner's identity, and writes it where serve reads it.
//
// Everything that can refuse the join refuses it before the token is spent: the settings, an
// identity the host already has, the host's capacity and its daemon, and the two directories
// written to, where both files are created beside where they go and given to the agent's
// account, and the key written, before the API is asked anything. What is left once it answers is
// to write runner.env and move both into place, so that a token the API refused leaves nothing
// behind, key included, and a token it took is rarely spent on a host that then cannot keep what
// it was given.
func Join(ctx context.Context, j Joining) (Joined, error) {
	settings, err := j.settings()
	if err != nil {
		return Joined{}, err
	}
	if err := j.unjoined(settings.identity); err != nil {
		return Joined{}, err
	}

	capacity, err := measure(j.MemInfo, settings.WorkDir)
	if err != nil {
		return Joined{}, err
	}
	contained, err := readContainment(ctx, j.Socket)
	if err != nil {
		return Joined{}, err
	}
	key, err := newHostKey()
	if err != nil {
		return Joined{}, err
	}

	// The key's directory is under /var/lib/agentiik, the tree the agent writes in, and is
	// the agent's; runner.env's is /etc/agentiik, the host's configuration, which is left to
	// whoever runs join.
	if err := makeDir(filepath.Dir(j.KeyPath), 0o700, j.Owner); err != nil {
		return Joined{}, err
	}
	if err := makeDir(filepath.Dir(j.EnvPath), 0o755, nil); err != nil {
		return Joined{}, err
	}
	keyFile, err := stage(j.KeyPath, j.Owner)
	if err != nil {
		return Joined{}, err
	}
	defer keyFile.abandon()
	envFile, err := stage(j.EnvPath, j.Owner)
	if err != nil {
		return Joined{}, err
	}
	defer envFile.abandon()
	if err := keyFile.write(key.private); err != nil {
		return Joined{}, err
	}

	// Written [] where it claims none, since the wire takes claiming nothing as something the
	// request says rather than a field it left out.
	claimed := settings.Labels
	if claimed == nil {
		claimed = []string{}
	}
	var answer joinAnswer
	err = newClient(settings.API, "", j.HTTP).Do(ctx, http.MethodPost, "/api/v1/runners", joinRequest{
		Token:        string(j.Token),
		PublicKey:    key.public,
		Labels:       claimed,
		Capacity:     capacity,
		Architecture: Architecture(),
		AgentVersion: Version(),
		Namespaces:   settings.Namespaces,
		Containment:  &contained,
	}, &answer)
	var refused *APIError
	switch {
	case errors.Is(err, ErrCredentialRefused):
		return Joined{}, errors.New("runner: the API refused the join token. It is wrong, already spent or expired, or it does not permit a label this host claims or its pool a namespace the host narrows itself to, and the API does not say which, so that nobody learns from its answers which tokens exist. Nothing was written: ask an administrator for a new token")
	case errors.As(err, &refused) && refused.Status == http.StatusBadRequest:
		return Joined{}, fmt.Errorf("runner: the API refused what this host said of itself, and spent no token: %s. Nothing was written", refused.Message)
	case err != nil:
		return Joined{}, fmt.Errorf("%w. Nothing was written. If joining again with the same token is refused, this attempt spent it and created a runner, which an administrator revokes", err)
	}

	spent := func(err error) error {
		return fmt.Errorf("runner: the API created runner %.64q in pool %.64q and spent the token, and this host could not keep what it was given, so it has not joined: %w. Revoke that runner, and join again with a new token and --replace", answer.Runner, answer.Pool, err)
	}
	// AGK_API is written as it was given rather than as the client reaches it, without its
	// trailing slashes: serve compares the file with its environment as written, and a unit
	// setting the same address join was given would otherwise be refused as another.
	text, err := renderEnv(j.EnvPath, []variable{
		{API, settings.written},
		{RunnerID, answer.Runner},
		{RunnerPool, answer.Pool},
		{Labels, strings.Join(settings.Labels, ",")},
		{Namespaces, strings.Join(settings.Namespaces, ",")},
		{Concurrency, settings.kept[Concurrency]},
		{WorkDir, settings.kept[WorkDir]},
		{Credential, string(answer.Credential)},
	})
	if err != nil {
		return Joined{}, spent(err)
	}
	if err := envFile.write(text); err != nil {
		return Joined{}, spent(err)
	}
	// A credential serve renewed to belongs to the identity this join replaces, or to one long
	// gone, and serve prefers it to runner.env, so it goes whatever runner it names. Before the
	// key, whose directory it shares, so that the new key is never in place beside it.
	if j.CredentialPath != "" {
		if err := os.Remove(j.CredentialPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Joined{}, spent(fmt.Errorf("runner: %s, which holds the credential an earlier runner of this host renewed to, cannot be taken away: %s", j.CredentialPath, reasonOf(err)))
		}
	}
	// The key first, so that a runner.env is never in place without the key it was joined
	// with. A replaced runner.env goes before either, since the two renames are not one: a
	// host that stops between them has no identity and joins again, where one holding the
	// new key beside the old runner.env would start as a runner whose key it no longer has.
	if j.Replace {
		if err := os.Remove(j.EnvPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Joined{}, spent(fmt.Errorf("runner: %s cannot be replaced: %s", j.EnvPath, reasonOf(err)))
		}
		if err := syncDir(filepath.Dir(j.EnvPath)); err != nil {
			return Joined{}, spent(err)
		}
	}
	if err := keyFile.commit(j.Replace); err != nil {
		return Joined{}, spent(err)
	}
	if err := envFile.commit(j.Replace || settings.settingsOnly); err != nil {
		return Joined{}, spent(err)
	}
	return Joined{Runner: answer.Runner, Pool: answer.Pool, Labels: settings.Labels, RotateBy: answer.RotateBy}, nil
}

// joinSettings are the settings join sends and writes.
type joinSettings struct {
	Config

	// written is AGK_API as it was given, which is what runner.env carries.
	written string

	// kept are the settings an operator wrote in a runner.env already there, which join
	// writes again as they were rather than dropping them.
	kept map[string]string

	// identity says that runner.env already holds a runner's identity, and settingsOnly that
	// it is there and holds none, which join replaces as it would with --replace.
	identity, settingsOnly bool
}

// joinWrites are the settings join writes to runner.env from what it was given, and what a
// runner.env already there says of them is read only where join was given nothing: the file is
// replaced whole, so its old value and the one given are not two values serve would compare.
var joinWrites = []string{API, Labels, Namespaces}

// joinKeeps are the settings join has no say in, which it keeps from a runner.env already there.
// Written there and in the environment, the two are held to agreeing, as serve holds them.
var joinKeeps = []string{Concurrency, WorkDir}

// settings reads what join sends and writes, held to the grammars serve reads them in.
func (j Joining) settings() (joinSettings, error) {
	lookup := j.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &reader{path: j.EnvPath, file: map[string]string{}, written: map[string]bool{}}
	var c joinSettings
	there, identity := j.existing(r)
	c.identity, c.settingsOnly, c.kept = identity, there != nil && !identity, map[string]string{}
	for _, name := range joinKeeps {
		if v, ok := there[name]; ok {
			r.file[name], c.kept[name] = v, v
		}
	}
	// The command line stands in front of the environment, as a flag given means that value,
	// and the environment in front of the file join is about to replace.
	r.lookup = func(name string) (string, bool) {
		switch {
		case name == API && j.API != "":
			return j.API, true
		case name == Labels && j.Labels != "":
			return j.Labels, true
		}
		if v, ok := lookup(name); ok && v != "" {
			return v, true
		}
		if slices.Contains(joinWrites, name) {
			v, ok := there[name]
			return v, ok
		}
		return "", false
	}

	// A flag that disagrees with the environment join runs in is refused, since serve reads
	// both and refuses the two written differently: joining would spend the token on a host
	// that could then never start. Neither value is repeated, since either may carry a secret.
	for _, given := range []struct{ flag, name, value string }{{"--api", API, j.API}, {"--labels", Labels, j.Labels}} {
		if v, set := lookup(given.name); set && v != "" && given.value != "" && v != given.value {
			r.refuse(given.flag, "is not what "+given.name+" is set to in this environment, and serve, reading both it and "+j.EnvPath+", would refuse the two: give the same value, or unset "+given.name)
		}
	}

	if written, set := r.env(API); set {
		c.API, c.written = r.api(), written
	} else {
		r.refuse("--api", "is not given and "+API+" is not set, and it is the address of the API this host joins, such as https://agentiik.example.com")
	}
	// Neither --labels nor AGK_RUNNER_LABELS claims no label, which is a runner of the pool
	// default: it takes the steps that name no runs_on, and a token of that pool permits none.
	c.Labels = r.labels()
	// The work root is where the disk is measured, so it is read as serve will read it, from
	// the environment or the file join keeps it in.
	c.Namespaces, c.WorkDir, _ = r.namespaces(), r.workDir(), r.concurrency()
	switch kind, ok := token.KindOf(string(j.Token)); {
	case j.Token == "":
		r.refuse("--token", "is not given, and it is the join token an administrator issued for this host's pool")
	case !ok || kind != token.Join || strings.ContainsFunc(string(j.Token), func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-')
	}):
		r.refuse("--token", "is not a join token, which is written agkjoin_ followed by its secret")
	}

	// The address is written to runner.env, so it is held to what every reader of that file
	// reads the same, as the file's own lines are. It is not repeated: it may carry a secret.
	if c.written != "" && c.API != "" {
		if fault := valueFault(c.written); fault != "" {
			r.refuse(API, fault+", and it is written to "+j.EnvPath)
		}
	}
	return c, r.err()
}

// existing reads the runner.env already on the host, where there is one: its settings, and
// whether it holds a runner's identity. "Also where the settings above may be written", so a file
// an operator wrote settings to before joining is one join keeps them from rather than a host
// that has joined. What is wrong with it is refused through r, as serve would refuse it.
func (j Joining) existing(r *reader) (map[string]string, bool) {
	checked, err := os.Lstat(j.EnvPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false
	case err != nil:
		r.refuse(j.EnvPath, "cannot be read, so whether this host has already joined cannot be told: "+reasonOf(err))
		return nil, false
	case !checked.Mode().IsRegular():
		r.refuse(j.EnvPath, "is not a file, and it is where join writes this runner's identity: remove it")
		return nil, false
	}
	f, err := os.OpenFile(j.EnvPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		r.refuse(j.EnvPath, "cannot be read, so whether this host has already joined cannot be told: "+reasonOf(err))
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(checked, info) {
		r.refuse(j.EnvPath, "was replaced while it was being read")
		return nil, false
	}
	text, err := io.ReadAll(io.LimitReader(f, envFileMaxBytes+1))
	if err != nil || len(text) > envFileMaxBytes {
		r.refuse(j.EnvPath, fmt.Sprintf("cannot be read whole within %d bytes", envFileMaxBytes))
		return nil, false
	}
	p := &reader{path: j.EnvPath, file: map[string]string{}, written: map[string]bool{}}
	p.parse(text)
	r.refused = append(r.refused, p.refused...)
	return p.file, p.written[RunnerID] || p.written[RunnerPool] || p.written[Credential]
}

// unjoined refuses a host that already has an identity, unless it is to be replaced.
//
// A key, or a runner.env holding an identity, is a runner already registered, and joining again
// would orphan it: its record stays in the API with a credential nobody holds any longer. So that
// is something an operator asks for, and is told the consequence of.
func (j Joining) unjoined(identity bool) error {
	const again = " agk-runner join --replace makes it a new runner with a new key, and the runner it was stays registered until an administrator revokes it"
	if j.Replace {
		return nil
	}
	_, err := os.Lstat(j.KeyPath)
	if identity {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runner: %s holds a runner's identity and %s, the key it joined with, is gone, so this host cannot be that runner again: a host whose key is gone is a new runner.%s", j.EnvPath, j.KeyPath, again)
		}
		return fmt.Errorf("runner: this host has already joined, since %s holds a runner's identity.%s", j.EnvPath, again)
	}
	switch {
	case err == nil:
		return fmt.Errorf("runner: this host has already joined, since %s is there.%s", j.KeyPath, again)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("runner: %s cannot be read, so whether this host has already joined cannot be told: %s", j.KeyPath, reasonOf(err))
	}
	return nil
}
