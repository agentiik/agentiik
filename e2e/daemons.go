package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The paths a runner's agent and its daemon share, each the same on both sides, since the daemon
// resolves every bind source it is asked for in its own filesystem: the work root under
// /var/lib/agentiik and the secrets tmpfs, as the page's Compose sample mounts them.
const (
	libPath     = "/var/lib/agentiik"
	secretsPath = "/run/agentiik/secrets"
	etcPath     = "/etc/agentiik"
)

// The daemon's socket, which the daemon writes in a volume of its own and the agent finds at the
// path it looks at when DOCKER_HOST is unset. socketGroup owns it, and the agent is given that
// group and no other way in, as the group that owns the socket is on a host.
const (
	daemonSocketDir = "/run/agk-docker"
	agentSocketDir  = "/var/run"
	socketGroup     = "2375"
)

// agentUser is the account the runner image runs the agent as, agentiik.
const agentUser = "65532"

// runnerPolicy is each runner's /etc/agentiik/runner.toml.
//
// require_userns_remap = false because a docker:dind daemon does not remap user namespaces, and
// the floor would refuse it: this installation tests what a server run does, and the remapped
// floor is held by the userns job of test.yml, on a daemon remapped as the page installs one.
// secrets_dir names the tmpfs of the runner's own, mounted noexec,nosuid,nodev, which a runner
// is refused without.
const runnerPolicy = `# The test installation's runners, on docker:dind daemons, which do not remap user namespaces.
require_userns_remap = false
secrets_dir = "/run/agentiik/secrets"
`

// Runner is one runner of the installation: its agent, in the runner image as the container form
// runs it, and the Docker daemon it drives, each in a container of its own.
type Runner struct {
	// Name is a or b.
	Name string

	// ID is the identifier the API minted when it joined.
	ID string

	// Agent and Daemon are the names of the two containers.
	Agent, Daemon string

	in *Installation
}

// runner stands one runner up: a daemon, then join with token, then serve.
//
// The agent runs in the runner image rather than as a process of this machine, because the paths
// it reads, /etc/agentiik/runner.env, runner.toml and /var/lib/agentiik/runner.key, are fixed,
// and two agents on one filesystem would share them. The container form gives each its own, which
// is also a form the page installs a runner in.
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
func (in *Installation) runner(ctx context.Context, name, token string) *Runner {
	r := &Runner{Name: name, Agent: in.id + "-runner-" + name, Daemon: in.id + "-daemon-" + name, in: in}
	lib := in.volume(ctx, "lib-"+name)
	socket := in.volume(ctx, "socket-"+name)
	// A tmpfs with the flags the secrets floor holds it to, owned by the agent. One mount,
	// which both containers bind, so a value the agent writes is the value the daemon binds.
	secrets := in.volume(ctx, "secrets-"+name, "--driver", "local",
		"--opt", "type=tmpfs", "--opt", "device=tmpfs",
		"--opt", "o=noexec,nosuid,nodev,size=16m,mode=0700,uid="+agentUser+",gid="+agentUser)
	etc := in.mkdir(0o755, "runner-"+name, "etc")
	if err := os.WriteFile(filepath.Join(etc, "runner.toml"), []byte(runnerPolicy), 0o644); err != nil {
		in.t.Fatal(err)
	}
	// /var/lib/agentiik is the agent's, as the page has it, and a volume is root's when made.
	if _, err := docker(ctx, "run", "--rm", "--network", "none", "-v", lib+":"+libPath, alpineImage, "chown", agentUser+":"+agentUser, libPath); err != nil {
		in.t.Fatal(err)
	}

	in.container(ctx, r.Daemon, "run", "-d", "--name", r.Daemon, "--label", in.label(),
		"--privileged", "--network", in.network,
		"-v", socket+":"+daemonSocketDir,
		"-v", lib+":"+libPath,
		"-v", secrets+":"+secretsPath,
		dindImage,
		"dockerd", "--host=unix://"+daemonSocketDir+"/docker.sock", "--group="+socketGroup,
		"--insecure-registry="+in.Registry)
	eventually(in.t, 2*time.Minute, "runner "+name+"'s daemon answered", func() error {
		_, err := r.daemon(ctx, "version")
		return err
	})

	shared := []string{
		"--network", "host",
		"-v", etc + ":" + etcPath,
		"-v", lib + ":" + libPath,
		"-v", socket + ":" + agentSocketDir,
		"-v", in.path("tls", "ca.pem") + ":/etc/ssl/certs/ca-certificates.crt:ro",
	}

	// join runs as root in the image, as the page runs it, and gives the key and runner.env
	// to the agent's account.
	joining := append([]string{"run", "--rm", "--user", "0:0"}, shared...)
	joining = append(joining, in.runnerIm, "join", "--api", in.PublicURL, "--token", token, "--labels", Label)
	said, err := docker(ctx, joining...)
	if err != nil {
		in.t.Fatalf("runner %s could not join: %s", name, err)
	}
	if r.ID = joinedAs(said); r.ID == "" {
		in.t.Fatalf("runner %s joined and did not say as whom:\n%s", name, said)
	}

	// serve as the page's Compose sample runs it: as agentiik, in the group that owns the
	// socket, holding the three capabilities and no other.
	serving := append([]string{"run", "-d", "--name", r.Agent, "--label", in.label(),
		"--user", agentUser + ":" + agentUser, "--group-add", socketGroup,
		"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE",
		"-v", secrets + ":" + secretsPath}, shared...)
	serving = append(serving, in.runnerIm, "serve")
	in.container(ctx, r.Agent, serving...)
	return r
}

// daemon runs the docker command line against the runner's own daemon, inside its container.
func (r *Runner) daemon(ctx context.Context, args ...string) (string, error) {
	return docker(ctx, append([]string{"exec", r.Daemon, "docker", "-H", "unix://" + daemonSocketDir + "/docker.sock"}, args...)...)
}

// joinedAs reads the runner's identifier out of what join said.
func joinedAs(said string) string {
	m := regexp.MustCompile(`joined pool \S+ as runner (\S+)\.`).FindStringSubmatch(said)
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
}

// Mount is one mount of the agent's container.
type Mount struct {
	Type, Source, Name, Destination string
}

// Holdings reads what the runner holds, from outside it: docker cp copies the two directories
// out whoever owns what is in them, and docker inspect answers the environment and the mounts.
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

// What a runner holds, and what it must not, in its own words: "a runner never speaks git, holds
// no lasting credential beyond its own runner identity, and obtains what its task names only by
// redeeming that task's grant". Its identity is runner.env, which holds AGK_API and the runner
// credential, and its key; runner.toml is the host's own settings.
var (
	// etcFiles are the files /etc/agentiik holds, and nothing else.
	etcFiles = []string{etcPath + "/runner.env", etcPath + "/runner.toml"}

	// keyFile is the host's key, which proves the machine.
	keyFile = libPath + "/runner.key"

	// runnerEnvKeys are the settings runner.env may carry, which are the runner's own: its
	// address of the API, its identity and its host settings.
	runnerEnvKeys = map[string]bool{
		"AGK_API": true, "AGK_RUNNER_ID": true, "AGK_RUNNER_POOL": true, "AGK_RUNNER_CREDENTIAL": true,
		"AGK_RUNNER_LABELS": true, "AGK_RUNNER_CONCURRENCY": true, "AGK_RUNNER_WORKDIR": true, "AGK_RUNNER_NAMESPACES": true,
	}

	// agentMounts are where the agent's container has something mounted: the two directories,
	// the secrets tmpfs, the daemon's socket, and the authority it trusts.
	agentMounts = map[string]bool{
		etcPath: true, libPath: true, secretsPath: true, agentSocketDir: true,
		"/etc/ssl/certs/ca-certificates.crt": true,
	}

	// A bus credential at rest, whichever way it is written: the decorated file nats and nsc
	// write, a JWT of any kind, or an NKey seed of a user, an account or an operator.
	natsDecoration = regexp.MustCompile(`-----BEGIN NATS [A-Z ]+-----`)
	jwtHeader      = regexp.MustCompile(`eyJ0eXAiOiJKV1Qi|eyJhbGciOi`)
	nkeySeed       = regexp.MustCompile(`S[UAO][A-Z2-7]{56}`)
)

// breaches holds what a runner holds to what it may, and answers each breach in a sentence.
// publicURL is the one address it is given; held are the installation's values it must hold
// nowhere; forbidden are the directories of this machine that must not be mounted into it.
func (h Holdings) breaches(publicURL string, held []heldValue, forbidden []string) []string {
	var broken []string
	for path := range h.Files {
		if strings.HasPrefix(path, etcPath+"/") && !slices.Contains(etcFiles, path) {
			broken = append(broken, fmt.Sprintf("%s is in %s, which holds runner.env and runner.toml alone", path, etcPath))
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
		if strings.HasPrefix(name, "AGK_") {
			broken = append(broken, fmt.Sprintf("the agent's environment sets %s, and its settings are in runner.env, which it reads itself", name))
		}
		broken = append(broken, heldIn("the agent's environment", v, held)...)
	}
	for _, m := range h.Mounts {
		if !agentMounts[m.Destination] {
			broken = append(broken, fmt.Sprintf("%s is mounted at %s, which the agent is not given", m.Source, m.Destination))
		}
		for _, dir := range forbidden {
			if m.Source == dir || strings.HasPrefix(m.Source, dir+"/") {
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
