package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pool, which is the thing an administrator creates before any machine exists.
//
// "An administrator creates a runner pool with its labels, its accepted namespaces and its
// resource ceilings, then issues a join token." Everything a runner is allowed is written here
// first: a token draws its labels from the pool, a machine draws its labels from the token, and
// what a runner will accept work for is the pool's rather than its own.

// ErrNoRunnerPool is a pool nobody created.
var ErrNoRunnerPool = errors.New("db: no runner pool of that name")

// ErrRunnerPoolExists is a pool created under a name another pool already has.
var ErrRunnerPoolExists = errors.New("db: a runner pool of that name exists")

// Containment is what runs a pool's containers.
const (
	// ContainmentHardened is "the default runtime with user-namespace remapping", which every
	// installation gets always, and what a pool that says nothing is given.
	ContainmentHardened = "hardened"
	// ContainmentSandboxed is gVisor's runsc on a pool of its own.
	ContainmentSandboxed = "sandboxed"
	// ContainmentSeparated is a pool on hosts of their own.
	ContainmentSeparated = "separated"
)

// RunnerPool is a set of hosts and the policy they carry.
type RunnerPool struct {
	Name   string
	Labels []string

	// AcceptedNamespaces is whose work this pool runs. Empty is every namespace, which is
	// what an installation with one pool has.
	AcceptedNamespaces []string

	// Ceilings are the most one task may be given here, over what it asks for rather than
	// over the host.
	Ceilings Ceilings

	// Containment is the tier, and empty is ContainmentHardened.
	Containment string

	CreatedAt time.Time
	CreatedBy string
}

// Ceilings are cpu, memory and pids, written as a step writes them: cpu a decimal number of
// cores, "0.5", memory a whole number with a binary suffix, "8Gi". Each is no ceiling of its kind
// where it is empty or zero, which is the ordinary case for a pool of uniform hosts where the
// host itself is the limit.
type Ceilings struct {
	CPU    string
	Memory string
	PIDs   int
}

// CreateRunnerPool writes one. It is the first thing that happens, before any token and any
// machine.
//
// The table holds a pool to the grammar the wire gives it, the name, each label and each
// namespace, and refuses anything else with PostgreSQL's own error. The API says why in words of
// its own before it gets here.
func (w *Wide) CreateRunnerPool(ctx context.Context, p RunnerPool) error {
	switch {
	case p.Name == "":
		return errors.New("db: a runner pool with no name")
	case p.CreatedBy == "":
		return errors.New("db: a runner pool nobody created")
	}
	containment := p.Containment
	if containment == "" {
		containment = ContainmentHardened
	}
	_, err := w.tx.Exec(ctx,
		`insert into runner_pools (name, labels, accepted_namespaces,
		                           ceiling_cpu, ceiling_memory, ceiling_pids, containment, created_by)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.Name, orEmptyStrings(p.Labels), orEmptyStrings(p.AcceptedNamespaces),
		nilIfEmpty(p.Ceilings.CPU), nilIfEmpty(p.Ceilings.Memory), zeroIsNull(p.Ceilings.PIDs),
		containment, p.CreatedBy)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == uniqueViolation && pg.ConstraintName == "runner_pools_pkey" {
		return fmt.Errorf("%w: %s", ErrRunnerPoolExists, p.Name)
	}
	if err != nil {
		return fmt.Errorf("db: runner pool %s could not be created: %w", p.Name, err)
	}
	return nil
}

// uniqueViolation is PostgreSQL's code for a row a unique index already holds.
const uniqueViolation = "23505"

// poolColumns are what a pool is read as, in the order scanPool reads them.
const poolColumns = `name, labels, accepted_namespaces,
	ceiling_cpu, ceiling_memory, ceiling_pids, containment, created_at, created_by`

// scanPool reads one row of poolColumns.
func scanPool(row pgx.Row) (RunnerPool, error) {
	var p RunnerPool
	var cpu, memory *string
	var pids *int
	if err := row.Scan(&p.Name, &p.Labels, &p.AcceptedNamespaces, &cpu, &memory, &pids,
		&p.Containment, &p.CreatedAt, &p.CreatedBy); err != nil {
		return RunnerPool{}, err
	}
	if cpu != nil {
		p.Ceilings.CPU = *cpu
	}
	if memory != nil {
		p.Ceilings.Memory = *memory
	}
	p.Ceilings.PIDs = orZero(pids)
	return p, nil
}

// RunnerPoolNamed answers one.
func (w *Wide) RunnerPoolNamed(ctx context.Context, name string) (RunnerPool, error) {
	p, err := scanPool(w.tx.QueryRow(ctx, `select `+poolColumns+` from runner_pools where name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunnerPool{}, ErrNoRunnerPool
	}
	if err != nil {
		return RunnerPool{}, fmt.Errorf("db: runner pool %s could not be read: %w", name, err)
	}
	return p, nil
}

// RunnerPools is the listing, ordered by name.
func (w *Wide) RunnerPools(ctx context.Context) ([]RunnerPool, error) {
	rows, err := w.tx.Query(ctx, `select `+poolColumns+` from runner_pools order by name`)
	if err != nil {
		return nil, fmt.Errorf("db: the runner pools could not be read: %w", err)
	}
	defer rows.Close()

	var pools []RunnerPool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, fmt.Errorf("db: a runner pool could not be read: %w", err)
		}
		pools = append(pools, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the runner pools could not be read: %w", err)
	}
	return pools, nil
}

// Accepts says whether this pool runs that namespace's work. No list at all is every namespace.
func (p RunnerPool) Accepts(namespace string) bool {
	return len(p.AcceptedNamespaces) == 0 || slices.Contains(p.AcceptedNamespaces, namespace)
}

func zeroIsNull(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func orZero(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
