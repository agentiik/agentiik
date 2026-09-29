package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Memoisation, the table of it: what a cached step's task published, under the key it was
// handed out with. The key is the controller's to make and the lookup its to decide on; what is
// here is the entry and whether everything it names is still there.

// Memo is one cache entry: the key, the run and step whose task published under it, and each port
// it published.
type Memo struct {
	Key   string
	Run   agk.RunID
	Step  agk.Step
	Ports []MemoPort
}

// MemoPort is one port a cached task published: its envelope by the digest it is stored under,
// written sha256:<hex> as a run's document writes one, its count of items and its size.
type MemoPort struct {
	Port   agk.Port `json:"port"`
	Digest string   `json:"digest"`
	Items  int      `json:"items"`
	Size   int64    `json:"size"`
}

// ErrNoMemo is a key no cache entry is held under.
var ErrNoMemo = errors.New("db: no cache entry under that key")

// Memo reads the cache entry a key names in a namespace.
func (w *Wide) Memo(ctx context.Context, namespace, key string) (Memo, error) {
	m := Memo{Key: key}
	var run, step string
	var ports []byte
	err := w.tx.QueryRow(ctx,
		`select run_id, step, ports from step_cache where namespace = $1 and key = $2`,
		namespace, key).Scan(&run, &step, &ports)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNoMemo
	}
	if err != nil {
		return m, fmt.Errorf("db: the cache entry %s could not be read: %w", key, err)
	}
	m.Run, m.Step = agk.RunID(run), agk.Step(step)
	if err := json.Unmarshal(ports, &m.Ports); err != nil {
		return m, fmt.Errorf("db: the cache entry %s could not be read: %w", key, err)
	}
	return m, nil
}

// Memoise records what a cached task published under its key, replacing an entry the key held:
// the latest run's envelopes are the ones kept the longest.
func (w *Wide) Memoise(ctx context.Context, namespace string, m Memo) error {
	if m.Key == "" || m.Run == "" || m.Step == "" {
		return fmt.Errorf("db: a cache entry names its key, its run and its step, and this one is %+v", m)
	}
	ports := m.Ports
	if ports == nil {
		ports = []MemoPort{}
	}
	doc, err := json.Marshal(ports)
	if err != nil {
		return fmt.Errorf("db: the cache entry %s could not be written: %w", m.Key, err)
	}
	if _, err := w.tx.Exec(ctx,
		`insert into step_cache (namespace, key, run_id, step, ports) values ($1, $2, $3, $4, $5)
		 on conflict (namespace, key) do update
		   set run_id = excluded.run_id, step = excluded.step, ports = excluded.ports, created_at = now()`,
		namespace, m.Key, string(m.Run), string(m.Step), doc); err != nil {
		return fmt.Errorf("db: the cache entry %s could not be written: %w", m.Key, err)
	}
	return nil
}

// Forget removes the entry a key names, which is how an entry found to name something no longer
// there is kept from being asked about again.
func (w *Wide) Forget(ctx context.Context, namespace, key string) error {
	if _, err := w.tx.Exec(ctx, `delete from step_cache where namespace = $1 and key = $2`, namespace, key); err != nil {
		return fmt.Errorf("db: the cache entry %s could not be removed: %w", key, err)
	}
	return nil
}

// MemoAlive says whether everything a cache entry would hand back is still there, and a hit
// therefore one: "a cache entry pointing at an expired artifact is not a hit". Its run's envelopes
// not purged; each envelope's object counted and not being collected; and each file its items name
// a live artifact with no fetch budget, since a one-shot artifact republished would be fetched by
// two runs out of a budget set for one.
//
// Asked in the transaction that records the hit, so that the answer holds for what is written
// with it: a collection that claims an object after this has to find it counted again.
func (w *Wide) MemoAlive(ctx context.Context, namespace string, m Memo, files []agk.URI) (bool, error) {
	var purged bool
	err := w.tx.QueryRow(ctx,
		`select envelopes_purged_at is not null or (expires_at is not null and expires_at <= now())
		 from runs where namespace = $1 and id = $2`,
		namespace, string(m.Run)).Scan(&purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: the run of the cache entry %s could not be read: %w", m.Key, err)
	}
	if purged {
		return false, nil
	}
	for _, p := range m.Ports {
		var counted bool
		err := w.tx.QueryRow(ctx,
			`select refs > 0 and collecting_at is null from artifact_objects
			 where namespace = $1 and digest = $2 for share`,
			namespace, p.Digest).Scan(&counted)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !counted {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("db: the envelope %s of the cache entry %s could not be read: %w", p.Digest, m.Key, err)
		}
	}
	for _, u := range files {
		var live bool
		err := w.tx.QueryRow(ctx,
			`select status = 'live' and expires_at > now() and fetches_left is null from artifacts
			 where namespace = $1 and run_id = $2 and step = $3 and port = $4 and name = $5`,
			namespace, string(u.Run), string(u.Step), string(u.Port), u.Name).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("db: the file %s of the cache entry %s could not be read: %w", u, m.Key, err)
		}
	}
	return true, nil
}
