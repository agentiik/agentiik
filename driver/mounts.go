package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// The four paths of the contract that brick does not name, plus the two the script
// keywords name. Package brick names Root, InDir and OutDir because those are the two
// edges it reads and writes; these are the rest of what the figure lists, and they live
// here because it is the driver that binds them. Moving them into brick later is an
// addition to that package rather than a change to it.
const (
	// RepoDir is where the workflow repository tree is bound, "mounted read-only at
	// /agk/repo/ in every step", and what AGK_REPO points at.
	RepoDir = "/agk/repo"

	// RunPath is the run context, read-only.
	RunPath = "/agk/run.json"

	// ParamsPath is the resolved parameters, read-only.
	ParamsPath = "/agk/params.json"

	// SecretsDir is the container side of a secret mount, "/agk/secrets/<name>",
	// which is where a brick manifest may ask for one and nowhere else. The host
	// side is Policy.SecretsDir, a tmpfs where the platform has one.
	SecretsDir = "/agk/secrets"

	// BinPath is where the static helper a script step may use is mounted:
	// "/agk/bin/agk is mounted read-only: a static helper for scripts that want to be
	// precise rather than lucky". It is a convenience and never a requirement, so it
	// is bound where the runner has the binary to bind, Policy.Helper, and absent
	// where it does not; the path is named here so that there is one spelling of it.
	BinPath = "/agk/bin/agk"

	// TmpDir is the other writable path. It is a sized tmpfs and nothing is
	// collected from it.
	TmpDir = "/tmp"
)

// Secrets is where a value comes from at the last moment.
//
// It is one method because the driver asks one question: what is this secret worth, now,
// for this task. A server runner answers it "by redeeming at the API the per-task grant
// the controller issued for that one task and that one secret", and agk run --local
// answers it off the command line. Neither is this package's business, which is why the
// source arrives at construction and not in a Task: a Task "carries no secret value at
// all".
type Secrets interface {
	Value(ctx context.Context, name string) ([]byte, error)
}

// secretMountRule is where a secret may be asked for, quoted from the manifest rules so
// that a refusal prints the rule rather than a paraphrase of it.
const secretMountRule = `^/agk/secrets/[^/]+$`

var secretMountPattern = regexp.MustCompile(secretMountRule)

// given is the host side of one task: what will be bound into the container, what will
// be a tmpfs inside it, what goes on standard input, and the secret values that were
// redeemed to build it.
//
// The values are carried out of here because masking is "a literal match against the
// values the task was given", and this is where a task is given them. Nothing else in
// the driver redeems a secret, so nothing else could hold the list the masker needs.
type given struct {
	Mounts []docker.Mount
	Tmpfs  map[string]string
	Stdin  []byte
	Values [][]byte
}

// prepare writes the host side of the contract under the task's working directory and
// says what the container is to be given.
//
// The order is the order of the figure: the envelope on standard input and under
// /agk/in/<port>/, the repository tree, the run context, the parameters, the secrets,
// and then the two writable paths. Nothing here starts anything or talks to a daemon: it
// is files on a disk and a slice of mounts, which is why every one of these rules is
// tested with no Docker in reach.
func prepare(ctx context.Context, t graph.Task, w *workdir, p Policy, store *artifact.Store, run agk.Run, repo string, secrets Secrets) (*given, error) {
	g := &given{Tmpfs: map[string]string{}}

	in, err := brick.WriteInputs(ctx, store, w.In, t.Inputs)
	if err != nil {
		return nil, fmt.Errorf("driver: step %s: %w", t.Step, err)
	}
	for _, m := range in {
		g.Mounts = append(g.Mounts, bind(m.Source, m.Target, m.ReadOnly))
	}
	if g.Stdin, err = stdinBytes(t); err != nil {
		return nil, err
	}

	repoMounts, err := repoBinds(t, repo)
	if err != nil {
		return nil, err
	}
	g.Mounts = append(g.Mounts, repoMounts...)

	if err := writeJSON(w.Run, run); err != nil {
		return nil, fault(t.Step, err, ChargePlatform, "%s could not be written under the working directory", RunPath)
	}
	g.Mounts = append(g.Mounts, bind(w.Run, RunPath, true))

	// The parameters are written unexamined. They arrive resolved, with every
	// expression evaluated, the matrix combination injected and the whole validated
	// against the manifest schema, so there is nothing here for the driver to have an
	// opinion about. An absent params is written as an empty object rather than as
	// null, because a brick reading the file expects a document with keys in it.
	params := t.Params
	if params == nil {
		params = map[string]any{}
	}
	if err := writeJSON(w.Params, params); err != nil {
		return nil, fault(t.Step, err, ChargePlatform, "%s could not be written under the working directory", ParamsPath)
	}
	g.Mounts = append(g.Mounts, bind(w.Params, ParamsPath, true))

	helper, err := helperBind(t, p)
	if err != nil {
		return nil, err
	}
	g.Mounts = append(g.Mounts, helper...)

	secretMounts, values, err := writeSecrets(ctx, t, w, secrets)
	if err != nil {
		return nil, err
	}
	g.Mounts = append(g.Mounts, secretMounts...)
	g.Values = values

	// /agk/out is a bind from the working directory and not a tmpfs. A tmpfs is
	// unmounted when the container stops, so an output written to one is gone before
	// anything can collect it, and collecting before the exit is not sound because a
	// brick writes until its last instant.
	g.Mounts = append(g.Mounts, bind(w.Out, brick.OutDir, false))

	// /tmp is the one writable path nothing is collected from, so it is the one that
	// can be a tmpfs. The flags are the settings table's own, and the size is the
	// policy's, because a tmpfs takes its pages from the host's memory.
	g.Tmpfs[TmpDir] = tmpfsOptions(p.TmpSize)

	return g, nil
}

// helperBind mounts the static helper a script step may use, where the runner has one to
// mount.
//
// "/agk/bin/agk is mounted read-only: a static helper for scripts that want to be precise
// rather than lucky, with agk items, agk emit and agk attach. It is a convenience, never
// a requirement." It is named in the runner's own configuration and not in a workflow,
// because which binary is on this host is a fact about the host; a runner that names none
// binds none, and the documented example, which pipes agk items through jq, is then a
// step that says "agk: not found" rather than a step that ran.
//
// It is bound for a script step alone, which is where the documentation offers it: a
// brick is an image that honours the contract on its own, and putting a binary of ours
// inside one would be an Agentiik library after all.
//
// A path the policy names and the host does not have refuses the task rather than being
// skipped. The daemon would otherwise create a directory at the source and bind that, so
// the step would find a directory where it was promised a program.
func helperBind(t graph.Task, p Policy) ([]docker.Mount, error) {
	if p.Helper == "" || !isScript(t) {
		return nil, nil
	}
	info, err := os.Stat(p.Helper)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fault(t.Step, nil, ChargePlatform, "%s is named as the static helper in %s and is not a file on this host: it is mounted read-only at %s for a script step, and a path that is not there would be bound as a directory", p.Helper, PolicyPath, BinPath)
	}
	return []docker.Mount{bind(p.Helper, BinPath, true)}, nil
}

// bind is one directory or file of the host under one path in the container.
func bind(source, target string, readOnly bool) docker.Mount {
	return docker.Mount{
		Type:     docker.MountBind,
		Source:   source,
		Target:   target,
		ReadOnly: readOnly,
	}
}

// tmpfsOptions writes the mount options of the writable tmpfs: the three flags the
// settings table names, and a size, because "the only writable paths are /agk/out and
// /tmp, mounted as sized tmpfs".
func tmpfsOptions(size int64) string {
	opts := "rw,noexec,nosuid,nodev"
	if size > 0 {
		opts += ",size=" + fmt.Sprint(size)
	}
	return opts
}

// stdinBytes is the envelope the container reads on standard input, encoded as it
// travels, or nothing where there is none.
//
// Standard input is one stream and an envelope is one document, so a step reading from
// several ports reads them under /agk/in/<port>/ and gets the port named in on standard
// input if it has one. A step with one input port gets that one whatever it is called,
// which is what makes the shorthand worth having. A step with no inputs gets an empty
// stream rather than an invented envelope: an envelope minted here would carry a meta
// block naming a step and a port that never produced it.
func stdinBytes(t graph.Task) ([]byte, error) {
	port, ok := stdinPort(t)
	if !ok {
		return nil, nil
	}
	var b bytes.Buffer
	if _, err := t.Inputs[port].Encode(&b); err != nil {
		return nil, &Fault{
			Step:   t.Step,
			Port:   port,
			Charge: ChargePlatform,
			Detail: "the envelope could not be written to standard input: " + err.Error(),
			err:    err,
		}
	}
	return b.Bytes(), nil
}

// stdinPort names the port whose envelope goes on standard input.
func stdinPort(t graph.Task) (agk.Port, bool) {
	if len(t.Inputs) == 1 {
		for port := range t.Inputs {
			return port, true
		}
	}
	if _, ok := t.Inputs["in"]; ok {
		return "in", true
	}
	return "", false
}

// repoBinds mounts the workflow repository tree and whatever a file selector relocates.
//
// "The whole tree is mounted read-only at /agk/repo/ in every step." Narrowing it is
// "an optimisation for large repositories, never a requirement, and never a permission
// boundary", and it happens where the tree is materialised rather than here, since what
// arrives is a directory. What does belong here is the long form's other half, which
// "relocates a path to wherever a tool insists on finding it": a tool that will only read
// /etc/ssl/certs/internal-ca.pem has to find it there, and a bind is what puts it there.
func repoBinds(t graph.Task, repo string) ([]docker.Mount, error) {
	if repo == "" {
		// A run with no repository tree behind it, which is what agk brick test
		// is, mounts nothing. Binding an empty path would create a directory on
		// the host and call it the workflow.
		return nil, nil
	}
	mounts := []docker.Mount{bind(repo, RepoDir, true)}
	for _, f := range t.Files {
		if f.To == "" {
			continue
		}
		if !path.IsAbs(f.To) {
			return nil, fault(t.Step, nil, ChargeBrick, "files: %q relocates a path to %q, which is not absolute: the long form of a selector names where in the container a tool insists on finding the file", f.From, f.To)
		}
		source, err := inside(repo, f.From)
		if err != nil {
			return nil, fault(t.Step, nil, ChargeBrick, "files: %v", err)
		}
		mounts = append(mounts, bind(source, path.Clean(f.To), true))
	}
	return mounts, nil
}

// inside resolves one path of a selector against the tree and refuses one that leaves
// it. A workflow may read anything in its own repository and nothing outside it, and
// ../../etc/shadow is a path that leaves.
func inside(repo, rel string) (string, error) {
	clean := filepath.Join(repo, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
	if clean != repo && !strings.HasPrefix(clean, repo+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q leaves the repository tree: a selector names a path relative to the root of the workflow repository", rel)
	}
	return clean, nil
}

// writeSecrets redeems the values and lays them down where the manifest asked for them.
//
// The value is obtained "at the last moment", here, and never travels on the task
// message: what a Task carries is "names and mount points and never values". Each one
// becomes a file of its own, bound read-only at its mount point, so a brick opens a path
// and the value is never in an environment "readable by its children" and in "diagnostic
// dumps".
//
// The settings table names the secret mount points in its Tmpfs row, and this is a bind
// instead, for a reason that is a property of the daemon: a tmpfs the daemon creates at
// container start is empty and cannot be pre-populated, so a value could not be placed in
// one before the container's first instruction runs. The host side is the tmpfs instead,
// Policy.SecretsDir, and where the platform has none the driver says so once.
func writeSecrets(ctx context.Context, t graph.Task, w *workdir, secrets Secrets) ([]docker.Mount, [][]byte, error) {
	if len(t.Secrets) == 0 {
		return nil, nil, nil
	}
	if secrets == nil {
		return nil, nil, fault(t.Step, nil, ChargePlatform, "%d secrets to redeem and no secret source: a runner redeems at the API the per-task grant the controller issued for that one task and that one secret, and agk run --local reads the command line", len(t.Secrets))
	}

	// Sorted by name, so that two runs of one step prepare the same mounts in the
	// same order, which is what makes a created container legible when it is read
	// twice.
	wanted := slices.Clone(t.Secrets)
	slices.SortFunc(wanted, func(a, b graph.SecretMount) int { return strings.Compare(a.Name, b.Name) })

	var mounts []docker.Mount
	var values [][]byte
	seen := map[string]bool{}
	for _, s := range wanted {
		target := s.Mount
		if target == "" {
			target = SecretsDir + "/" + s.Name
		}
		if !secretMountPattern.MatchString(target) {
			return nil, nil, fault(t.Step, ErrContractBroken, ChargeBrick, "secret %s is mounted at %q: a secret is mounted on tmpfs at /agk/secrets/<name>, %s, and a mount elsewhere, /run/secrets/bearer out of habit, is refused", s.Name, target, secretMountRule)
		}
		if seen[target] {
			return nil, nil, fault(t.Step, ErrContractBroken, ChargeBrick, "secret %s: two secrets are mounted at %q, and one path cannot hold both", s.Name, target)
		}
		seen[target] = true

		value, err := secrets.Value(ctx, s.Name)
		if err != nil {
			return nil, nil, fault(t.Step, err, ChargePlatform, "secret %s could not be redeemed at the last moment, as the per-task grant is", s.Name)
		}
		// The file is named by the mount point and not by the secret's own name.
		// The manifest chooses where a value is read from, and two secrets of
		// different names may not collide on the host any more than they may
		// collide in the container.
		source := filepath.Join(w.Secrets, path.Base(target))
		if err := writeSecret(source, value); err != nil {
			return nil, nil, fault(t.Step, err, ChargePlatform, "secret %s could not be written where it is bound from", s.Name)
		}
		mounts = append(mounts, bind(source, target, true))
		values = append(values, value)
	}
	return mounts, values, nil
}

// writeSecret puts one value on the disk, or on the tmpfs where the platform has one.
//
// It is created private and made readable afterwards rather than being created readable,
// so that the window in which the file exists with a mode wider than it needs is no
// window at all. The mode it ends on is readable because the account that reads it is
// the container's and not this process's.
func writeSecret(path string, value []byte) error {
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(value); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return os.Chmod(path, secretMode)
}

// writeJSON writes one document of the contract, read-only to the container.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	os.Remove(path)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	// Readable by the container's account, which is not the account that wrote it,
	// and writable by neither: the bind is read-only and so is the file behind it.
	return os.Chmod(path, 0o444)
}
