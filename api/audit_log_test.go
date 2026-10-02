package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// auditRead is what GET /api/v1/auth/audit answers, as openapi.json names it field by field.
type auditRead struct {
	Entries []struct {
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
	} `json:"entries"`
	Head     int64 `json:"head"`
	Verified int64 `json:"verified"`
}

// The audit log is read by an administrator alone, the newest entries first, each as the export
// writes it, a page at a time, narrowed by who did what, where and to what; beside the page, the
// last entry appended and the last the chain was proved to. Anybody else is refused, and a query
// the route cannot read is refused before anything is read.
func TestTheAuditLogIsReadByAnAdministratorNewestFirst(t *testing.T) {
	h, pool := withRunnersAuthorizedBy(t, everything{who: "admin"})
	if err := pool.Installation(t.Context(), db.AuditLog, func(ctx context.Context, w *db.Wide) error {
		for i, r := range []struct {
			namespace string
			record    audit.Record
		}{
			{"", audit.Record{Actor: "dana", Action: audit.UserCreate, Target: "erin", Result: audit.Done}},
			{"finance", audit.Record{Actor: "carol", Action: audit.NamespaceUpdate, Target: "finance", Result: audit.Done}},
			{"finance", audit.Record{Actor: "alice", Action: audit.RunCancel, Target: "01JMZ8V1P9C4XQ7K2N4D6F8H0A", Result: audit.Done}},
			{"finance", audit.Record{Actor: "alice", Action: audit.RunCancel, Target: "01JMZ8V1P9C4XQ7K2N4D6F8H0A", Result: audit.Unchanged}},
		} {
			var err error
			if r.namespace == "" {
				err = w.Audit(ctx, r.record)
			} else {
				err = w.AuditIn(ctx, r.namespace, r.record)
			}
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all := audited(t, pool)

	read := func(query string) auditRead {
		t.Helper()
		w, _ := call(t, h, "GET", "/api/v1/auth/audit"+query, "admin", nil)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("an administrator reading the audit log%s was answered %d: %s", query, w.Code, w.Body)
		}
		d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
		d.DisallowUnknownFields()
		var got auditRead
		if err := d.Decode(&got); err != nil {
			t.Fatalf("the audit log answered %s: %v", w.Body, err)
		}
		return got
	}
	seqs := func(r auditRead) string {
		var out []string
		for _, e := range r.Entries {
			out = append(out, fmt.Sprint(e.Seq))
		}
		return strings.Join(out, " ")
	}

	head := all[len(all)-1].Seq
	got := read("")
	if got.Head != head || len(got.Entries) != len(all) || got.Entries[0].Seq != head || got.Entries[len(got.Entries)-1].Seq != all[0].Seq {
		t.Fatalf("the log was read as %s up to %d, and holds %d entries up to %d", seqs(got), got.Head, len(all), head)
	}
	// Each entry as the export writes it, so that it verifies as the export's copy would.
	line, err := json.Marshal(all[len(all)-1])
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(got.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(line) {
		t.Errorf("the last entry was answered as\n%s\nand the export writes it\n%s", first, line)
	}

	// A page at a time, the next one before where the last ended.
	page := read("?limit=2")
	if len(page.Entries) != 2 || page.Entries[0].Seq != head || page.Entries[1].Seq != head-1 {
		t.Errorf("a page of two read %s", seqs(page))
	}
	next := read(fmt.Sprintf("?limit=2&before=%d", page.Entries[1].Seq))
	if len(next.Entries) != 2 || next.Entries[0].Seq != head-2 {
		t.Errorf("the page before %d read %s", page.Entries[1].Seq, seqs(next))
	}

	// Narrowed, each as written.
	for query, want := range map[string]int{
		"?actor=alice":                       2,
		"?action=run.cancel":                 2,
		"?action=run.cancel&actor=carol":     0,
		"?namespace=finance":                 3,
		"?target=01JMZ8V1P9C4XQ7K2N4D6F8H0A": 2,
		"?actor=Alice":                       0,
		"?since=2000-01-01T00:00:00Z":        len(all),
		"?until=2000-01-01T00:00:00Z":        0,
	} {
		if got := read(query); len(got.Entries) != want {
			t.Errorf("%s read %d entries, want %d: %s", query, len(got.Entries), want, seqs(got))
		}
	}
	for _, e := range read("?namespace=-").Entries {
		if e.Namespace != "" {
			t.Errorf("the acts on the installation read entry %d, done in %s", e.Seq, e.Namespace)
		}
	}
	if got := read("?namespace=-&actor=dana"); len(got.Entries) != 1 || got.Entries[0].Action != "user.create" {
		t.Errorf("dana's act on the installation read %s", seqs(got))
	}

	for _, query := range []string{"?limit=0", "?limit=201", "?limit=many", "?before=0", "?before=-3", "?action=cancel", "?since=yesterday", "?since=2026-10-02T10:00:00Z&until=2026-10-02T09:00:00Z"} {
		if w, _ := call(t, h, "GET", "/api/v1/auth/audit"+query, "admin", nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s was answered %d", query, w.Code)
		}
	}
	if w, _ := call(t, h, "GET", "/api/v1/auth/audit", "alice", nil); w.Code != http.StatusForbidden {
		t.Errorf("a caller who administers nothing was answered %d", w.Code)
	}
	if w, _ := call(t, h, "GET", "/api/v1/auth/audit", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a request with no credential was answered %d", w.Code)
	}

	// Reading the log records nothing in it.
	if after := audited(t, pool); len(after) != len(all) {
		t.Errorf("reading the log appended %d entries to it", len(after)-len(all))
	}
}
