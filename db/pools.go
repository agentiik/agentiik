package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The pool, which is the thing an administrator creates before any machine exists.
//
// "An administrator creates a runner pool with its labels, its accepted namespaces and its
// resource ceilings, then issues a join token." Everything a runner is allowed is written here
// first: a token draws its labels from the pool, a machine draws its labels from the token, and
// what a runner will accept work for is the pool's rather than its own.

// ErrNoRunnerPool is a pool nobody created.
var ErrNoRunnerPool = errors.New("db: no runner pool of that name")

// RunnerPool is a set of hosts and the policy they carry.
type RunnerPool struct {
	Name   string   `json:"pool"`
	Labels []string `json:"labels"`

	// AcceptedNamespaces is whose work this pool runs. Empty is every namespace, which is
	// what an installation with one pool has and what it should not have to write down.
	AcceptedNamespaces []string `json:"accepted_namespaces"`

	// The ceilings, over what a task asks for rather than over the host. Zero is no ceiling
	// of that kind, which is the ordinary case for a pool of uniform hosts where the host
	// itself is the limit.
	MaxCPU         int   `json:"max_cpu,omitempty"`
	MaxMemoryBytes int64 `json:"max_memory_bytes,omitempty"`
	MaxDiskBytes   int64 `json:"max_disk_bytes,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`

	// Runners and Ready are what the pool actually holds. A pool with no ready runner is
	// what a namespace writing allowed_runner_pools against it needs to see, because a
	// selector naming it would queue for ever and say nothing about why.
	Runners int `json:"runners"`
	Ready   int `json:"ready"`
}

// CreateRunnerPool writes one. It is the first thing that happens, before any token and any
// machine.
func (w *Wide) CreateRunnerPool(ctx context.Context, p RunnerPool) error {
	switch {
	case p.Name == "":
		return errors.New("db: a runner pool with no name")
	case p.CreatedBy == "":
		return errors.New("db: a runner pool nobody created")
	}
	_, err := w.tx.Exec(ctx,
		`insert into runner_pools (name, labels, accepted_namespaces,
		                           max_cpu, max_memory_bytes, max_disk_bytes, created_by)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		p.Name, orEmptyStrings(p.Labels), orEmptyStrings(p.AcceptedNamespaces),
		zeroIsNull(p.MaxCPU), zeroIsNull64(p.MaxMemoryBytes), zeroIsNull64(p.MaxDiskBytes),
		p.CreatedBy)
	if err != nil {
		return fmt.Errorf("db: runner pool %s could not be created: %w", p.Name, err)
	}
	return nil
}

// RunnerPoolNamed answers one, without its counts, which is what a check before a write needs.
func (w *Wide) RunnerPoolNamed(ctx context.Context, name string) (RunnerPool, error) {
	var p RunnerPool
	var cpu *int
	var memory, disk *int64
	err := w.tx.QueryRow(ctx, `
		select name, labels, accepted_namespaces, max_cpu, max_memory_bytes, max_disk_bytes,
		       created_at, created_by
		from runner_pools where name = $1`, name).
		Scan(&p.Name, &p.Labels, &p.AcceptedNamespaces, &cpu, &memory, &disk,
			&p.CreatedAt, &p.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunnerPool{}, ErrNoRunnerPool
	}
	if err != nil {
		return RunnerPool{}, fmt.Errorf("db: runner pool %s could not be read: %w", name, err)
	}
	p.MaxCPU, p.MaxMemoryBytes, p.MaxDiskBytes = orZero(cpu), orZero64(memory), orZero64(disk)
	return p, nil
}

// RunnerPools is the listing, with what each pool holds.
func (w *Wide) RunnerPools(ctx context.Context) ([]RunnerPool, error) {
	rows, err := w.tx.Query(ctx, `
		select p.name, p.labels, p.accepted_namespaces,
		       p.max_cpu, p.max_memory_bytes, p.max_disk_bytes, p.created_at, p.created_by,
		       count(r.id) filter (where r.state <> 'revoked'),
		       count(r.id) filter (where r.state = 'ready')
		from runner_pools p left join runners r on r.pool = p.name
		group by p.name order by p.name`)
	if err != nil {
		return nil, fmt.Errorf("db: the runner pools could not be read: %w", err)
	}
	defer rows.Close()

	var pools []RunnerPool
	for rows.Next() {
		var p RunnerPool
		var cpu *int
		var memory, disk *int64
		if err := rows.Scan(&p.Name, &p.Labels, &p.AcceptedNamespaces, &cpu, &memory, &disk,
			&p.CreatedAt, &p.CreatedBy, &p.Runners, &p.Ready); err != nil {
			return nil, fmt.Errorf("db: a runner pool could not be read: %w", err)
		}
		p.MaxCPU, p.MaxMemoryBytes, p.MaxDiskBytes = orZero(cpu), orZero64(memory), orZero64(disk)
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

func zeroIsNull64(n int64) *int64 {
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

func orZero64(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
