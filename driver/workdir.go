package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

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

	// The parents are created with the work root's own mode, private to the runner,
	// so that the one permissive directory below sits behind them.
	for _, dir := range []string{w.Root, w.In, w.Secrets} {
		if err := os.MkdirAll(dir, workdirMode); err != nil {
			w.remove()
			return nil, fmt.Errorf("driver: task %s: working directory %s: %w", id, dir, err)
		}
	}
	// The two directories the contract names under /agk/out exist before the
	// container does. A brick writes /agk/out/ports/<port>.json, and one that does
	// not create the directory first is honouring the contract as it is written.
	for _, dir := range []string{w.Out, filepath.Join(w.Out, "ports"), filepath.Join(w.Out, "files")} {
		if err := os.MkdirAll(dir, outMode); err != nil {
			w.remove()
			return nil, fmt.Errorf("driver: task %s: working directory %s: %w", id, dir, err)
		}
		// MkdirAll applies the process umask, which on a runner is usually 022 and
		// would take the group and other bits straight back off. The mode is the
		// access decision here, so it is set rather than requested.
		if err := os.Chmod(dir, outMode); err != nil {
			w.remove()
			return nil, fmt.Errorf("driver: task %s: working directory %s: %w", id, dir, err)
		}
	}
	return w, nil
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
	// A chmod closes a directory somebody left open, and fails outright where this
	// process is not the account that owns it, which is the same refusal by another
	// route and the one check that needs no platform specific call to make.
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
	roots := []string{w.Root}
	if w.secretsOwn {
		roots = append(roots, w.Secrets)
	}
	for _, root := range roots {
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
			return fmt.Errorf("driver: the working directory %s could not be given to uid %d and gid %d, which is the base of this daemon's remapped range: %w. A runner on a daemon with userns-remap prepares each task's directory inside that range, and the range itself is the one /etc/subuid gives the daemon's account", root, uid, gid, err)
		}
	}
	return nil
}

// remove takes the task's directory away, which is what "removed with the container"
// means on this side. It is called on every path out of a task, so it reports nothing: a
// directory that is already gone is the outcome asked for.
func (w *workdir) remove() {
	if w == nil {
		return
	}
	os.RemoveAll(w.Root)
	if w.secretsOwn {
		os.RemoveAll(w.Secrets)
	}
}
