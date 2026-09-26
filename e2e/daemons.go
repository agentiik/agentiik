package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/driver"
)

// The paths a runner's agent and its daemon share, each the same on both sides, since the daemon
// resolves every bind source it is asked for in its own filesystem: the work root under
// /var/lib/agentiik, as the Compose file mounts it. etcPath is the agent's own.
const (
	libPath = "/var/lib/agentiik"
	etcPath = "/etc/agentiik"
)

// caPath is where each agent finds the authority the terminator's certificate is signed by, as the
// Compose file mounts AGENTIIK_CA, and trusted is SSL_CERT_DIR as it sets it: init's certificate,
// which the bus presents, and that authority.
const (
	caPath  = "/etc/ssl/agentiik"
	trusted = etcPath + "/trust:" + caPath
)

// The daemon's socket, which the daemon writes in a volume of its own and the agent finds at the
// path it looks at when DOCKER_HOST is unset. socketGroup owns it, as the group that owns the
// socket does on a host, and the agent, started as root, reads it off the socket and takes it.
const (
	daemonSocketDir = "/run/agk-docker"
	agentSocketDir  = "/var/run"
	socketGroup     = "2375"
)

// runnerPolicy is each runner's /etc/agentiik/runner.toml, as the Compose file writes it with its
// default.
//
// require_userns_remap = false because a docker:dind daemon does not remap user namespaces, and
// the floor would refuse it: this installation tests what a server run does, and the remapped
// floor is held by the userns job of test.yml, on a daemon remapped as the page installs one.
const runnerPolicy = `# The test installation's runners, on docker:dind daemons, which do not remap user namespaces.
require_userns_remap = false
`

// Runner is one runner of the installation: its agent, in the runner image as the Compose file runs
// it, and the Docker daemon it drives, each in a container of its own.
type Runner struct {
	// Name is a or b, or the name a test gave the runner it joined to the pool default.
	Name string

	// ID is the identifier the API minted when it joined.
	ID string

	// Agent and Daemon are the names of the two containers.
	Agent, Daemon string

	in *Installation
}

// joining is how a runner is given its join token: a value, in AGK_RUNNER_JOIN_TOKEN, or none,
// which is the file init wrote in its runner volume, which the runner then mounts at /etc/agentiik
// and names in AGK_RUNNER_JOIN_TOKEN_FILE, as the Compose file's runner does.
type joining struct{ token string }

// joinWith is a join token given as a value.
func joinWith(token string) joining { return joining{token: token} }

// joinFromInit is the join token of the pool default init wrote for the runner beside it.
var joinFromInit = joining{}

// runner stands one runner up: a daemon, then the agent, which joins on its own with join,
// claiming labels, and serves. With no label it claims none, as a runner of the pool default does.
//
// The agent is started as the Compose file starts it: as root, in the runner image, with the
// capabilities it needs to prepare the host and drop to agentiik, the join token and the labels in
// its environment, and runner.toml mounted at /etc/agentiik/runner.toml. It gives itself its
// directories, takes the socket's group and joins; nothing is prepared for it here. Each agent has
// an /etc/agentiik of its own, since runner.env is written there: the runner of the pool default
// has init's runner volume, as the Compose file's has, and the others a directory holding init's
// certificate, as a runner on another machine would be given it.
//
// Each daemon is a docker:dind container of its own, so that runner b never sees runner a's
// containers: on one daemon, b would adopt a's container by its label, and a test of two machines
// would be a test of adoption. The daemons are on the installation's network with the registry,
// which they reach by its name. The agents join the host's network instead, where 127.0.0.1 is
// the terminator in front of the API and the bus, and reach their daemon through its socket
// alone.
//
// The daemons are not on the host's network, because a daemon started with no bridge removes the
// interface docker0 wherever it runs, which on the host's network is the host daemon's own.
func (in *Installation) runner(ctx context.Context, name string, join joining, labels ...string) *Runner {
	r := &Runner{Name: name, Agent: in.id + "-runner-" + name, Daemon: in.id + "-daemon-" + name, in: in}
	lib := in.volume(ctx, "lib-"+name)
	socket := in.volume(ctx, "socket-"+name)
	policy := in.path("runner.toml")
	if err := os.WriteFile(policy, []byte(runnerPolicy), 0o644); err != nil {
		in.t.Fatal(err)
	}

	in.container(ctx, r.Daemon, "run", "-d", "--name", r.Daemon, "--label", in.label(), "--userns", "host",
		"--privileged", "--network", in.network,
		"-v", socket+":"+daemonSocketDir,
		"-v", lib+":"+libPath,
		dindImage,
		"dockerd", "--host=unix://"+daemonSocketDir+"/docker.sock", "--group="+socketGroup,
		"--insecure-registry="+in.Registry)
	eventually(in.ctx, in.t, 2*time.Minute, "runner "+name+"'s daemon answered", func() error {
		_, err := r.daemon(ctx, "version")
		return err
	})

	etc := in.volumes["runner"]
	environment := []string{"-e", "AGK_API=" + in.PublicURL, "-e", "SSL_CERT_DIR=" + trusted}
	if join.token == "" {
		environment = append(environment, "-e", "AGK_RUNNER_JOIN_TOKEN_FILE="+etcPath+"/join-token")
	} else {
		etc = in.mkdir(0o755, "runner-"+name, "etc")
		trust := in.mkdir(0o755, "runner-"+name, "etc", "trust")
		if err := os.WriteFile(filepath.Join(trust, "agentiik.pem"), []byte(in.read(ctx, "runner/trust/agentiik.pem")+"\n"), 0o644); err != nil {
			in.t.Fatal(err)
		}
		environment = append(environment, "-e", "AGK_RUNNER_JOIN_TOKEN="+join.token)
	}
	if len(labels) > 0 {
		environment = append(environment, "-e", "AGK_RUNNER_LABELS="+strings.Join(labels, ","))
	}
	serving := append([]string{"run", "-d", "--name", r.Agent, "--label", in.label(), "--userns", "host",
		"--network", "host",
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE",
		"--cap-add", "SETUID", "--cap-add", "SETGID",
		"-v", etc + ":" + etcPath,
		"-v", policy + ":" + etcPath + "/runner.toml:ro",
		"-v", lib + ":" + libPath,
		"-v", socket + ":" + agentSocketDir,
		"-v", in.path("tls") + ":" + caPath + ":ro"}, environment...)
	in.container(ctx, r.Agent, append(serving, in.runnerIm, "serve")...)

	eventually(in.ctx, in.t, 2*time.Minute, "runner "+name+" joined", func() error {
		said, err := dockerCombined(ctx, "logs", r.Agent)
		if err != nil {
			return fmt.Errorf("%w: %s", err, said)
		}
		if r.ID = joinedAs(said); r.ID == "" {
			if !r.running(ctx) {
				return errStop(fmt.Errorf("runner %s's agent exited without joining:\n%s", name, said))
			}
			return errors.New("it has not said it joined")
		}
		return nil
	})
	return r
}

// daemon runs the docker command line against the runner's own daemon, inside its container.
func (r *Runner) daemon(ctx context.Context, args ...string) (string, error) {
	return docker(ctx, append([]string{"exec", r.Daemon, "docker", "-H", "unix://" + daemonSocketDir + "/docker.sock"}, args...)...)
}

// joinedAs reads the runner's identifier out of what joining said: agk-runner join's sentence, or
// the line serve logs when it joins on its own.
func joinedAs(said string) string {
	m := regexp.MustCompile(`joined pool \S+ as runner ([^\s,.]+)`).FindStringSubmatch(said)
	if m == nil {
		return ""
	}
	return m[1]
}

// volume makes a named volume, labelled as the installation's, and removes it with the
// installation. It answers the volume's name.
func (in *Installation) volume(ctx context.Context, name string, opts ...string) string {
	name = in.id + "-" + name
	args := append([]string{"volume", "create", "--label", in.label()}, opts...)
	if _, err := docker(ctx, append(args, name)...); err != nil {
		in.t.Fatal(err)
	}
	in.undo(func() {
		gone, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(gone, "volume", "rm", "-f", name)
	})
	return name
}

// running says whether the agent's container is still running.
func (r *Runner) running(ctx context.Context) bool {
	state, err := docker(ctx, "inspect", "--format", "{{.State.Running}}", r.Agent)
	return err == nil && state == "true"
}

// Kill ends the runner's agent with SIGKILL, as a host that loses its power or meets the kernel's
// OOM killer ends it: nothing is told, the agent's heartbeat stops mid-interval, and its daemon
// stays up with every container on it still running.
func (r *Runner) Kill(ctx context.Context) error {
	_, err := docker(ctx, "kill", "--signal", "KILL", r.Agent)
	return err
}

// Running answers the containers of run the runner's daemon is running now, by the step their
// label names, each as the idempotency key its task label carries.
func (r *Runner) Running(ctx context.Context, run string) (map[string][]string, error) {
	out, err := r.daemon(ctx, "ps", "--filter", "label="+driver.LabelRun+"="+run,
		"--format", `{{.Label "`+driver.LabelStep+`"}} {{.Label "`+driver.LabelTask+`"}}`)
	if err != nil {
		return nil, err
	}
	return byStep(out), nil
}

// Created answers every container the runner's daemon created for run from since until now, by
// the step their label names, each as the idempotency key its task label carries. It is read from
// the daemon's events rather than from its containers, since a runner removes each container once
// its task has ended and a daemon remembers that it created one.
func (r *Runner) Created(ctx context.Context, run string, since time.Time) (map[string][]string, error) {
	out, err := r.daemon(ctx, "events",
		"--since", strconv.FormatInt(since.Unix(), 10), "--until", strconv.FormatInt(time.Now().Unix(), 10),
		"--filter", "type=container", "--filter", "event=create", "--filter", "label="+driver.LabelRun+"="+run,
		"--format", `{{index .Actor.Attributes "`+driver.LabelStep+`"}} {{index .Actor.Attributes "`+driver.LabelTask+`"}}`)
	if err != nil {
		return nil, err
	}
	return byStep(out), nil
}

// byStep reads lines of a step and a key into the keys of each step, in the order they came.
func byStep(out string) map[string][]string {
	steps := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		step, key, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			steps[step] = append(steps[step], key)
		}
	}
	return steps
}

// Holdings is everything a runner holds: the files under the directories its agent is given,
// its environment, and what is mounted into it.
type Holdings struct {
	// Files are the regular files under /etc/agentiik and /var/lib/agentiik, by their path in
	// the agent's container, with their content.
	Files map[string][]byte

	// Directories are the directories under the same two, by their path.
	Directories []string

	// Env is the agent's environment, as its container was started with it.
	Env []string

	// Mounts are the sources mounted into the agent's container, each with where.
	Mounts []Mount

	// Added are the paths docker diff says were added to the agent's own filesystem, outside
	// everything mounted into it: what the agent wrote anywhere but where it is given to.
	Added []string

	// SecretsVolumes are the volumes of a task's secrets still on the runner's daemon, each made
	// for the task that is given a value and removed with it.
	SecretsVolumes []string
}

// Mount is one mount of the agent's container.
type Mount struct {
	Type, Source, Name, Destination string
}

// Holdings reads what the runner holds, from outside it: docker cp copies the two directories
// out whoever owns what is in them, docker inspect answers the environment and the mounts, and
// the runner's daemon lists the secrets volumes it still has.
func (r *Runner) Holdings(ctx context.Context) (Holdings, error) {
	h := Holdings{Files: map[string][]byte{}}
	for _, dir := range []string{etcPath, libPath} {
		archive, err := dockerBytes(ctx, "cp", r.Agent+":"+dir, "-")
		if err != nil {
			return Holdings{}, err
		}
		if err := h.read(filepath.Dir(dir), archive); err != nil {
			return Holdings{}, fmt.Errorf("the archive of %s: %w", dir, err)
		}
	}
	volumes, err := r.SecretsVolumes(ctx)
	if err != nil {
		return Holdings{}, err
	}
	h.SecretsVolumes = volumes

	diff, err := docker(ctx, "diff", r.Agent)
	if err != nil {
		return Holdings{}, err
	}
	for _, line := range strings.Split(diff, "\n") {
		if path, added := strings.CutPrefix(line, "A "); added {
			h.Added = append(h.Added, path)
		}
	}

	var inspected []struct {
		Config struct{ Env []string }
		Mounts []Mount
	}
	out, err := docker(ctx, "inspect", r.Agent)
	if err != nil {
		return Holdings{}, err
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil || len(inspected) != 1 {
		return Holdings{}, fmt.Errorf("docker inspect answered what does not decode: %v", err)
	}
	h.Env, h.Mounts = inspected[0].Config.Env, inspected[0].Mounts
	return h, nil
}

// SecretsVolumes answers the volumes of a task's secrets the runner's daemon still has. The driver
// makes one for each task given a value and removes it with the task, so once every task has
// ended there is none.
func (r *Runner) SecretsVolumes(ctx context.Context) ([]string, error) {
	out, err := r.daemon(ctx, "volume", "ls", "--quiet", "--filter", "name="+secretsVolumePrefix)
	if err != nil {
		return nil, err
	}
	var volumes []string
	for _, v := range strings.Fields(out) {
		// The filter matches a name anywhere, and a secrets volume's begins with the prefix.
		if strings.HasPrefix(v, secretsVolumePrefix) {
			volumes = append(volumes, v)
		}
	}
	return volumes, nil
}

// read adds what a tar archive holds, its paths taken as under parent.
func (h *Holdings) read(parent string, archive []byte) error {
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(parent, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			h.Directories = append(h.Directories, path)
		case tar.TypeReg:
			content, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			h.Files[path] = content
		default:
			// A link, a socket or a device is held as a file with no content, so that
			// what is there is still named.
			h.Files[path] = nil
		}
	}
}

// dockerBytes is docker, with its standard output as it was written.
func dockerBytes(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// secretsVolumePrefix begins the name of every volume the driver gives a task's secret values on,
// which is removed with the task.
const secretsVolumePrefix = "agk-secrets-"

// What a runner holds, and what it must not, in its own words: "a runner never speaks git, holds
// no lasting credential beyond its own runner identity, and obtains what its task names only by
// redeeming that task's grant". Its identity is runner.env, which holds AGK_API and the runner
// credential, and its key; runner.toml is the host's own settings, and trust/agentiik.pem and
// join-token are what init gives the runner beside it, as the Compose file mounts them: the
// certificate the bus presents, and a join token of the pool default, spent by the one join that
// uses it and valid an hour.
var (
	// etcFiles are the files /etc/agentiik may hold, and nothing else.
	etcFiles = []string{etcPath + "/runner.env", etcPath + "/runner.toml", etcPath + "/trust/agentiik.pem", etcPath + "/join-token"}

	// keyFile is the host's key, which proves the machine.
	keyFile = libPath + "/runner.key"

	// runnerEnvKeys are the settings runner.env may carry, which are the runner's own: its
	// address of the API, its identity and its host settings.
	runnerEnvKeys = map[string]bool{
		"AGK_API": true, "AGK_RUNNER_ID": true, "AGK_RUNNER_POOL": true, "AGK_RUNNER_CREDENTIAL": true,
		"AGK_RUNNER_LABELS": true, "AGK_RUNNER_CONCURRENCY": true, "AGK_RUNNER_WORKDIR": true, "AGK_RUNNER_NAMESPACES": true,
	}

	// agentEnvKeys are the settings the agent's environment may carry besides: the join token
	// it joins with on its own, as a value or a file, and where it trusts certificates.
	agentEnvKeys = map[string]bool{"AGK_RUNNER_JOIN_TOKEN": true, "AGK_RUNNER_JOIN_TOKEN_FILE": true}

	// agentMounts are where the agent's container has something mounted: the two directories,
	// runner.toml, the daemon's socket, and the authority it trusts.
	agentMounts = map[string]bool{
		etcPath: true, etcPath + "/runner.toml": true, libPath: true, agentSocketDir: true, caPath: true,
	}

	// A bus credential at rest, whichever way it is written: the decorated file nats and nsc
	// write, a JWT of any kind, or an NKey seed of a user, an account or an operator.
	natsDecoration = regexp.MustCompile(`-----BEGIN NATS [A-Z ]+-----`)
	jwtHeader      = regexp.MustCompile(`eyJ0eXAiOiJKV1Qi|eyJhbGciOi`)
	nkeySeed       = regexp.MustCompile(`S[UAO][A-Z2-7]{56}`)
)

// breaches holds what a runner holds to what it may, and answers each breach in a sentence.
// publicURL is the one address it is given; held are the installation's values it must hold
// nowhere; forbidden are the volumes, by name, and the directories of this machine that must not
// be mounted into it.
func (h Holdings) breaches(publicURL string, held []heldValue, forbidden []string) []string {
	var broken []string
	for path := range h.Files {
		if strings.HasPrefix(path, etcPath+"/") && !slices.Contains(etcFiles, path) {
			broken = append(broken, fmt.Sprintf("%s is in %s, which holds runner.env, runner.toml, trust/agentiik.pem and join-token alone", path, etcPath))
		}
	}

	env, ok := h.Files[etcPath+"/runner.env"]
	if !ok {
		broken = append(broken, "there is no runner.env, which holds the runner's identity")
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(env), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		settings[key] = value
		if !runnerEnvKeys[key] {
			broken = append(broken, fmt.Sprintf("runner.env sets %s, which is not a runner's setting", key))
		}
	}
	if ok && settings["AGK_API"] != publicURL {
		broken = append(broken, fmt.Sprintf("runner.env gives AGK_API as %q, and the one address a runner is given is %s", settings["AGK_API"], publicURL))
	}
	if ok && settings["AGK_RUNNER_CREDENTIAL"] == "" {
		broken = append(broken, "runner.env holds no runner credential")
	}
	if _, ok := h.Files[keyFile]; !ok {
		broken = append(broken, "there is no "+keyFile+", which is the host's key")
	}

	for _, path := range append(slices.Sorted(maps.Keys(h.Files)), h.Directories...) {
		for _, part := range strings.Split(path, "/") {
			if part == ".git" || part == "agentiik.yaml" {
				broken = append(broken, fmt.Sprintf("%s is held at rest, and a runner never holds a repository", path))
			}
		}
	}
	for _, path := range slices.Sorted(maps.Keys(h.Files)) {
		broken = append(broken, heldIn("the file "+path, string(h.Files[path]), held)...)
	}
	for _, v := range h.Env {
		name, _, _ := strings.Cut(v, "=")
		// A host setting may be in the environment, as the Compose file sets the labels, the
		// concurrency and the join token; the credential never is, and nothing else of AGK_ is
		// a runner's.
		switch {
		case name == "AGK_RUNNER_CREDENTIAL":
			broken = append(broken, "the agent's environment sets AGK_RUNNER_CREDENTIAL, and a credential is read from runner.env alone, never inherited by what the agent starts")
		case strings.HasPrefix(name, "AGK_") && !runnerEnvKeys[name] && !agentEnvKeys[name]:
			broken = append(broken, fmt.Sprintf("the agent's environment sets %s, which is not a runner's setting", name))
		}
		broken = append(broken, heldIn("the agent's environment", v, held)...)
	}
	for _, path := range h.Added {
		// The mount points themselves are made when the container is, and hold nothing.
		if !agentMounts[path] && !slices.ContainsFunc(slices.Collect(maps.Keys(agentMounts)), func(m string) bool { return strings.HasPrefix(m, path+"/") }) {
			broken = append(broken, fmt.Sprintf("%s was written in the agent's own filesystem, and it keeps nothing outside %s and %s", path, etcPath, libPath))
		}
	}
	for _, v := range h.SecretsVolumes {
		broken = append(broken, fmt.Sprintf("the volume %s is still on the runner's daemon, and a task's secrets volume is removed with the task", v))
	}
	for _, m := range h.Mounts {
		if !agentMounts[m.Destination] {
			broken = append(broken, fmt.Sprintf("%s is mounted at %s, which the agent is not given", m.Source, m.Destination))
		}
		for _, dir := range forbidden {
			if m.Name == dir || m.Source == dir || strings.HasPrefix(m.Source, dir+"/") {
				broken = append(broken, fmt.Sprintf("%s is mounted at %s, and it is the installation's, which no runner sees", m.Source, m.Destination))
			}
		}
	}
	return broken
}

// heldIn answers a sentence for every held value text carries, and for a bus credential in any
// of its forms.
func heldIn(where, text string, held []heldValue) []string {
	var found []string
	for _, v := range held {
		if v.value != "" && strings.Contains(text, v.value) {
			found = append(found, fmt.Sprintf("%s holds %s", where, v.what))
		}
	}
	for _, p := range []struct {
		re   *regexp.Regexp
		what string
	}{
		{natsDecoration, "a NATS credential file"},
		{jwtHeader, "a JWT, which is what a bus credential is"},
		{nkeySeed, "an NKey seed, which is what signs as a bus user or account"},
	} {
		if p.re.MatchString(text) {
			found = append(found, fmt.Sprintf("%s holds %s", where, p.what))
		}
	}
	return found
}
