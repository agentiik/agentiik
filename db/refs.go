package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// A workflow repository, as the database holds it: its refs, and the packs its objects are kept in.
//
// "Objects are packfiles in the object store; refs live in PostgreSQL. ... Refs in the database also
// make a ref update an ordinary transaction beside every other piece of state." So a push moves its
// refs in the transaction that records its versions, and a fetch reads them in the transaction that
// lists the packs holding what they name. The packs themselves are bytes in the object store, which
// package repo/store writes and reads; what is here is which of them a repository holds, and in
// which state.

// MaxRefBytes is the longest ref name held, 1,024 bytes, a bound git itself does not set: a ref is a
// key of an index, which PostgreSQL holds to some 2,700 bytes a row, and git keeps a ref it writes as
// a file under .git, whose path macOS holds to 1,024, so no ref a clone can check out is past it.
const MaxRefBytes = 1024

// Repository is a workflow as git reads it, at one moment.
type Repository struct {
	Namespace string
	Workflow  string

	// Key is what its packs are kept under in the object store, <namespace>/git/<key>/: random, and
	// never changed, so that a rename moves no object.
	Key string

	// DefaultBranch is the branch HEAD names, without refs/heads/.
	DefaultBranch string

	// Refs are its branches and tags, in git's order, byte by byte. The default branch is among them
	// with no commit while it is unborn.
	Refs []Ref

	// Packs are its live packs, newest first: every object any ref names is in one of them.
	Packs []Pack
}

// Ref is one branch or tag.
type Ref struct {
	// Name is the ref in full, refs/heads/main or refs/tags/v2.1.0.
	Name string

	// Commit is the commit it points at, an annotated tag peeled to it, and empty while the branch
	// is unborn. Tag is the annotated tag object an annotated tag's ref names, and empty for a
	// branch and a lightweight tag.
	Commit string
	Tag    string

	// Protected is whether pushing to it takes grant:manage, which only the default branch of a
	// protected repository is.
	Protected bool

	// MovedBy and MovedAt are who last moved it and when, empty while it is unborn.
	MovedBy string
	MovedAt time.Time
}

// Target is what git says the ref names: the tag object where it is an annotated tag, the commit
// otherwise, and empty while it is unborn.
func (r Ref) Target() string {
	if r.Tag != "" {
		return r.Tag
	}
	return r.Commit
}

// RefUpdate is one command of a push: git's old and new values of one ref, each 40 hexadecimal
// digits, and empty where git writes forty zeros.
type RefUpdate struct {
	Ref string

	// Old is what the pusher found the ref naming, empty where it creates it. New is what it names
	// once moved, empty where it deletes it: a commit, or an annotated tag object for a tag.
	Old string
	New string

	// Commit is the commit New peels to, where New is an annotated tag; empty is New itself, which
	// is what a branch and a lightweight tag name.
	Commit string
}

// ErrStaleRef is a ref no longer where the push found it: another push moved it first.
var ErrStaleRef = errors.New("db: a ref is no longer where the push found it")

// ErrDefaultBranch is a push deleting the default branch, which is refused.
var ErrDefaultBranch = errors.New("db: the default branch is not deleted: another branch is named the default first")

// Pack is one packfile of a repository, named by its checksum.
type Pack struct {
	// Name is the pack's checksum, 40 hexadecimal digits: the SHA-1 its trailer and its index hold,
	// and what its keys are made of.
	Name string

	// Size is the pack's length in bytes, and Objects how many objects it holds.
	Size    int64
	Objects int

	CreatedAt time.Time
}

// ErrNoPack is a pack no longer receiving: collected once it had been receiving past the grace, or
// never recorded.
var ErrNoPack = errors.New("db: that pack is not receiving")

// Repository reads a workflow's repository: its key, its default branch, its refs and its live packs.
//
// The refs are read before the packs, which is what makes the two agree without a lock. A pack is
// made live in the transaction that moves refs onto what it holds, and superseded only once a pack
// holding everything it did is live, so packs read after the refs hold every object the refs name.
// Read the other way round, a push committing between the two reads would name a commit no pack
// listed holds.
func (n *NS) Repository(ctx context.Context, workflow string) (Repository, error) {
	r := Repository{Namespace: n.namespace, Workflow: workflow}
	err := n.tx.QueryRow(ctx,
		`select repository, default_branch from workflows where namespace = $1 and name = $2`,
		n.namespace, workflow).Scan(&r.Key, &r.DefaultBranch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("db: the repository of %s could not be read: %w", workflow, err)
	}

	// In C's collation, byte by byte, which is the order git sorts refs in and a client reads an
	// advertisement in, whatever collation the database was created with.
	rows, err := n.tx.Query(ctx,
		`select ref, commit, tag, protected, moved_by, moved_at from workflow_refs
		 where namespace = $1 and workflow = $2 order by ref collate "C"`,
		n.namespace, workflow)
	if err != nil {
		return Repository{}, fmt.Errorf("db: the refs of %s could not be read: %w", workflow, err)
	}
	r.Refs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ref, error) {
		var ref Ref
		var commit, tag, by *string
		var at *time.Time
		if err := row.Scan(&ref.Name, &commit, &tag, &ref.Protected, &by, &at); err != nil {
			return Ref{}, err
		}
		ref.Commit, ref.Tag, ref.MovedBy = deref(commit), deref(tag), deref(by)
		if at != nil {
			ref.MovedAt = *at
		}
		return ref, nil
	})
	if err != nil {
		return Repository{}, fmt.Errorf("db: the refs of %s could not be read: %w", workflow, err)
	}

	rows, err = n.tx.Query(ctx,
		`select name, size, objects, created_at from git_packs
		 where namespace = $1 and repository = $2 and state = 'live'
		 order by created_at desc, name`,
		n.namespace, r.Key)
	if err != nil {
		return Repository{}, fmt.Errorf("db: the packs of %s could not be read: %w", workflow, err)
	}
	r.Packs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Pack, error) {
		var p Pack
		err := row.Scan(&p.Name, &p.Size, &p.Objects, &p.CreatedAt)
		return p, err
	})
	if err != nil {
		return Repository{}, fmt.Errorf("db: the packs of %s could not be read: %w", workflow, err)
	}
	return r, nil
}

// UpdateRefs moves the refs of one push, all of them or none, as by at at, now where at is zero.
//
// Each ref moves by compare and swap on what git says it names, under a lock on the workflow's row
// that serialises every push to the repository and the repack's swap of its packs: an update moves a
// ref still naming Old, a creation takes an unborn branch or a name no ref holds, and a deletion
// removes a ref still naming Old. Any ref no longer where the push found it fails the whole set with
// ErrStaleRef naming it, and nothing of the set is kept, even where the caller's transaction goes on
// and commits: the set is written under a savepoint of its own. That is git's atomic push, which
// every push is, so that a push is accepted or refused whole.
//
// A push deleting the default branch is refused whole with ErrDefaultBranch, as git refuses to delete
// the branch a repository has checked out: HEAD would name nothing, and unborn would no longer mean
// what the wire says it means, a default branch nothing was pushed to yet, which a run naming no ref
// reads to run the latest version sent as a tree. Another branch is named the default first.
//
// The lock is FOR NO KEY UPDATE, which two pushes take in turn and which leaves alone the key share a
// version, a grant or a run takes on the workflow's row as it is written: a push waits for a push,
// and nothing else waits for one.
func (n *NS) UpdateRefs(ctx context.Context, workflow, by string, at time.Time, updates []RefUpdate) error {
	if by == "" {
		return fmt.Errorf("db: the refs of %s moved by nobody", workflow)
	}
	if len(updates) == 0 {
		return fmt.Errorf("db: the refs of %s moved by no command", workflow)
	}
	type move struct {
		ref, old, commit, tag string
		deletes               bool
	}
	moves := make([]move, 0, len(updates))
	seen := map[string]bool{}
	for _, u := range updates {
		m, err := u.check()
		if err != nil {
			return fmt.Errorf("db: the refs of %s: %w", workflow, err)
		}
		if seen[u.Ref] {
			return fmt.Errorf("db: the refs of %s: %s is moved twice by one push", workflow, u.Ref)
		}
		seen[u.Ref] = true
		moves = append(moves, move{ref: u.Ref, old: u.Old, commit: m.commit, tag: m.tag, deletes: u.New == ""})
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	var branch string
	err := n.tx.QueryRow(ctx,
		`select default_branch from workflows where namespace = $1 and name = $2 for no key update`,
		n.namespace, workflow).Scan(&branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return fmt.Errorf("db: the repository of %s could not be locked: %w", workflow, err)
	}

	for _, m := range moves {
		if m.deletes && m.ref == "refs/heads/"+branch {
			return fmt.Errorf("%w: %s of %s", ErrDefaultBranch, m.ref, workflow)
		}
	}

	set, err := n.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: the refs of %s could not be moved: %w", workflow, err)
	}
	for _, m := range moves {
		var moved int64
		var err error
		switch {
		case m.old == "":
			moved, err = exec(ctx, set,
				`update workflow_refs set commit = $4, tag = $5, moved_by = $6, moved_at = $7
				 where namespace = $1 and workflow = $2 and ref = $3 and commit is null`,
				n.namespace, workflow, m.ref, m.commit, nilIfEmpty(m.tag), by, at)
			if err == nil && moved == 0 {
				moved, err = exec(ctx, set,
					`insert into workflow_refs (namespace, workflow, ref, commit, tag, moved_by, moved_at)
					 values ($1, $2, $3, $4, $5, $6, $7)
					 on conflict (namespace, workflow, ref) do nothing`,
					n.namespace, workflow, m.ref, m.commit, nilIfEmpty(m.tag), by, at)
			}
		case m.deletes:
			moved, err = exec(ctx, set,
				`delete from workflow_refs
				 where namespace = $1 and workflow = $2 and ref = $3 and coalesce(tag, commit) = $4`,
				n.namespace, workflow, m.ref, m.old)
		default:
			moved, err = exec(ctx, set,
				`update workflow_refs set commit = $4, tag = $5, moved_by = $6, moved_at = $7
				 where namespace = $1 and workflow = $2 and ref = $3 and coalesce(tag, commit) = $8`,
				n.namespace, workflow, m.ref, m.commit, nilIfEmpty(m.tag), by, at, m.old)
		}
		if err == nil && moved == 0 {
			err = fmt.Errorf("%w: %s of %s", ErrStaleRef, m.ref, workflow)
		} else if err != nil {
			err = fmt.Errorf("db: %s of %s could not be moved: %w", m.ref, workflow, err)
		}
		if err != nil {
			// Rolled back on a context of its own, as a transaction's is: the usual reason a
			// statement failed is that ctx ended, and a savepoint left open would leave the
			// commands before it to the caller's commit.
			rollback, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackWithin)
			rerr := set.Rollback(rollback)
			stop()
			if rerr != nil {
				return errors.Join(err, fmt.Errorf("db: the refs of %s moved before it could not be put back: %w", workflow, rerr))
			}
			return err
		}
	}
	if err := set.Commit(ctx); err != nil {
		return fmt.Errorf("db: the refs of %s could not be moved: %w", workflow, err)
	}
	return nil
}

// checked is what a command writes: the commit, and the annotated tag where there is one.
type checked struct{ commit, tag string }

// check refuses a command git would not send, or one the table could not hold.
func (u RefUpdate) check() (checked, error) {
	if err := checkRef(u.Ref); err != nil {
		return checked{}, err
	}
	for _, id := range []string{u.Old, u.New, u.Commit} {
		if id != "" && !objectID.MatchString(id) {
			return checked{}, fmt.Errorf("%s: %q is not an object ID, which is 40 lowercase hexadecimal digits", u.Ref, id)
		}
	}
	switch {
	case u.Old == "" && u.New == "":
		return checked{}, fmt.Errorf("%s is neither created, moved nor deleted", u.Ref)
	case u.New == "" && u.Commit != "":
		return checked{}, fmt.Errorf("%s is deleted and names commit %s", u.Ref, u.Commit)
	case u.New == "":
		return checked{}, nil
	case u.Commit == "" || u.Commit == u.New:
		return checked{commit: u.New}, nil
	case !strings.HasPrefix(u.Ref, "refs/tags/"):
		return checked{}, fmt.Errorf("%s names %s peeled to %s, and only a tag names an annotated tag", u.Ref, u.New, u.Commit)
	}
	return checked{commit: u.Commit, tag: u.New}, nil
}

// objectID is a git object's ID as the table holds one: SHA-1, whole, lower case.
var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// refAllowed is what a ref may be made of, and refForbidden what it may not hold, as the wire's
// refName writes them and git check-ref-format decides: the two halves of workflow_refs' check,
// which git_ref_name holds in SQL.
var (
	refAllowed   = regexp.MustCompile(`^refs/(heads|tags)/[^\x00-\x20\x7f~^:?*\[\\]+$`)
	refForbidden = regexp.MustCompile(`/[./]|\.\.|@\{|[/.]$|\.lock(/|$)`)
)

// CheckRef refuses a ref a repository cannot hold: anything but a branch or a tag, a name git
// refuses, and a name past MaxRefBytes, as the table refuses it, so that a push is refused before
// anything is written rather than by the table in the middle of it.
func CheckRef(ref string) error { return checkRef(ref) }

// checkRef refuses a ref the table would: anything but a branch or a tag, a name git refuses, and a
// name past MaxRefBytes.
func checkRef(ref string) error {
	switch {
	case len(ref) > MaxRefBytes:
		return fmt.Errorf("a ref of %d bytes, past the %d a ref is held to", len(ref), MaxRefBytes)
	case !utf8.ValidString(ref) || !refAllowed.MatchString(ref) || refForbidden.MatchString(ref):
		return fmt.Errorf("%q is not a branch or a tag git could name: a ref is refs/heads/<branch> or refs/tags/<tag>, on git check-ref-format's rules", ref)
	}
	return nil
}

// ReceivePack records a pack a push is about to write into the repository of workflow, and answers
// the repository's key, which the pack's keys are made of.
//
// It is recorded receiving before a byte of it is written, in a transaction of its own, so that a
// file under git/ always has a row naming it: one a push died writing is collected with its row once
// it has been receiving past the grace, and one a push is writing is never taken for anything else.
// A pack of the same name is the same bytes, since its name is their checksum: one receiving is
// receiving again from now, one live stays live, and one superseded is receiving again, so that a
// repack's collection, which deletes a pack only while it is superseded, leaves the bytes this push
// is about to write alone. One not live takes the size and the count given now, which are those of
// the bytes about to be written: a write refused for a size that was not theirs leaves nothing to
// hold the next one to.
func (n *NS) ReceivePack(ctx context.Context, workflow string, p Pack) (string, error) {
	switch {
	case !objectID.MatchString(p.Name):
		return "", fmt.Errorf("db: a pack of %s named %q, where a pack is named by its checksum", workflow, p.Name)
	case p.Size <= 0 || p.Objects < 0:
		return "", fmt.Errorf("db: pack %s of %s is %d bytes holding %d objects", p.Name, workflow, p.Size, p.Objects)
	}
	var key string
	err := n.tx.QueryRow(ctx,
		`select repository from workflows where namespace = $1 and name = $2`,
		n.namespace, workflow).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return "", fmt.Errorf("db: the repository of %s could not be read: %w", workflow, err)
	}
	if _, err := n.tx.Exec(ctx,
		`insert into git_packs (namespace, repository, name, size, objects)
		 values ($1, $2, $3, $4, $5)
		 on conflict (namespace, repository, name) do update
		   set state = 'receiving', superseded_at = null, created_at = now(),
		       size = excluded.size, objects = excluded.objects
		 where git_packs.state <> 'live'`,
		n.namespace, key, p.Name, p.Size, p.Objects); err != nil {
		return "", fmt.Errorf("db: pack %s of %s could not be recorded: %w", p.Name, workflow, err)
	}
	return key, nil
}

// PackLive makes a received pack one a fetch reads, in the transaction that moves the refs onto what
// it holds, under the same lock on the workflow's row. A pack no longer receiving, which the
// collection took once it had been receiving past the grace, is ErrNoPack, and the push that wrote
// it fails rather than naming objects the store no longer holds. One live already stays live.
func (n *NS) PackLive(ctx context.Context, workflow, name string) error {
	var key string
	err := n.tx.QueryRow(ctx,
		`select repository from workflows where namespace = $1 and name = $2 for no key update`,
		n.namespace, workflow).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", ErrNoWorkflow, n.namespace, workflow)
	}
	if err != nil {
		return fmt.Errorf("db: the repository of %s could not be locked: %w", workflow, err)
	}
	tag, err := n.tx.Exec(ctx,
		`update git_packs set state = 'live'
		 where namespace = $1 and repository = $2 and name = $3 and state in ('receiving', 'live')`,
		n.namespace, key, name)
	if err != nil {
		return fmt.Errorf("db: pack %s of %s could not be made live: %w", name, workflow, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s of %s/%s", ErrNoPack, name, n.namespace, workflow)
	}
	return nil
}

// exec runs one statement and answers how many rows it touched.
func exec(ctx context.Context, tx pgx.Tx, sql string, args ...any) (int64, error) {
	tag, err := tx.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
