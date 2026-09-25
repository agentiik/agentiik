// Package audit is the installation's audit log: what an entry is, how the chain is hashed and
// verified, and how the log is exported outside the installation.
//
// "The audit log is append-only and chained. Each entry carries the hash of the previous one, so an
// edit or a deletion leaves a break, not a silence. It is exported continuously outside the
// installation, since the incident's own host may be unreadable."
//
// # Where the chain is made
//
// An entry is appended in the transaction of the act it records, so that an act never lands
// unrecorded and an entry never records an act that was rolled back. Two acts committing at once
// would each read the same last entry and fork the chain if each hashed on its own, so the chain is
// made by PostgreSQL rather than here: the insert takes the head of the chain under a row lock, and
// numbers, dates and hashes the entry after it (migration 0030). Appends take turns, each waiting
// for the one before it to commit or roll back. This package spells the same hash again, in Sum, so
// that a chain is verified by code that is not the code that wrote it, and a copy outside the
// installation is verified with no database at all.
//
// # What is hashed
//
// SHA-256 over the label "agentiik audit 1", the previous entry's hash, the entry's number as eight
// bytes, and then each of its fields as four bytes of length followed by its UTF-8: the moment to the
// microsecond in UTC, the actor, the action, the namespace (empty for an act on the installation),
// the target, the result and the detail, a JSON object kept as the text it was written in. The
// first entry follows thirty-two zero bytes.
//
// # What it never holds
//
// A secret's value. A secret write records the name, the provider, the path and whether a value
// was written, and a join token issued records its identifier and never the token.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The acts recorded, as the documentation's audit log row names them: manual trigger,
// cancellation, secret write, runner policy change, and runner drain and revocation. Approval
// arrives with the wait step in v0.8.0, and the identity and access events with principals in
// v0.3.0.
const (
	// RunTrigger is a run started by hand, POST /api/v1/{ns}/workflows/{workflow}/runs.
	RunTrigger = "run.trigger"
	// RunCancel is a run asked to cancel, POST /api/v1/runs/{id}/cancel.
	RunCancel = "run.cancel"
	// SecretWrite is a secret declared, changed or given a value, PUT
	// /api/v1/{ns}/secrets/{name}, and SecretDelete one removed, DELETE on the same path.
	SecretWrite  = "secret.write"
	SecretDelete = "secret.delete"
	// RunnerPoolCreate and JoinTokenIssue are the runner policy changes this milestone has: a
	// pool created, with the namespaces it accepts and its ceilings, and a join token issued,
	// which lets one machine into a pool with a set of labels.
	RunnerPoolCreate = "runner_pool.create"
	JoinTokenIssue   = "join_token.issue"
	// RunnerDrain and RunnerRevoke are POST /api/v1/runners/{runner}/drain and /revoke.
	RunnerDrain  = "runner.drain"
	RunnerRevoke = "runner.revoke"
)

// The results an entry records.
const (
	// Done is an act that changed something.
	Done = "done"
	// Unchanged is an act asked of something already so: a run asked to cancel after it ended,
	// a runner drained twice or revoked again. It is recorded all the same, since who asked is
	// part of what happened.
	Unchanged = "unchanged"
)

// Record is what an act appends: everything but its place in the chain, which the database gives
// it. The namespace is the one the appending transaction is bound to, or none on the installation.
type Record struct {
	Actor  string
	Action string
	Target string
	Result string

	// Detail is the rest of the act, marshalled as a JSON object. Never a secret's value.
	Detail map[string]any
}

// Check refuses a record the log would refuse, before a transaction is spent on it.
func (r Record) Check() error {
	switch {
	case r.Actor == "":
		return errors.New("audit: an act nobody did, and every entry names who acted")
	case r.Action == "":
		return errors.New("audit: an act of no kind")
	case r.Target == "":
		return fmt.Errorf("audit: a %s done to nothing", r.Action)
	case r.Result != Done && r.Result != Unchanged:
		return fmt.Errorf("audit: a %s whose result is %q, and it is done or unchanged", r.Action, r.Result)
	}
	return nil
}

// DetailText is the detail as the log keeps it: a JSON object, {} where there is none. The keys come
// out sorted, since encoding/json sorts a map's, so one detail has one spelling.
func (r Record) DetailText() (string, error) {
	if r.Detail == nil {
		return "{}", nil
	}
	b, err := json.Marshal(r.Detail)
	if err != nil {
		return "", fmt.Errorf("audit: the detail of a %s could not be written: %w", r.Action, err)
	}
	return string(b), nil
}

// HashSize is the size of a hash in the chain.
const HashSize = sha256.Size

// Genesis is what the first entry follows.
var Genesis = make([]byte, HashSize)

// Entry is one link of the chain, as the log holds it and as the export carries it.
//
// At and Detail are strings on the wire, the moment as it is hashed and the detail as the text it
// was written in, so that whatever receives the export and writes it down again keeps the bytes the
// hash was computed over: a receiver that re-encoded a JSON object would reorder or respace it.
type Entry struct {
	Seq       int64
	At        time.Time
	Actor     string
	Action    string
	Namespace string
	Target    string
	Result    string
	Detail    string
	PrevHash  []byte
	Hash      []byte
}

// Moment is the moment as the chain spells it: UTC, to the microsecond, as PostgreSQL keeps it.
func Moment(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

// Sum is the hash this entry should carry, computed from its fields and the hash it follows.
func (e Entry) Sum() []byte {
	h := sha256.New()
	h.Write([]byte("agentiik audit 1"))
	h.Write(e.PrevHash)
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(e.Seq))
	h.Write(seq[:])
	for _, field := range []string{Moment(e.At), e.Actor, e.Action, e.Namespace, e.Target, e.Result, e.Detail} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return h.Sum(nil)
}

// wire is an entry as one line of the export.
type wire struct {
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Namespace string `json:"namespace,omitempty"`
	Target    string `json:"target"`
	Result    string `json:"result"`
	Detail    string `json:"detail"`
	PrevHash  string `json:"prev_hash"`
	Hash      string `json:"hash"`
}

// MarshalJSON writes an entry as one line of the export.
func (e Entry) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{
		Seq: e.Seq, At: Moment(e.At), Actor: e.Actor, Action: e.Action, Namespace: e.Namespace,
		Target: e.Target, Result: e.Result, Detail: e.Detail,
		PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash),
	})
}

// UnmarshalJSON reads one line of the export.
func (e *Entry) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	at, err := time.Parse("2006-01-02T15:04:05.000000Z", w.At)
	if err != nil {
		return fmt.Errorf("audit: entry %d is dated %q, which is not a moment as the chain writes one", w.Seq, w.At)
	}
	prev, err := hex.DecodeString(w.PrevHash)
	if err != nil {
		return fmt.Errorf("audit: entry %d follows %q, which is not a hash", w.Seq, w.PrevHash)
	}
	hash, err := hex.DecodeString(w.Hash)
	if err != nil {
		return fmt.Errorf("audit: entry %d carries %q, which is not a hash", w.Seq, w.Hash)
	}
	*e = Entry{
		Seq: w.Seq, At: at, Actor: w.Actor, Action: w.Action, Namespace: w.Namespace,
		Target: w.Target, Result: w.Result, Detail: w.Detail, PrevHash: prev, Hash: hash,
	}
	return nil
}

// Break is where a chain stops holding: the first entry that does not follow the one before it,
// or whose hash is not the one its fields give.
type Break struct {
	Seq int64
	Why string
}

func (b *Break) Error() string {
	return fmt.Sprintf("audit: the chain breaks at entry %d: %s", b.Seq, b.Why)
}

// Chain verifies entries one at a time, in order, each against the one before it.
//
// The zero value starts at the beginning, before entry 1 and following Genesis. One made with From
// starts after an entry already verified, which is how the export carries on from the last entry
// the sink accepted rather than reading the log again from its first.
type Chain struct {
	seq  int64
	hash []byte
}

// From is a chain that carries on after entry seq, whose hash was hash.
func From(seq int64, hash []byte) *Chain {
	return &Chain{seq: seq, hash: bytes.Clone(hash)}
}

// Next checks one entry, the one after the last it was given, and answers a *Break if it does not
// follow. A chain that has broken stays where it broke.
func (c *Chain) Next(e Entry) error {
	prev := c.hash
	if prev == nil {
		prev = Genesis
	}
	switch {
	case e.Seq != c.seq+1:
		// A number skipped is an entry removed; a number repeated or going back is an entry
		// put where another was.
		return &Break{Seq: c.seq + 1, Why: fmt.Sprintf("entry %d comes next, and it is not there: the next one is %d", c.seq+1, e.Seq)}
	case !bytes.Equal(e.PrevHash, prev):
		return &Break{Seq: e.Seq, Why: "it does not carry the hash of the entry before it, so an entry before it was changed or removed"}
	case !bytes.Equal(e.Hash, e.Sum()):
		return &Break{Seq: e.Seq, Why: "its hash is not the one its fields give, so it was changed after it was written"}
	}
	c.seq, c.hash = e.Seq, bytes.Clone(e.Hash)
	return nil
}

// Last is the last entry verified and its hash: 0 and Genesis before any.
func (c *Chain) Last() (int64, []byte) {
	if c.hash == nil {
		return c.seq, Genesis
	}
	return c.seq, c.hash
}

// Verify checks a whole chain from its first entry, and answers the first *Break.
func Verify(entries []Entry) error {
	var c Chain
	for _, e := range entries {
		if err := c.Next(e); err != nil {
			return err
		}
	}
	return nil
}
