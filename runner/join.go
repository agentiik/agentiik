package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	// them, and where it is empty, AGK_RUNNER_LABELS from the environment.
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

	// Socket is the daemon's, where empty is the one docker finds itself.
	Socket string

	// HTTP is what reaches the API, and nil is a client that follows no redirect.
	HTTP *http.Client
}

// Joined is what a join came to.
type Joined struct {
	Runner, Pool string

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
// written to, where both files are created, empty and given to the agent's account, before the
// API is asked anything. What is left once it answers is to write them and move them into place,
// so that a token the API refused leaves nothing behind, key included, and a token it took is
// rarely spent on a host that then cannot keep what it was given.
func Join(ctx context.Context, j Joining) (Joined, error) {
	settings, err := j.settings()
	if err != nil {
		return Joined{}, err
	}
	if err := j.unjoined(); err != nil {
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

	var answer joinAnswer
	err = newClient(settings.API, "", j.HTTP).Do(ctx, http.MethodPost, "/api/v1/runners", joinRequest{
		Token:        string(j.Token),
		PublicKey:    key.public,
		Labels:       settings.Labels,
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
		return fmt.Errorf("runner: the API created runner %.64q in pool %.64q and spent the token, and this host could not keep what it was given, so it has not joined: %w. Revoke that runner and join again with a new token", answer.Runner, answer.Pool, err)
	}
	text, err := renderEnv(j.EnvPath, []variable{
		{API, settings.API},
		{RunnerID, answer.Runner},
		{RunnerPool, answer.Pool},
		{Labels, strings.Join(settings.Labels, ",")},
		{Namespaces, strings.Join(settings.Namespaces, ",")},
		{Credential, string(answer.Credential)},
	})
	if err != nil {
		return Joined{}, spent(err)
	}
	if err := envFile.write(text); err != nil {
		return Joined{}, spent(err)
	}
	// The key first, so that a runner.env is never in place without the key it was joined
	// with.
	if err := keyFile.commit(j.Replace); err != nil {
		return Joined{}, spent(err)
	}
	if err := envFile.commit(j.Replace); err != nil {
		return Joined{}, spent(err)
	}
	return Joined{Runner: answer.Runner, Pool: answer.Pool, RotateBy: answer.RotateBy}, nil
}

// settings reads what join sends and writes, held to the grammars serve reads them in.
func (j Joining) settings() (Config, error) {
	lookup := j.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &reader{path: j.EnvPath, file: map[string]string{}, written: map[string]bool{}}
	// The command line stands in front of the environment, as a flag given means that value.
	r.lookup = func(name string) (string, bool) {
		switch {
		case name == API && j.API != "":
			return j.API, true
		case name == Labels && j.Labels != "":
			return j.Labels, true
		}
		return lookup(name)
	}

	var c Config
	if _, set := r.env(API); set {
		c.API = r.api()
	} else {
		r.refuse("--api", "is not given and "+API+" is not set, and it is the address of the API this host joins, such as https://agentiik.example.com")
	}
	if _, set := r.env(Labels); set {
		c.Labels = r.labels()
	} else {
		r.refuse("--labels", "is not given and "+Labels+" is not set, and they are the labels this runner claims, within what the token permits, such as zone=dmz,arch=amd64: serve refuses to start without them")
	}
	c.Namespaces, c.WorkDir = r.namespaces(), r.workDir()
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
	if c.API != "" {
		if fault := valueFault(c.API); fault != "" {
			r.refuse(API, fault+", and it is written to "+j.EnvPath)
		}
	}
	return c, r.err()
}

// unjoined refuses a host that already has an identity, unless it is to be replaced.
//
// A key or a runner.env already there is a runner already registered, and joining again would
// orphan it: its record stays in the API with a credential nobody holds any longer. So that is
// something an operator asks for, and is told the consequence of.
func (j Joining) unjoined() error {
	if j.Replace {
		return nil
	}
	for _, path := range []string{j.KeyPath, j.EnvPath} {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			return fmt.Errorf("runner: this host has already joined, since %s is there. agk-runner join --replace makes it a new runner with a new key, and the runner it was stays registered until an administrator revokes it", path)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("runner: %s cannot be read, so whether this host has already joined cannot be told: %s", path, reasonOf(err))
		}
	}
	return nil
}
