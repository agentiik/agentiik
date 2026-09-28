package driver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/docker"
)

// The paths of the contract the driver binds besides the two edges, plus the two the
// script keywords name. Package brick names Root, InDir and OutDir because those are the
// two edges it reads and writes, and RepoDir, which the API spells too; these are the rest
// of what the figure lists, and they live here because it is the driver that binds them.
// Moving them into brick later is an addition to that package rather than a change to it.
const (
	// RepoDir is where the workflow repository tree is bound, "mounted read-only at
	// /agk/repo/ in every step", and what AGK_REPO points at. It is brick's, which
	// names it for the API as well.
	RepoDir = brick.RepoDir

	// RunPath is the run context, read-only.
	RunPath = "/agk/run.json"

	// ParamsPath is the resolved parameters, read-only.
	ParamsPath = "/agk/params.json"

	// SecretsDir is where a task's secret values are, "/agk/secrets/<name>", which is
	// where a brick manifest may ask for one and nowhere else. It is a tmpfs volume of
	// the task's own, and no directory of the host.
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

// Secrets is where a value comes from when the container is prepared.
//
// It is one method because the driver asks one question: what is this secret worth, now,
// for this task. A server runner answers it from what it was given "by redeeming at the
// API the per-task grant the controller issued for that one task and that one secret",
// which it did before it pulled the image, and agk run --local answers it off the command
// line. Neither is this package's business, which is why the source arrives at
// construction, or with one task's Run in Sources, and never in a Task: a Task "carries
// no secret value at all". A source given in Sources is that task's alone, which is what
// lets it be asked by name: two tasks from two namespaces that both name billing are
// asked of two sources.
type Secrets interface {
	Value(ctx context.Context, name string) ([]byte, error)
}

// secretMountRule is where a secret may be asked for, quoted from the manifest rules so
// that a refusal prints the rule rather than a paraphrase of it. It is the grammar the brick
// manifest, the task message and the grant redemption all hold a mount to, and it is narrower
// than one path segment on purpose: the file on the task's secrets volume is named after the
// last element of the mount, so /agk/secrets/. would name the volume itself and /agk/secrets/..
// its parent.
const secretMountRule = `^/agk/secrets/[A-Za-z0-9][A-Za-z0-9._-]*$`

var secretMountPattern = regexp.MustCompile(secretMountRule)

// given is the host side of one task: what will be bound into the container, what will
// be a tmpfs inside it, what goes on standard input, and the secret values that were
// redeemed to build it.
//
// The values are carried out of here because masking is "a literal match against the
// values the task was given", and this is where a task is given them. Nothing else in
// the driver redeems a secret, so nothing else could hold the list the masker needs.
//
// Secrets are the same values by the file each is written in on the task's secrets volume,
// which is filled once the working directory is settled and just before the container is
// created, and hold is what keeps that volume filled until the container has started.
type given struct {
	Mounts  []docker.Mount
	Tmpfs   map[string]string
	Stdin   []byte
	Values  [][]byte
	Secrets []secretFile
	hold    *holder

	// Placed are the copies placeFiles made of the files a step's files place, which are
	// given to the container's own account once the working directory has been given to
	// the range (ownPlaced).
	Placed []placedCopy
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

	sources := sourcesOf(ctx)
	repoMounts, placed, err := repoBinds(t, repo, w, sources.Placed, sources.Repo != "")
	if err != nil {
		return nil, err
	}
	g.Mounts = append(g.Mounts, repoMounts...)
	g.Placed = placed

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

	// The values are redeemed here and written nowhere yet: they go on the task's secrets
	// volume, which Run fills once the working directory is settled.
	if g.Secrets, g.Values, err = redeemSecrets(ctx, t, secrets); err != nil {
		return nil, err
	}

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
	if !helperIsFile(p.Helper) {
		return nil, fault(t.Step, nil, ChargePlatform, "%s is named as the static helper in %s and is not a file on this host: it is mounted read-only at %s for a script step, and a path that is not there would be bound as a directory", p.Helper, PolicyPath, BinPath)
	}
	return []docker.Mount{bind(p.Helper, BinPath, true)}, nil
}

// helperIsFile says whether the helper the policy names is a file on this host.
func helperIsFile(helper string) bool {
	info, err := os.Stat(helper)
	return err == nil && info.Mode().IsRegular()
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

// repoBinds mounts the workflow repository tree and whatever a file selector places elsewhere.
//
// "The whole tree is mounted read-only at /agk/repo/ in every step." Narrowing it is
// "an optimisation for large repositories, never a requirement, and never a permission
// boundary", and it happens where the tree is materialised rather than here, since what
// arrives is a directory. What does belong here is the long form's other half, which
// "relocates a path to wherever a tool insists on finding it", with "mode applies to every
// file it places": a tool that will only read /etc/ssl/certs/internal-ca.pem has to find it
// there, and a postgres image reads its init scripts from one directory and nowhere else.
//
// placed are the files the runner laid out for the ones its redemption placed. Nil is a
// redemption that placed nothing, or none at all, and the step's files are then read against
// the tree itself, with the selection the API makes (graph.SelectFiles). On a laptop, where
// agk run --local binds the working tree whole, that is every long form with its mode. On a
// server it is a redemption that placed nothing: a task granted before its grant named its
// step's files, or a version recorded before they selected anything, each handed its tree
// whole, and each placed as it was then, what a long form relocates and nothing else, with the
// mode the tree gives it. A selector no tree could answer is the workflow's, and refused before
// anything is placed.
func repoBinds(t graph.Task, repo string, w *workdir, placed []Placed, server bool) ([]docker.Mount, []placedCopy, error) {
	if repo == "" {
		// A task with no repository tree behind it mounts nothing, which is a brick
		// test whose case carries no repo/ directory and any caller that has no tree
		// to give. Binding an empty path would create a directory on the host and
		// call it the workflow.
		return nil, nil, nil
	}
	if err := graph.CheckFiles(t.Files); err != nil {
		return nil, nil, fault(t.Step, nil, ChargeBrick, "%v", err)
	}
	mounts := []docker.Mount{bind(repo, RepoDir, true)}
	if placed == nil {
		var err error
		if placed, err = placeFromTree(t, repo, server); err != nil {
			return nil, nil, err
		}
	}
	if len(placed) == 0 {
		return mounts, nil, nil
	}
	binds, copies, err := placeFiles(t, w, placed)
	if err != nil {
		return nil, nil, err
	}
	return append(mounts, binds...), copies, nil
}

// placeFromTree is what the step's files place, read against the tree at repo: every file under
// each placing selector's base, the directory a walk of a large working tree starts from, and
// none of git's own, which a commit never holds. asBefore places what a runner placed before a
// step's files selected anything: a relocation, with the tree's mode.
//
// The tree is read through an os.Root, so that nothing it reaches is outside it. A working tree
// is somebody's own directory and may hold a link to their home: a selector that walked through
// one would copy their keys into a container.
func placeFromTree(t graph.Task, repo string, asBefore bool) ([]Placed, error) {
	var placing []graph.FileSelector
	for _, f := range t.Files {
		switch {
		case f.To != "" && asBefore:
			placing = append(placing, graph.FileSelector{From: f.From, To: f.To})
		case (f.To != "" || f.Mode != "") && !asBefore:
			placing = append(placing, f)
		}
	}
	if len(placing) == 0 {
		return nil, nil
	}
	tree, err := os.OpenRoot(repo)
	if err != nil {
		return nil, fault(t.Step, err, ChargePlatform, "files: the tree at %s could not be opened: %v", repo, err)
	}
	defer tree.Close()
	seen := map[string]bool{}
	var paths []string
	for _, f := range placing {
		base, err := f.Base()
		if err != nil {
			// A mode given to a path that leaves the tree, which selects nothing.
			continue
		}
		err = fs.WalkDir(tree.FS(), base, func(at string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && at == base && errors.Is(err, fs.ErrNotExist):
				// Nothing there, which selects nothing.
				return fs.SkipAll
			case err != nil:
				return err
			case d.IsDir() && d.Name() == ".git":
				return fs.SkipDir
			case !d.Type().IsRegular():
				// A link is resolved on whichever host lays the tree out and can point
				// outside it, which is why a push refuses one: it is left where it is.
				return nil
			}
			if !seen[at] {
				seen[at] = true
				paths = append(paths, at)
			}
			return nil
		})
		if err != nil {
			return nil, fault(t.Step, err, ChargeBrick, "files: the tree under %s could not be read: %v, and a path that passes through a link out of the tree is refused here as a push refuses the link", base, err)
		}
	}
	selected := graph.SelectFiles(placing, paths)
	out := make([]Placed, 0, len(selected.Placed))
	for _, p := range selected.Placed {
		out = append(out, Placed{Source: filepath.Join(repo, filepath.FromSlash(p.Path)), To: p.To, Mode: p.Mode})
	}
	return out, nil
}

// placedDir is where, under a task's working directory, the files its step's files place are
// laid out, each at the path the container finds it.
const placedDir = "placed"

// placedDirMode is what a directory of the placed files is, for the reason a tree's are 0755: a
// remapped container reaches them as an account they are given to, and an image's own account
// may be another.
const placedDirMode = 0o755

// placeFiles copies every placed file into the task's working directory, at the path the
// container finds it, with its mode, and answers the binds that put them there.
//
// A copy, rather than a bind of the tree's own file, for the two things a placed file is given
// that the tree's is not: its mode, and the owner the working directory is given with it, the
// base of a remapped range, so that a 0600 key is readable by the container's root and by nobody
// else. The tree's files are links to a cache every task of the namespace shares, and either set
// on one of them would be set on all. The copies go with the working directory, and so with the
// container.
//
// Each directory a selector relocates into is bound once, with everything placed below it, and a
// file placed anywhere else, which is what a mode with no to places over the tree, is bound on
// its own. Two different files placed on one path are refused, since one of them would go unread,
// and so is a file placed where another is placed below it.
func placeFiles(t graph.Task, w *workdir, placed []Placed) ([]docker.Mount, []placedCopy, error) {
	dir := filepath.Join(w.Root, placedDir)
	type from struct {
		source string
		mode   os.FileMode
	}
	put := map[string]from{}
	var copies []placedCopy
	sorted := slices.Clone(placed)
	slices.SortFunc(sorted, func(a, b Placed) int {
		return cmp.Or(strings.Compare(a.To, b.To), strings.Compare(a.Source, b.Source), strings.Compare(a.Mode, b.Mode))
	})
	for _, p := range sorted {
		to := path.Clean(p.To)
		if !path.IsAbs(p.To) || to == "/" || strings.ContainsRune(to, 0) {
			return nil, nil, fault(t.Step, nil, ChargePlatform, "a file of the tree is placed at %q, which is not a path of the container a file can be placed at", p.To)
		}
		mode, err := placedMode(p)
		if err != nil {
			return nil, nil, fault(t.Step, err, ChargePlatform, "files: the file placed at %s: %v", to, err)
		}
		if had, ok := put[to]; ok {
			// The same bytes with the same mode are one file, however many selectors
			// place it, as they are on a server, where one digest is one source.
			if had.mode == mode && (had.source == p.Source || sameBytes(had.source, p.Source)) {
				continue
			}
			return nil, nil, fault(t.Step, nil, ChargeBrick, "files: two files are placed at %s, and one path holds one of them: a relocation keeps each file's path below the directory it names, so two selectors placing different files there leave one unread", to)
		}
		name, err := placeOne(dir, to, p.Source, mode)
		if err != nil {
			if errors.Is(err, errPlacedOver) {
				return nil, nil, fault(t.Step, nil, ChargeBrick, "files: %s is placed as a file, and another file is placed below it as if it were a directory", to)
			}
			return nil, nil, fault(t.Step, err, ChargePlatform, "files: the file placed at %s: %v", to, err)
		}
		put[to] = from{p.Source, mode}
		copies = append(copies, placedCopy{Path: name, To: to, Mode: mode})
	}

	// Each file under the shallowest directory a selector relocates into, or on its own.
	roots := map[string]bool{}
	for _, f := range t.Files {
		if f.To != "" {
			roots[path.Clean(f.To)] = true
		}
	}
	used := map[string]bool{}
	for to := range put {
		root := to
		for at := to; at != "/"; at = path.Dir(at) {
			if roots[at] {
				root = at
			}
		}
		used[root] = true
	}
	var targets []string
	alone := 0
	for root := range used {
		nested := false
		for at := path.Dir(root); at != "/"; at = path.Dir(at) {
			if used[at] {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		targets = append(targets, root)
		if !roots[root] {
			alone++
		}
	}
	if alone > placedAloneMax {
		return nil, nil, fault(t.Step, nil, ChargeBrick, "files: %d files are each bound on their own, where a mode with no to leaves them under %s, and a container is given at most %d: relocate them into a directory with to, which is one bind however many files it holds", alone, RepoDir, placedAloneMax)
	}
	slices.Sort(targets)
	mounts := make([]docker.Mount, 0, len(targets))
	for _, target := range targets {
		mounts = append(mounts, bind(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(target, "/"))), target, true))
	}
	return mounts, copies, nil
}

// placedAloneMax is how many placed files a container is given each on a bind of its own, which
// is what a mode with no to places, over the tree. Each is a mount the daemon makes when it
// creates the container, and what that costs grows faster than their number: Docker Desktop took
// 4 seconds to create a container with 256 of them, 44 with 1,024, and had not with 4,096 after
// five minutes. A directory relocated with to is one bind however many files it holds, which is
// what a step placing more wants.
const placedAloneMax = 256

// ownPlaced gives each placed file whose mode gives its owner something it gives nobody else to
// the account the container runs as, through the remapped range where there is one.
//
// The rest of what a task is given is read by everybody, since the account a container runs as is
// often a name this driver never resolves. A mode such as 0600 is asked for precisely so that
// nobody else reads the file, "a key that must not be" world readable, and a key nobody can read
// is no key: owned by the range's base, or by the runner's own account on a daemon that does not
// remap, it is readable by no brick, since a brick never runs as root and every capability,
// CAP_DAC_OVERRIDE among them, is dropped. So it is given to the one account that should read it.
// That account is known where it is written as a number, spec.runtime.user in a manifest or the
// image's own user, or where none is written, which is root; a name is refused for such a mode,
// with what to write instead, rather than handing over a file nobody can open.
//
// A process that may not give a file away, agk run --local by a person who is not root, says so
// and goes on: Docker Desktop's file sharing reads a file whatever its owner, and a runner holds
// CAP_CHOWN, which giving the working directory to a remapped range asks of it already.
func (d *Docker) ownPlaced(t graph.Task, floor *usernsFloor, user string, copies []placedCopy) error {
	for _, c := range copies {
		owner, others := (c.Mode>>6)&0o7, c.Mode&0o7
		if owner&^others == 0 {
			continue
		}
		uid, gid, known := accountOf(user)
		if !known {
			return fault(t.Step, nil, ChargeBrick, "files: %s is placed with the mode %04o, which gives its owner what it gives nobody else, and the container runs as %q, which is not an account number this runner can give the file to: write the account as a number, as spec.runtime.user may, or give the file a mode its account reads as anybody does", c.To, c.Mode, user)
		}
		if base, baseGID, remapped := floor.ownership(); remapped {
			uid, gid = base+uid, baseGID+gid
		}
		if err := os.Lchown(c.Path, uid, gid); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				d.say(fmt.Sprintf("files: the file placed at %s for step %s keeps this process's owner, since it may not give it to uid %d, the account the container runs as: a daemon that honours owners, as Docker Engine does and Docker Desktop's file sharing does not, lets nobody but that owner read a file with the mode %04o", c.To, t.Step, uid, c.Mode))
				return nil
			}
			return fault(t.Step, err, ChargePlatform, "files: the file placed at %s could not be given to uid %d: %v", c.To, uid, err)
		}
	}
	return nil
}

// accountOf reads the account a container runs as, written as Docker takes it: nothing, which is
// root, or a uid with a gid after a colon, the gid 0 where none is written. A name, of the account
// or of its group, is not read, since only the image's own account database says what it is.
func accountOf(user string) (uid, gid int, known bool) {
	if user == "" {
		return 0, 0, true
	}
	u, g, grouped := strings.Cut(user, ":")
	uid, err := strconv.Atoi(u)
	if err != nil || uid < 0 {
		return 0, 0, false
	}
	if !grouped {
		return uid, 0, true
	}
	gid, err = strconv.Atoi(g)
	if err != nil || gid < 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// placedCopy is one copy placeFiles made: where it is on the host, where the container finds it,
// and its mode.
type placedCopy struct {
	Path string
	To   string
	Mode os.FileMode
}

// sameBytes says whether two files hold the same bytes, and says no where either cannot be read.
func sameBytes(a, b string) bool {
	x, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	y, err := os.ReadFile(b)
	return err == nil && bytes.Equal(x, y)
}

// errPlacedOver is a file placed where a directory of another placed file is, or below a file
// placed before it.
var errPlacedOver = errors.New("driver: a placed file and a placed directory share a path")

// placedMode is the mode a placed file takes: the one it was placed with, or, where none was
// given, the one it has on the host.
func placedMode(p Placed) (os.FileMode, error) {
	if p.Mode == "" {
		info, err := os.Stat(p.Source)
		if err != nil {
			return 0, err
		}
		return info.Mode().Perm(), nil
	}
	if !placedModePattern.MatchString(p.Mode) {
		return 0, fmt.Errorf("the mode %q is not three octal digits", p.Mode)
	}
	m, err := strconv.ParseUint(p.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("the mode %q does not read as octal", p.Mode)
	}
	return os.FileMode(m), nil
}

// placedModePattern is the grammar of a mode, the workflow's and the wire's.
var placedModePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// placeOne copies one file to the path to names under dir, creating each directory between the two
// with placedDirMode, gives it its mode once its bytes are there, and answers where it is.
func placeOne(dir, to, source string, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, placedDirMode); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, placedDirMode); err != nil {
		return "", err
	}
	at := dir
	for _, segment := range strings.Split(strings.TrimPrefix(path.Dir(to), "/"), "/") {
		if segment == "" {
			continue
		}
		at = filepath.Join(at, segment)
		err := os.Mkdir(at, placedDirMode)
		if errors.Is(err, fs.ErrExist) {
			if info, serr := os.Lstat(at); serr != nil || !info.IsDir() {
				return "", errPlacedOver
			}
			continue
		}
		if err == nil {
			err = os.Chmod(at, placedDirMode)
		}
		if err != nil {
			return "", err
		}
	}
	src, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer src.Close()
	name := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(to, "/")))
	dst, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", errPlacedOver
	}
	if err != nil {
		return "", err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return name, os.Chmod(name, mode)
}

// redeemSecrets asks for the values and names the file each is written in.
//
// The value is asked of the task's secret source here, Sources.Secrets where the runner
// gave one and Config.Secrets otherwise, and never travels on the task message: what a
// Task carries is "names and mount points and never values". A server runner answers
// from the task's redemption, which it made before the pull, since it redeems before it
// acknowledges the task message, and agk run --local from the command line. Each one
// becomes a file of its own on the task's secrets volume, mounted read-only at
// /agk/secrets, so a brick opens a path and the value is never in an environment
// "readable by its children" and in "diagnostic dumps".
//
// The volume is a tmpfs, which is what the Tmpfs row of the settings table asks of a secret
// mount point, and it is the task's alone: a tmpfs mount of the container's own is empty when
// its first instruction runs, and a tmpfs volume is one a container can be given already
// filled. It is mounted noexec,nosuid,nodev, the flags the row names.
func redeemSecrets(ctx context.Context, t graph.Task, secrets Secrets) ([]secretFile, [][]byte, error) {
	if len(t.Secrets) == 0 {
		return nil, nil, nil
	}
	if secrets == nil {
		return nil, nil, fault(t.Step, nil, ChargePlatform, "%d secrets to redeem and no secret source: a runner redeems at the API the per-task grant the controller issued for that one task and that one secret, and agk run --local reads the command line", len(t.Secrets))
	}

	// Sorted by name, so that two runs of one step fill the volume in the same order,
	// which is what makes what they were given legible when it is read twice.
	wanted := slices.Clone(t.Secrets)
	slices.SortFunc(wanted, func(a, b graph.SecretMount) int { return strings.Compare(a.Name, b.Name) })

	var files []secretFile
	var values [][]byte
	seen := map[string]bool{}
	for _, s := range wanted {
		target := secretTarget(s)
		if !secretMountPattern.MatchString(target) {
			return nil, nil, fault(t.Step, ErrContractBroken, ChargeBrick, "secret %s is mounted at %q: a secret is mounted on tmpfs at /agk/secrets/<name>, %s, and a mount elsewhere, /run/secrets/bearer out of habit, is refused", s.Name, target, secretMountRule)
		}
		if seen[target] {
			return nil, nil, fault(t.Step, ErrContractBroken, ChargeBrick, "secret %s: two secrets are mounted at %q, and one path cannot hold both", s.Name, target)
		}
		seen[target] = true

		value, err := secrets.Value(ctx, s.Name)
		if err != nil {
			return nil, nil, fault(t.Step, err, ChargePlatform, "secret %s has no value to give the task: a runner has it from the redemption of the per-task grant, made before the image was pulled, and agk run --local from the command line", s.Name)
		}
		files = append(files, secretFile{Name: path.Base(target), Value: value})
		values = append(values, value)
	}
	return files, values, nil
}

// secretTarget is where in the container a secret is mounted: where the manifest said, or
// /agk/secrets/<name> where it said nothing.
func secretTarget(s graph.SecretMount) string {
	if s.Mount != "" {
		return s.Mount
	}
	return SecretsDir + "/" + s.Name
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
