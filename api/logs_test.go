package api_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// A task's log, taken from the runner holding the task: in order, once, capped, and closed by its
// last chunk.

// aChunk is one chunk of the task's log whose lines say what they are.
func aChunk(seq, first int, final bool, lines ...string) api.LogShipment {
	c := api.LogShipment{IdempotencyKey: grantKey, Seq: int64(seq), FirstLine: int64(first), Final: final, Lines: []api.LogLine{}}
	at := time.Date(2026, 9, 10, 6, 42, 31, 0, time.UTC)
	for i, text := range lines {
		c.Lines = append(c.Lines, api.LogLine{At: at.Add(time.Duration(first+i) * time.Millisecond), Text: text})
	}
	return c
}

// holding is a runner that joined and redeemed the task, and the task's grant.
func (g grants) holding(t *testing.T) (credential, grant string) {
	t.Helper()
	credential = g.joined(t)
	grant, _, _ = g.dispatched(t, nil)
	g.redeemed(t, credential, asking(grant))
	return credential, grant
}

// ship is a shipment that has to be taken, and its answer.
func (g grants) ship(t *testing.T, credential, grant string, c api.LogShipment) map[string]any {
	t.Helper()
	w, answer := shipped(t, g.handler, credential, grant, c)
	if w.Code != http.StatusOK {
		t.Fatalf("chunk %d answered %d: %s", c.Seq, w.Code, w.Body)
	}
	return answer
}

// stands says an answer is where a log stands: how many lines of the chunk were written, the next
// chunk expected, the lines held and whether the cap cut the log.
func stands(t *testing.T, answer map[string]any, accepted, next, lines int, truncated bool) {
	t.Helper()
	if answer["accepted"] != float64(accepted) || answer["next_seq"] != float64(next) ||
		answer["lines"] != float64(lines) || answer["truncated"] != truncated {
		t.Errorf("answered %v, want accepted %d, next_seq %d, lines %d, truncated %t", answer, accepted, next, lines, truncated)
	}
}

// logged is the task's log as a reader finds it: where it stands and every line it holds, in order.
func (g grants) logged(t *testing.T) (db.TaskLog, []string) {
	t.Helper()
	var l db.TaskLog
	var chunks []db.LogChunk
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		l, chunks, err = ns.TaskLog(ctx, grantTaskRow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range chunks {
		lines, err := api.ReadLogChunk(t.Context(), g.objects, c.Key)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != c.Lines {
			t.Errorf("chunk %d is indexed with %d lines and holds %d", c.Seq, c.Lines, len(lines))
		}
		for _, line := range lines {
			texts = append(texts, line.Text)
		}
	}
	return l, texts
}

// A chunk shipped again because its answer was lost is answered again and written once, and a
// chunk past a gap is answered with where the gap is and kept nowhere, so that the log a reader
// finds is the lines in the order they were written whatever order the chunks arrived in.
func TestAChunkSentTwiceIsWrittenOnce(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)

	stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, "one", "two", "three")), 3, 2, 3, false)
	stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, "one", "two", "three")), 0, 2, 3, false)
	stands(t, g.ship(t, credential, grant, aChunk(3, 6, false, "six")), 0, 2, 3, false)
	stands(t, g.ship(t, credential, grant, aChunk(2, 4, false, "four", "five")), 2, 3, 5, false)
	stands(t, g.ship(t, credential, grant, aChunk(3, 6, false, "six")), 1, 4, 6, false)
	stands(t, g.ship(t, credential, grant, aChunk(2, 4, false, "four", "five")), 0, 4, 6, false)

	l, lines := g.logged(t)
	if want := []string{"one", "two", "three", "four", "five", "six"}; !slices.Equal(lines, want) {
		t.Errorf("the log holds %q, want %q", lines, want)
	}
	if l.Lines != 6 || l.NextSeq != 4 || l.FinalSeq != 0 {
		t.Errorf("the log stands at %+v", l)
	}

	// The same seq with other lines is not a redelivery, and neither is a chunk that does not
	// begin where the ones before it ended.
	if w, _ := shipped(t, g.handler, credential, grant, aChunk(2, 4, false, "four", "5")); w.Code != http.StatusConflict {
		t.Errorf("chunk two shipped again with other lines answered %d: %s", w.Code, w.Body)
	}
	if w, _ := shipped(t, g.handler, credential, grant, aChunk(4, 9, false, "nine")); w.Code != http.StatusConflict {
		t.Errorf("chunk four beginning at line nine after six lines answered %d: %s", w.Code, w.Body)
	}
	if _, lines := g.logged(t); len(lines) != 6 {
		t.Errorf("refused chunks were written: %q", lines)
	}
}

// The last chunk closes the log, with lines or without, and nothing follows it; the last chunk
// shipped again is answered as held.
func TestTheLastChunkClosesTheLog(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)

	stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, "working")), 1, 2, 1, false)
	stands(t, g.ship(t, credential, grant, aChunk(2, 2, true)), 0, 3, 1, false)
	stands(t, g.ship(t, credential, grant, aChunk(2, 2, true)), 0, 3, 1, false)
	for _, c := range []api.LogShipment{aChunk(3, 2, false, "after"), aChunk(3, 2, true), aChunk(9, 2, true)} {
		if w, _ := shipped(t, g.handler, credential, grant, c); w.Code != http.StatusConflict {
			t.Errorf("chunk %d after the last one answered %d: %s", c.Seq, w.Code, w.Body)
		}
	}
	if l, lines := g.logged(t); l.FinalSeq != 2 || !slices.Equal(lines, []string{"working"}) {
		t.Errorf("the closed log stands at %+v holding %q", l, lines)
	}
}

// A container that wrote nothing has a log all the same, closed with no lines and no object.
func TestALogWithNoLinesIsClosedByAnEmptyLastChunk(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	answer := g.ship(t, credential, grant, aChunk(1, 1, true))
	stands(t, answer, 0, 2, 0, false)
	if answer["uri"] != "agk://log/"+grantRun+"/"+grantRun+"%2Frender%2F1" {
		t.Errorf("an empty log is answered at %v", answer["uri"])
	}
	if l, lines := g.logged(t); l.FinalSeq != 1 || len(lines) != 0 {
		t.Errorf("an empty log stands at %+v holding %q", l, lines)
	}
}

// The caps are applied where the log is written: the lines past the cap are dropped, the line the
// byte cap falls inside is cut at the last whole character before it, and from then on nothing is
// written, though the log still moves on and the last chunk still closes it.
func TestTheCapsCutTheLogWhereItIsWritten(t *testing.T) {
	for name, c := range map[string]struct {
		lines, bytes int
		shipped      []string
		kept         []string
	}{
		"lines": {lines: 3, shipped: []string{"a", "b", "c", "d"}, kept: []string{"a", "b", "c"}},
		"bytes": {bytes: 5, shipped: []string{"abc", "déf", "ghi"}, kept: []string{"abc", "d"}},
	} {
		t.Run(name, func(t *testing.T) {
			g := withLogCaps(t, c.lines, c.bytes)
			credential, grant := g.holding(t)

			stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, c.shipped[:1]...)), 1, 2, 1, false)
			stands(t, g.ship(t, credential, grant, aChunk(2, 2, false, c.shipped[1:]...)), len(c.kept)-1, 3, len(c.kept), true)
			stands(t, g.ship(t, credential, grant, aChunk(2, 2, false, c.shipped[1:]...)), 0, 3, len(c.kept), true)
			// Past the cap nothing is written, and what was still on its way moves the log on,
			// past a gap as well, since there is no order left to keep.
			stands(t, g.ship(t, credential, grant, aChunk(5, 40, false, "more")), 0, 6, len(c.kept), true)
			stands(t, g.ship(t, credential, grant, aChunk(3, 5, false, "late")), 0, 6, len(c.kept), true)
			stands(t, g.ship(t, credential, grant, aChunk(6, 41, true, "last")), 0, 7, len(c.kept), true)

			l, lines := g.logged(t)
			if !slices.Equal(lines, c.kept) || !l.Truncated || l.FinalSeq != 6 {
				t.Errorf("the capped log stands at %+v holding %q, want %q", l, lines, c.kept)
			}
			if w, _ := shipped(t, g.handler, credential, grant, aChunk(7, 42, false, "after")); w.Code != http.StatusConflict {
				t.Errorf("a chunk after the last one of a capped log answered %d", w.Code)
			}
			var rows int
			if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
				`select count(*) from task_log_chunks where task_id = $1`, grantTaskRow).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 2 {
				t.Errorf("the index holds %d chunks, and a capped log is indexed up to the chunk the cap fell inside", rows)
			}
		})
	}
}

// withLogCaps is withGrants with the log's caps set to something a test reaches.
func withLogCaps(t *testing.T, lines, bytes int) grants {
	t.Helper()
	return withRunnerOptions(t, func(o *api.RunnerOptions) { o.LogMaxLines, o.LogMaxBytes = lines, int64(bytes) })
}

// withRunnerOptions is withGrants serving the runner routes with options a test changed.
func withRunnerOptions(t *testing.T, change func(*api.RunnerOptions)) grants {
	t.Helper()
	g := withGrants(t, held{})
	rt, err := api.NewRouter(everything{who: "admin"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	o := api.RunnerOptions{Pool: g.pool, Objects: g.objects, URLs: g.signed, Secrets: held{}}
	change(&o)
	if _, err := api.NewRunners(rt, o); err != nil {
		t.Fatal(err)
	}
	g.handler = rt
	return g
}

// slowly is a store whose every write takes a while, which is the window two shipments of one log
// would both read the log in if nothing held it.
type slowly struct{ artifact.Objects }

func (s slowly) Put(ctx context.Context, key string, r io.Reader) error {
	time.Sleep(20 * time.Millisecond)
	return s.Objects.Put(ctx, key, r)
}

// A runner ships the log of a task its redemption bound to it, and no other: not another runner's,
// not one nobody has redeemed, and not with a grant that is not the task's, or that names another
// task than the body does. It is a runner route, so nothing but a runner credential reaches it.
func TestAnotherRunnersTaskIsRefused(t *testing.T) {
	g := withGrants(t, held{})
	credential := g.joined(t)
	grant, _, _ := g.dispatched(t, nil)
	chunk := aChunk(1, 1, false, "one")

	if w, _ := shipped(t, g.handler, credential, grant, chunk); w.Code != http.StatusConflict {
		t.Errorf("a task nobody redeemed answered %d: %s", w.Code, w.Body)
	}
	g.redeemed(t, credential, asking(grant))
	other := g.joined(t)
	if w, _ := shipped(t, g.handler, other, grant, chunk); w.Code != http.StatusConflict {
		t.Errorf("another runner's task answered %d: %s", w.Code, w.Body)
	}

	elsewhere := chunk
	elsewhere.IdempotencyKey = grantRun + "/render/2"
	for name, c := range map[string]struct {
		credential, grant string
		chunk             api.LogShipment
	}{
		"with no grant":                       {credential, "", chunk},
		"with a grant that is not the task's": {credential, strings.TrimSuffix(grant, grant[len(grant)-4:]) + "AAAA", chunk},
		"with a grant for another task":       {credential, "agkgrant_01M2ZZZZZZZZZZZZZZZZZZZZZZ_Zm9vYmFyYmF6cXV4MTIzNA", chunk},
		"naming another task than the grant":  {credential, grant, elsewhere},
		"with no credential":                  {"", grant, chunk},
		"as an administrator":                 {"admin", grant, chunk},
	} {
		if w, _ := shipped(t, g.handler, c.credential, c.grant, c.chunk); w.Code != http.StatusUnauthorized {
			t.Errorf("a shipment %s answered %d: %s", name, w.Code, w.Body)
		}
	}
	var logs int
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(), `select count(*) from task_logs`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 0 {
		t.Errorf("refused shipments opened %d logs", logs)
	}
}

// A chunk says what it is in the body alone, and one that does not is refused before anything is
// read: the grant never in the body, final on every chunk, lines on every one and at least one on
// every one but the last, and a log beginning at line 1.
func TestAChunkThatIsNotOneIsRefused(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	line := map[string]any{"at": "2026-09-10T06:42:31.882Z", "text": "one"}
	chunk := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"idempotency_key": string(grantKey), "seq": 1, "first_line": 1, "final": false, "lines": []any{line}}
		change(c)
		return c
	}
	for name, body := range map[string]map[string]any{
		"carrying its grant":         chunk(func(c map[string]any) { c["grant"] = grant }),
		"saying nothing of final":    chunk(func(c map[string]any) { delete(c, "final") }),
		"with final null":            chunk(func(c map[string]any) { c["final"] = nil }),
		"with no lines":              chunk(func(c map[string]any) { delete(c, "lines") }),
		"empty and not the last":     chunk(func(c map[string]any) { c["lines"] = []any{} }),
		"counted from 0":             chunk(func(c map[string]any) { c["seq"] = 0 }),
		"first, beginning at line 2": chunk(func(c map[string]any) { c["first_line"] = 2 }),
		"with no key":                chunk(func(c map[string]any) { delete(c, "idempotency_key") }),
		"with a key that is not one": chunk(func(c map[string]any) { c["idempotency_key"] = "render/1" }),
		"with a line saying no at":   chunk(func(c map[string]any) { c["lines"] = []any{map[string]any{"text": "one"}} }),
		"with a line saying no text": chunk(func(c map[string]any) { c["lines"] = []any{map[string]any{"at": "2026-09-10T06:42:31.882Z"}} }),
		"with a line saying more": chunk(func(c map[string]any) {
			c["lines"] = []any{map[string]any{"at": "2026-09-10T06:42:31.882Z", "text": "one", "stream": "stderr"}}
		}),
		"with a line at no instant":    chunk(func(c map[string]any) { c["lines"] = []any{map[string]any{"at": "yesterday", "text": "one"}} }),
		"with a line that is a string": chunk(func(c map[string]any) { c["lines"] = []any{"one"} }),
	} {
		if w, _ := shipped(t, g.handler, credential, grant, body); w.Code != http.StatusBadRequest {
			t.Errorf("a chunk %s answered %d: %s", name, w.Code, w.Body)
		}
	}

	many := make([]any, 4097)
	for i := range many {
		many[i] = map[string]any{"at": "2026-09-10T06:42:31.882Z", "text": ""}
	}
	if w, _ := shipped(t, g.handler, credential, grant, chunk(func(c map[string]any) { c["lines"] = many })); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunk of 4,097 lines answered %d: %s", w.Code, w.Body)
	}
	if w, _ := shipped(t, g.handler, credential, grant, chunk(func(c map[string]any) { c["lines"] = many[:4096] })); w.Code != http.StatusOK {
		t.Errorf("a chunk of 4,096 lines answered %d: %s", w.Code, w.Body)
	}
}

// Finishing what it holds is what a drain and a grace leave a runner to do, so a draining runner
// and a revoked one in its grace both ship the log of a task they hold. Past the grace nothing the
// runner sends is taken.
func TestARunnerDrainedOrRevokedShipsTheLogOfWhatItHolds(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	runner := *g.bound(t)
	order := func(verb string) {
		t.Helper()
		if w, _ := call(t, g.handler, "POST", "/api/v1/runners/"+runner+"/"+verb, "admin", api.Order{Reason: "retired"}); w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", verb, w.Code, w.Body)
		}
	}

	order("drain")
	stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, "draining")), 1, 2, 1, false)
	order("revoke")
	stands(t, g.ship(t, credential, grant, aChunk(2, 2, false, "revoked")), 1, 3, 2, false)

	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`update runners set revoked_at = now() - interval '2 hours', results_accepted_until = now() - interval '1 second' where id = $1`, runner); err != nil {
		t.Fatal(err)
	}
	if w, _ := shipped(t, g.handler, credential, grant, aChunk(3, 3, true, "too late")); w.Code != http.StatusUnauthorized {
		t.Errorf("a revoked runner past its grace answered %d: %s", w.Code, w.Body)
	}
	if _, lines := g.logged(t); !slices.Equal(lines, []string{"draining", "revoked"}) {
		t.Errorf("the log holds %q", lines)
	}
}

// The log of a task is taken whatever has happened to the task since: its deadline passed, which is
// when the last lines of a task that ran out of time arrive, it was cancelled while its container
// was stopping, or it was declared lost while its host was only cut off.
func TestTheLogOfATaskIsTakenPastItsDeadlineAndItsEnding(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	conn := dbtest.Superuser(t, g.super)
	if _, err := conn.Exec(t.Context(), `update task_grants set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	stands(t, g.ship(t, credential, grant, aChunk(1, 1, false, "past the deadline")), 1, 2, 1, false)
	for i, state := range []string{"cancelled", "lost"} {
		if _, err := conn.Exec(t.Context(), `update tasks set state = $1 where id = $2`, state, grantTaskRow); err != nil {
			t.Fatal(err)
		}
		stands(t, g.ship(t, credential, grant, aChunk(i+2, i+2, false, state)), 1, i+3, i+2, false)
	}
}

// The task's row names the log from its first chunk, which is what the purge finds a log by, so a
// log whose result never came back is swept with its run: the purge is handed every object the
// log's lines are in, forgets them once confirmed, and keeps where the log stood. A run past its
// retention takes no more.
func TestThePurgeStillFindsTheLog(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	conn := dbtest.Superuser(t, g.super)
	g.ship(t, credential, grant, aChunk(1, 1, false, "one"))
	g.ship(t, credential, grant, aChunk(2, 2, false, "two"))

	var uri string
	if err := conn.QueryRow(t.Context(), `select log_uri from tasks where id = $1`, grantTaskRow).Scan(&uri); err != nil {
		t.Fatal(err)
	}
	if uri != "agk://log/"+grantRun+"/"+grantRun+"%2Frender%2F1" {
		t.Errorf("the task names its log %q", uri)
	}

	if _, err := conn.Exec(t.Context(), `
		update runs set started_at = now() - interval '2 days', finished_at = now() - interval '1 day',
		                expires_at = now() - interval '1 second'
		where id = $1`, grantRun); err != nil {
		t.Fatal(err)
	}
	if w, _ := shipped(t, g.handler, credential, grant, aChunk(3, 3, true)); w.Code != http.StatusConflict {
		t.Errorf("a chunk of a run past its retention answered %d: %s", w.Code, w.Body)
	}

	expired, err := g.pool.ExpiredLogs(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].Task != grantTaskRow || expired[0].URI != uri || len(expired[0].Keys) != 2 {
		t.Fatalf("the purge found %+v", expired)
	}
	_, held := g.logged(t)
	for i, key := range expired[0].Keys {
		if _, err := api.ReadLogChunk(t.Context(), g.objects, key); err != nil {
			t.Errorf("the purge was handed %s, which is not a chunk of the log: %s", key, err)
		}
		if !strings.Contains(key, "/"+grantTaskRow+"/") || !strings.HasPrefix(key, "finance/logs/"+grantRun+"/") {
			t.Errorf("chunk %d is kept at %s", i+1, key)
		}
	}
	if len(held) != 2 {
		t.Errorf("the log holds %q", held)
	}

	if n, err := g.pool.LogsPurged(t.Context(), expired); err != nil || n != 1 {
		t.Fatalf("the purge confirmed %d: %v", n, err)
	}
	if again, err := g.pool.ExpiredLogs(t.Context(), 0); err != nil || len(again) != 0 {
		t.Errorf("a purged log is found again: %+v, %v", again, err)
	}
	if l, lines := g.logged(t); len(lines) != 0 || l.Lines != 2 {
		t.Errorf("a purged log stands at %+v holding %q, and keeps its count and nothing it names", l, lines)
	}
}

// A reader follows the index to the chunks and takes from each only the bytes its key was written
// for, so an object that holds anything else is refused rather than read as the log.
func TestAChunkIsReadBackOnlyAsItWasWritten(t *testing.T) {
	g := withGrants(t, held{})
	credential, grant := g.holding(t)
	g.ship(t, credential, grant, aChunk(1, 1, false, "one"))
	keys := chunkKeys(t, g)
	if len(keys) != 1 {
		t.Fatalf("the log is indexed as %q", keys)
	}
	if err := g.objects.Put(t.Context(), keys[0], strings.NewReader(`{"at":"2026-09-10T06:42:31Z","text":"not one"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if lines, err := api.ReadLogChunk(t.Context(), g.objects, keys[0]); err == nil {
		t.Errorf("a chunk holding other bytes was read as %v", lines)
	}
}

// chunkKeys are the objects the task's log is indexed at, in order.
func chunkKeys(t *testing.T, g grants) []string {
	t.Helper()
	var keys []string
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, chunks, err := ns.TaskLog(ctx, grantTaskRow)
		for _, c := range chunks {
			keys = append(keys, c.Key)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

// Shipments of one log arriving at once are taken one after the other, so a chunk a runner retried
// before its first attempt was answered is still written once.
func TestOneChunkShippedManyTimesAtOnceIsWrittenOnce(t *testing.T) {
	g := withRunnerOptions(t, func(o *api.RunnerOptions) { o.Objects = slowly{o.Objects} })
	credential, grant := g.holding(t)
	g.ship(t, credential, grant, aChunk(1, 1, false, "zero"))
	const times = 8
	accepted := make(chan any, times)
	var wg sync.WaitGroup
	for range times {
		wg.Go(func() {
			w, answer := shipped(t, g.handler, credential, grant, aChunk(2, 2, false, "one", "two"))
			if w.Code != http.StatusOK {
				t.Errorf("a chunk shipped at once with others answered %d: %s", w.Code, w.Body)
			}
			accepted <- answer["accepted"]
		})
	}
	wg.Wait()
	close(accepted)
	var written []any
	for a := range accepted {
		if a != 0.0 {
			written = append(written, a)
		}
	}
	if len(written) != 1 || written[0] != 2.0 {
		t.Errorf("a chunk shipped %d times at once was written %v", times, written)
	}
	if l, lines := g.logged(t); l.Lines != 3 || !slices.Equal(lines, []string{"zero", "one", "two"}) {
		t.Errorf("the log stands at %+v holding %q", l, lines)
	}
}
