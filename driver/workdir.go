package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The modes the host side of the contract is laid down with.
//
// A task's directory is private to the runner account and is traversed by nothing else,
// which is what makes the one permissive mode below safe: the writable leaf is reachable
// only through parents nobody but the runner can enter, and it is removed with the
// container.
//
// The out tree is the exception, and it is permissive on purpose. A container runs as
// the account its image declares, "65532:65532" or a name this driver never resolves, so
// the only directory mode that lets every image write its outputs is one that does not
// name an account. The bind is what the container reaches it through, so the mode of the
// directory itself is the whole of the access decision and the parents are not in it.
const (
	workdirMode = 0o700
	outMode     = 0o777
	// A secret value is read by the container's account, which is not the account
	// the file was written by, so it is readable and never writable. It sits under
	// the same private parents, and where the platform has a tmpfs it never reaches
	// a disk at all.
	secretMode = 0o444
)

// workdir is one task's working directory on the host: what the mounts are bound from,
// "created fresh, owned by an unprivileged account, and removed with the container, so
// no residue of one namespace survives into the next task on that host".
//
// Secrets sits apart from the rest because it is a different filesystem where the
// platform has one. Everything else is one tree, so removing the task is removing a
// directory.
type workdir struct {
	Root    string
	In      string
	Out     string
	Run     string
	Params  string
	Secrets string

	// secretsOwn says whether Secrets is a directory of this task's own that has to
	// be removed separately, which it is exactly when it is not inside Root.
	secretsOwn bool
}

// newWorkdir creates the directory of one task, fresh.
//
// secretsDir is Policy.SecretsDir, the filesystem a secret value may be written on
// without touching a disk. Empty puts the values under the task's own directory, which is
// the platform that has no tmpfs and the case the driver announces.
//
// The path is the task's identity spelled as directories, run/step/attempt and the shard
// where there is one, rather than the identifier with its separators replaced. The
// identifier is parsed first, so a task whose identity could not have been composed is
// refused before anything is created, and two tasks can no more share a directory than
// they can share an identity.
//
// Fresh means fresh. A directory left behind by a process that died between creating it
// and creating the container is removed rather than reused, because a half prepared
// input is worse than no input: the brick would read an envelope from the attempt before
// this one and never know.
func newWorkdir(root string, id agk.TaskID, secretsDir string) (*workdir, error) {
	w, err := workdirFor(root, id, secretsDir)
	if err != nil {
		return nil, err
	}

	// The one directory this runner owns under Policy.SecretsDir is claimed before
	// anything is removed or created beneath it, because everything beneath it is
	// reached through it and a link left under that name would be followed.
	if w.secretsOwn {
		base := filepath.Join(secretsDir, secretsBase)
		if err := ownedDir(base); err != nil {
			return nil, fmt.Errorf("driver: task %s: %s is where this runner writes secret values, and %s is shared with everything else on this host: %w", id, base, secretsDir, err)
		}
	}

	if err := os.RemoveAll(w.Root); err != nil {
		return nil, fmt.Errorf("driver: task %s: working directory %s: %w", id, w.Root, err)
	}
	if w.secretsOwn {
		if err := os.RemoveAll(w.Secrets); err != nil {
			return nil, fmt.Errorf("driver: task %s: secrets directory %s: %w", id, w.Secrets, err)
		}
	}

	// A directory that could not be prepared is taken away again, and what could not
	// be taken away is part of the refusal rather than dropped.
	abandon := func(dir string, err error) (*workdir, error) {
		err = fmt.Errorf("driver: task %s: working directory %s: %w", id, dir, err)
		if left := w.remove(); left != nil {
			err = fmt.Errorf("%w, and what was prepared was not all taken away: %v", err, left)
		}
		return nil, err
	}

	// The parents are created with the work root's own mode, private to the runner,
	// so that the one permissive directory below sits behind them.
	for _, dir := range []string{w.Root, w.In, w.Secrets} {
		if err := os.MkdirAll(dir, workdirMode); err != nil {
			return abandon(dir, err)
		}
	}
	// The two directories the contract names under /agk/out exist before the
	// container does. A brick writes /agk/out/ports/<port>.json, and one that does
	// not create the directory first is honouring the contract as it is written.
	for _, dir := range []string{w.Out, filepath.Join(w.Out, "ports"), filepath.Join(w.Out, "files")} {
		if err := os.MkdirAll(dir, outMode); err != nil {
			return abandon(dir, err)
		}
		// MkdirAll applies the process umask, which on a runner is usually 022 and
		// would take the group and other bits straight back off. The mode is the
		// access decision here, so it is set rather than requested.
		if err := os.Chmod(dir, outMode); err != nil {
			return abandon(dir, err)
		}
	}
	return w, nil
}

// freshWorkdir is newWorkdir under the record's lock, which is the lock sweep takes empty
// run, step and attempt directories away under: the parents a task's directory is created
// in are the parents a sweep may be removing, and sweep says what goes wrong between the two.
func (d *Docker) freshWorkdir(id agk.TaskID) (*workdir, error) {
	d.keys.mu.Lock()
	defer d.keys.mu.Unlock()
	return newWorkdir(d.cfg.WorkRoot, id, d.cfg.Policy.SecretsDir)
}

// workdirFor names the directory of one task without creating or removing anything.
//
// It is the half of newWorkdir that is arithmetic on a path, and it is separate because
// the delivery that adopts a container did not prepare the directory and must still take
// it away: "the working directory of a task is created fresh, owned by an unprivileged
// account, and removed with the container, so no residue of one namespace survives into
// the next task on that host". The path is derived from the task identifier and never
// minted, so the directory a second delivery names is the directory the first prepared.
func workdirFor(root string, id agk.TaskID, secretsDir string) (*workdir, error) {
	if root == "" {
		return nil, fmt.Errorf("driver: no work root: a task's working directory is created under one")
	}
	rel, err := taskPath(id)
	if err != nil {
		return nil, err
	}

	w := &workdir{Root: filepath.Join(root, rel)}
	w.In = filepath.Join(w.Root, "in")
	w.Out = filepath.Join(w.Root, "out")
	w.Run = filepath.Join(w.Root, "run.json")
	w.Params = filepath.Join(w.Root, "params.json")
	if secretsDir != "" {
		w.Secrets = filepath.Join(secretsDir, secretsBase, rel)
		w.secretsOwn = true
	} else {
		w.Secrets = filepath.Join(w.Root, "secrets")
	}
	return w, nil
}

// secretsBase is the one directory this runner owns under Policy.SecretsDir. Every task's
// values live under it, so it is the single place the ownership of that tree is decided.
const secretsBase = "agentiik"

// ownedDir makes one directory this process alone may write to, under a path that is
// shared with everything else on the machine.
//
// Policy.SecretsDir is /dev/shm on Linux, and /dev/shm is mode 1777: anything on the host
// can get there first. A link left under that name would be followed and a secret value
// written through it, and a directory left group or world writable would leave every
// value beneath it reachable, since the value itself is readable by design and the mode
// of its parents is the whole of what protects it. So the directory is created rather
// than assumed, and one that is already there is checked rather than trusted.
func ownedDir(path string) error {
	err := os.Mkdir(path, workdirMode)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	// Lstat and not Stat: a link left under this name is the thing being refused, and
	// Stat would report whatever it points at rather than the link itself.
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is %s and not a directory of this runner's own", path, modeName(info.Mode()))
	}
	// The owner is read rather than left to the chmod below to refuse. A runner on a
	// remapped daemon holds CAP_FOWNER, and a chmod by a process holding it succeeds on
	// a directory anybody owns, so a directory made first by another account on the
	// host would be closed and kept, with that account still its owner and free to open
	// it again once values are written beneath it.
	if uid, ok := ownerOf(info); ok && uid != os.Geteuid() {
		return fmt.Errorf("%s belongs to uid %d, and this runner is uid %d", path, uid, os.Geteuid())
	}
	// A chmod closes a directory this runner left open, and fails outright where this
	// process is not the account that owns it and holds nothing that lets it, which is
	// the same refusal by another route on a platform whose owner is not read above.
	return os.Chmod(path, workdirMode)
}

// modeName says in one word what something that is not a directory is, so that a refusal
// names what it found.
func modeName(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m.IsRegular():
		return "a file"
	default:
		return "a " + m.Type().String()
	}
}

// taskPath spells a task identifier as a relative path.
//
// Every segment is one the identifier was composed from and every one of them is
// validated by agk before it gets here: a run identifier carries no separator, a step
// name is an identifier and an attempt is digits. The shard is written index-of on one
// segment rather than on two, so that a directory listing shows shards of one attempt
// beside each other.
func taskPath(id agk.TaskID) (string, error) {
	run, step, attempt, shard, err := agk.ParseTaskID(string(id))
	if err != nil {
		return "", fmt.Errorf("driver: %w", err)
	}
	parts := []string{string(run), string(step), strconv.Itoa(attempt)}
	if !shard.IsZero() {
		parts = append(parts, strconv.Itoa(shard.Index)+"-"+strconv.Itoa(shard.Of))
	}
	return filepath.Join(parts...), nil
}

// own gives the tree to the account a remapped container runs as.
//
// Remapping "introduces some configuration complexity in situations where the container
// needs access to resources on the Docker host, such as bind mounts", and every brick
// receives bind mounts under /agk. The uid and gid are the pair the daemon's own root
// directory ends in, which is the base of the remapped range, so the files a task is
// given belong to the same range the container's processes live in.
//
// A chown this process may not do refuses the task and names what it tried, because the
// alternative is a container that starts and then silently cannot write its outputs.
// Chowning to a uid that is not your own is a privileged operation, so a runner that
// finds a remapped daemon is a runner that has to be able to do it.
func (w *workdir) own(uid, gid int) error {
	for _, root := range w.trees() {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Lchown and not Chown: a symlink in the tree is followed by the
			// second, which would take the chown outside the directory it is
			// meant for.
			return os.Lchown(path, uid, gid)
		})
		if err != nil {
			return fmt.Errorf("driver: the working directory %s could not be given to uid %d and gid %d, which is the base of this daemon's remapped range: %w. A runner on a daemon with userns-remap prepares each task's directory inside that range, and the range itself is the one /etc/subuid gives the daemon's account. A runner that is not root does it with CAP_CHOWN, which its unit grants with AmbientCapabilities", root, uid, gid, err)
		}
	}
	return nil
}

// trees are the directories that hold the whole of one task on the host: its working
// directory, and its secrets directory where that is not inside the first.
func (w *workdir) trees() []string {
	if w.secretsOwn {
		return []string{w.Root, w.Secrets}
	}
	return []string{w.Root}
}

// remove takes the task's directory away, which is what "removed with the container"
// means on this side, and answers with what it could not take away. A directory that is
// already gone is the outcome asked for and no error.
//
// It is called on every path out of a task, and what it answers never changes what became
// of the task: the container ran or it did not, whatever is left here. What is left is
// still the one thing the directory exists not to leave, "residue of one namespace" that
// survives "into the next task on that host", secret values among it, so a caller says so
// rather than dropping it. A brick creates files under /agk/out as an account of its own,
// in directories it may make unreadable, and a runner that cannot remove them is the case
// this answers for. Every tree is attempted whatever became of the one before it, so that a
// working directory that could not be removed does not keep the secret values with it.
func (w *workdir) remove() error {
	if w == nil {
		return nil
	}
	var left []error
	for _, tree := range w.trees() {
		// RemoveAll names the path it stopped at, which is what a person goes and
		// looks at.
		if err := os.RemoveAll(tree); err != nil {
			left = append(left, err)
		}
	}
	return errors.Join(left...)
}

// emptyKept is how long a run, step or attempt directory a task left empty stays before a
// sweep takes it away.
//
// A task's own directory goes with its container, and the directories above it are shared
// with every other task of its run, its step and its attempt, so they are not the task's to
// take. What removes them is a sweep, hourly beside the record's prune, of the ones that have
// held nothing for this long. Not at once, when a task ends and leaves its parents empty,
// because the next shard or the next attempt of the same step names the same parents a
// moment later: on Docker Desktop, whose file sharing is where agk run --local binds from, a
// directory removed and created again at one path is refused as a bind source ("error while
// creating mount source path ... no such file or directory") or served as it was before,
// for about a second after, and a fan-out whose shards run one at a time would have every
// shard after the first fail on it. An hour is far longer than that and far shorter than
// anything a host is worse for: what is left is one directory per step run in the last two.
const emptyKept = time.Hour

// sweep takes away the run, step and attempt directories on the work root, and on the secrets
// directory where it is apart from it, that tasks left empty before cutoff.
//
// It is called with the record's lock held, which is the lock a task's working directory is
// created under, and that is what makes it safe. MkdirAll finds a parent there and then
// creates the directory below it, two calls with nothing between them, and a parent removed
// in between refuses the second, which would refuse the task on the account of a runner
// that was only tidying up. Under the one lock, a parent is either removed before a sibling
// looks for it, and created again, or found and filled before anything tries to remove it.
//
// Only what taskPath could have spelled is looked at, a run, then a step, an attempt and a
// shard, and never anything below a task's own directory: the work root holds the record
// and the trees beside the tasks, and a work root is a directory somebody chose. A run is
// one the record under the work root has a directory for, since a name that is merely
// valid as a run identifier is nearly any name: an empty lost+found at the root of a
// filesystem mounted for the work root is one, and the runner may remove it. A runner writes
// every key there, as taken, before its task's directory is created; agk run --local holds
// nothing and writes a key as it ends, and clears what is left when its session closes. The
// record's prune keeps a run's directory for as long as the run is on the work root.
// A directory is taken away with os.Remove, which refuses one that holds anything, so a
// task's directory that could not be removed stays to be found. It is judged by the moment
// it last changed as the walk found it, before a child the same sweep took away changed it
// again, so that a run whose last step went empty an hour ago goes in the same sweep as
// the step.
//
// A secrets directory stays while the task directory of the same name is on the work root.
// Every task has one, and it is empty for a task given no secret, so emptiness alone does
// not tell a parent from a task that is still running; the working directory, which is
// never empty while its task runs, does.
//
// The secrets directory is swept only while it is still this runner's alone, as ownedDir
// leaves it. It sits on a filesystem every account on the host can write to, and one that
// somebody else made or opened could have a link swapped in under a path the walk found,
// between the walk and the removal, which would take away an empty directory of that
// name wherever the link points. A task is refused its directory there by ownedDir, and
// the sweep leaves it alone for the same reason.
func sweep(root, secretsDir string, cutoff time.Time) {
	if root == "" {
		return
	}
	sweepTree(root, root, cutoff, nil)
	base := filepath.Join(secretsDir, secretsBase)
	if secretsDir != "" && stillOwned(base) {
		sweepTree(root, base, cutoff, func(rel string) bool {
			_, err := os.Lstat(filepath.Join(root, rel))
			return !errors.Is(err, fs.ErrNotExist)
		})
	}
}

// stillOwned says whether a directory is as ownedDir leaves it: a directory and not a link,
// belonging to this process's account where the platform says, and closed to every other.
func stillOwned(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	uid, ok := ownerOf(info)
	return !ok || uid == os.Geteuid()
}

// sweepTree is sweep over the tree under top, whose runs are the ones the record under root
// has. held says a directory is to stay whatever it holds.
func sweepTree(root, top string, cutoff time.Time, held func(rel string) bool) {
	type empty struct{ path, rel string }
	var found []empty
	filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if path == top {
			return err
		}
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(top, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if !taskShaped(parts) || len(parts) == 1 && !recorded(root, parts[0]) {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			found = append(found, empty{path, rel})
		}
		if len(parts) == 4 {
			// A shard's own directory: what is below it is the task's.
			return filepath.SkipDir
		}
		return nil
	})
	// Deepest first, so that a step goes after the attempts that emptied it.
	for i := len(found) - 1; i >= 0; i-- {
		if held != nil && held(found[i].rel) {
			continue
		}
		os.Remove(found[i].path)
	}
}

// recorded says whether the record under the work root has a directory for a run.
func recorded(root, run string) bool {
	info, err := os.Lstat(filepath.Join(root, KeysDir, run))
	return err == nil && info.IsDir()
}

// taskShaped says whether a path relative to a tree is one taskPath spells or a parent of
// one: a run, then a step, an attempt and a shard written index-of.
func taskShaped(parts []string) bool {
	if len(parts) == 0 || len(parts) > 4 || strings.HasPrefix(parts[0], ".") || agk.RunID(parts[0]).Validate() != nil {
		return false
	}
	if len(parts) >= 2 && agk.Step(parts[1]).Validate() != nil {
		return false
	}
	id := strings.Join(parts[:min(len(parts), 3)], "/")
	if len(parts) == 4 {
		index, of, ok := strings.Cut(parts[3], "-")
		if !ok {
			return false
		}
		id += "/" + index + "/" + of
	}
	if len(parts) >= 3 {
		if _, _, _, _, err := agk.ParseTaskID(id); err != nil {
			return false
		}
	}
	return true
}
