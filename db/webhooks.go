package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Webhooks worth trusting: the webhook a request names, what it checks the caller against, and the
// deliveries it has taken.

// Hook is a webhook armed: the workflow whose default branch declares it, where in its list, and
// what it declares with the values in force.
type Hook struct {
	Workflow string
	Position int
	Commit   string
	Declared json.RawMessage
}

// ErrNoHook is a path and a method no webhook of the namespace answers, which a request is answered
// as an absent one is.
var ErrNoHook = errors.New("db: no webhook of the namespace answers that path and method")

// Hook is the webhook armed at path for method.
func (n *NS) Hook(ctx context.Context, path, method string) (Hook, error) {
	var h Hook
	err := n.tx.QueryRow(ctx,
		`select workflow, position, commit, declared from triggers
		 where namespace = $1 and kind = 'webhook' and path = $2 and method = $3`,
		n.namespace, path, method).Scan(&h.Workflow, &h.Position, &h.Commit, &h.Declared)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hook{}, ErrNoHook
	}
	if err != nil {
		return Hook{}, fmt.Errorf("db: the webhook at %s %s could not be read: %w", method, path, err)
	}
	if h.Declared, err = canonical(h.Declared); err != nil {
		return Hook{}, fmt.Errorf("db: the webhook at %s %s: %w", method, path, err)
	}
	return h, nil
}

// HookRefused counts one request the webhook at path and method refused for want of a caller it
// could prove: "a sudden run of them is an unannounced rotation or somebody guessing".
func (n *NS) HookRefused(ctx context.Context, path, method string, at time.Time) error {
	_, err := n.tx.Exec(ctx,
		`update triggers set failures = failures + 1, failed_at = $4
		 where namespace = $1 and kind = 'webhook' and path = $2 and method = $3`,
		n.namespace, path, method, at)
	if err != nil {
		return fmt.Errorf("db: a refusal of the webhook at %s %s could not be counted: %w", method, path, err)
	}
	return nil
}

// HookCredential is what one webhook checks a caller against: the secret an hmac signature is made
// with, sealed, at the write it is bound to, and the SHA-256 of the certificate an mtls caller
// presents. Either is empty where none was written.
type HookCredential struct {
	Version     int
	Secret      json.RawMessage
	Certificate []byte

	WrittenBy string
	WrittenAt time.Time
}

// ErrNoHookCredential is a webhook no credential was written for.
var ErrNoHookCredential = errors.New("db: no credential was written for the webhook")

// HookCredential is what the webhook of workflow at path and method checks a caller against.
func (n *NS) HookCredential(ctx context.Context, workflow, path, method string) (HookCredential, error) {
	var c HookCredential
	err := n.tx.QueryRow(ctx,
		`select version, sealed, certificate, written_by, written_at from webhook_credentials
		 where namespace = $1 and workflow = $2 and path = $3 and method = $4`,
		n.namespace, workflow, path, method).Scan(&c.Version, &c.Secret, &c.Certificate, &c.WrittenBy, &c.WrittenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HookCredential{}, ErrNoHookCredential
	}
	if err != nil {
		return HookCredential{}, fmt.Errorf("db: the credential of the webhook %s %s of %s could not be read: %w", method, path, workflow, err)
	}
	return c, nil
}

// HookCredentials are the credentials written for the webhooks of workflow, by method and path, for
// a listing to say which are written, when and by whom, and never what.
func (n *NS) HookCredentials(ctx context.Context, workflow string) (map[[2]string]HookCredential, error) {
	rows, err := n.tx.Query(ctx,
		`select path, method, version, sealed, certificate, written_by, written_at from webhook_credentials
		 where namespace = $1 and workflow = $2`, n.namespace, workflow)
	if err != nil {
		return nil, fmt.Errorf("db: the credentials of the webhooks of %s could not be read: %w", workflow, err)
	}
	defer rows.Close()
	out := map[[2]string]HookCredential{}
	for rows.Next() {
		var path, method string
		var c HookCredential
		if err := rows.Scan(&path, &method, &c.Version, &c.Secret, &c.Certificate, &c.WrittenBy, &c.WrittenAt); err != nil {
			return nil, fmt.Errorf("db: the credentials of the webhooks of %s could not be read: %w", workflow, err)
		}
		out[[2]string{method, path}] = c
	}
	return out, rows.Err()
}

// WriteHookSecret replaces the secret of the webhook of workflow at path and method with the one seal
// answers, sealed at the next write of it, and says who wrote it. The certificate written beside it,
// if any, stays.
func (n *NS) WriteHookSecret(ctx context.Context, workflow, path, method, by string, seal func(version int) (json.RawMessage, error)) error {
	var version int
	err := n.tx.QueryRow(ctx,
		`insert into webhook_credentials (namespace, workflow, path, method, version, written_by)
		 values ($1, $2, $3, $4, 0, $5)
		 on conflict (namespace, workflow, path, method) do update set written_by = excluded.written_by
		 returning version`,
		n.namespace, workflow, path, method, by).Scan(&version)
	if err != nil {
		return fmt.Errorf("db: the secret of the webhook %s %s of %s could not be written: %w", method, path, workflow, err)
	}
	sealed, err := seal(version + 1)
	if err != nil {
		return err
	}
	if _, err := n.tx.Exec(ctx,
		`update webhook_credentials set version = $5, sealed = $6, written_by = $7, written_at = now()
		 where namespace = $1 and workflow = $2 and path = $3 and method = $4`,
		n.namespace, workflow, path, method, version+1, sealed, by); err != nil {
		return fmt.Errorf("db: the secret of the webhook %s %s of %s could not be written: %w", method, path, workflow, err)
	}
	return nil
}

// WriteHookCertificate replaces the certificate the webhook of workflow at path and method accepts
// with the one whose SHA-256 is sum. The secret written beside it, if any, stays.
func (n *NS) WriteHookCertificate(ctx context.Context, workflow, path, method, by string, sum []byte) error {
	if _, err := n.tx.Exec(ctx,
		`insert into webhook_credentials (namespace, workflow, path, method, certificate, written_by)
		 values ($1, $2, $3, $4, $5, $6)
		 on conflict (namespace, workflow, path, method) do update
		   set certificate = excluded.certificate, written_by = excluded.written_by, written_at = now()`,
		n.namespace, workflow, path, method, sum, by); err != nil {
		return fmt.Errorf("db: the certificate of the webhook %s %s of %s could not be written: %w", method, path, workflow, err)
	}
	return nil
}

// Delivery takes the delivery its sender named id for the webhook at path and method, at, and says
// whether it was taken already, with the run that delivery started. Deliveries older than forget are
// let go first: a request is refused outside its window, so none of them can come again.
//
// Taken in the transaction the run is created in, so that a delivery whose run was refused is not
// taken, and its sender's retry is answered as the first request would have been. Two copies of one
// delivery arriving together take turns on its row, and the second is answered with the run of the
// first once the first commits.
func (n *NS) Delivery(ctx context.Context, path, method, id string, at time.Time, forget time.Duration) (agk.RunID, bool, error) {
	if _, err := n.tx.Exec(ctx,
		`delete from webhook_deliveries where namespace = $1 and taken_at < $2`,
		n.namespace, at.Add(-forget)); err != nil {
		return "", false, fmt.Errorf("db: the deliveries past their window could not be let go: %w", err)
	}
	tag, err := n.tx.Exec(ctx,
		`insert into webhook_deliveries (namespace, path, method, id, taken_at) values ($1, $2, $3, $4, $5)
		 on conflict (namespace, path, method, id) do nothing`,
		n.namespace, path, method, id, at)
	if err != nil {
		return "", false, fmt.Errorf("db: the delivery %s to %s %s could not be taken: %w", id, method, path, err)
	}
	if tag.RowsAffected() == 1 {
		return "", false, nil
	}
	var run string
	err = n.tx.QueryRow(ctx,
		`select coalesce(run, '') from webhook_deliveries where namespace = $1 and path = $2 and method = $3 and id = $4`,
		n.namespace, path, method, id).Scan(&run)
	if err != nil {
		return "", false, fmt.Errorf("db: the delivery %s to %s %s could not be read: %w", id, method, path, err)
	}
	return agk.RunID(run), true, nil
}

// DeliveryRun records the run the delivery id started.
func (n *NS) DeliveryRun(ctx context.Context, path, method, id string, run agk.RunID) error {
	if _, err := n.tx.Exec(ctx,
		`update webhook_deliveries set run = $5 where namespace = $1 and path = $2 and method = $3 and id = $4`,
		n.namespace, path, method, id, string(run)); err != nil {
		return fmt.Errorf("db: the run of the delivery %s to %s %s could not be recorded: %w", id, method, path, err)
	}
	return nil
}

// RunState is the state run is in now, which a caller waiting on it reads.
func (n *NS) RunState(ctx context.Context, run agk.RunID) (agk.RunState, error) {
	var state string
	err := n.tx.QueryRow(ctx, `select state from runs where namespace = $1 and id = $2`, n.namespace, string(run)).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return 0, fmt.Errorf("db: the state of run %s could not be read: %w", run, err)
	}
	var s agk.RunState
	if err := s.UnmarshalText([]byte(state)); err != nil {
		return 0, fmt.Errorf("db: run %s is in state %q: %w", run, state, err)
	}
	return s, nil
}
