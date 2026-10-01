package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
)

// The series of GET /api/v1/{ns}/stats/steps, over runs of finance/monthly-invoicing laid out an
// hour at a time.

type stepsAnswer struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Bucket   string `json:"bucket,omitempty"`
	By       string `json:"by,omitempty"`
	Workflow string `json:"workflow"`
	Steps    []struct {
		Step            string        `json:"step"`
		Buckets         []stepsBucket `json:"buckets,omitempty"`
		Overall         *stepsBucket  `json:"overall,omitempty"`
		Hours           []stepsHour   `json:"hours,omitempty"`
		Previous        []stepsBucket `json:"previous,omitempty"`
		PreviousOverall *stepsBucket  `json:"previous_overall,omitempty"`
	} `json:"steps"`
}

type stepsBucket struct {
	Since          string          `json:"since"`
	Until          string          `json:"until"`
	Duration       *seriesPercents `json:"duration_ms,omitempty"`
	Attempts       int             `json:"attempts"`
	ExitCodes      []seriesRetry   `json:"exit_codes"`
	ItemsPerMinute *float64        `json:"items_per_minute,omitempty"`
}

type stepsHour struct {
	Weekday  int `json:"weekday"`
	Hour     int `json:"hour"`
	Duration *struct {
		P50 int64 `json:"p50"`
	} `json:"duration_ms,omitempty"`
}

// stepsLaidOut is someRuns with the tasks of finance/monthly-invoicing's two runs laid out in the
// hour starting at from, two days back:
//
//   - its first run's normalize lost its first dispatch, failed with 108 on the next after 5
//     seconds, and succeeded on a second attempt after 2;
//   - its archive fanned out into two shards handed 30 and 10 items, which ran 60 and 30 seconds,
//     the second within the first, so that the fan-out ran a minute;
//   - its second run's normalize is still running;
//   - and its first run ran legacy, a step the head of the workflow no longer declares.
func stepsLaidOut(t *testing.T) (someRuns, time.Time) {
	t.Helper()
	s, from := laidOut(t)
	at := func(d time.Duration) time.Time { return from.Add(d) }
	s.sql(t, `delete from tasks where run_id = any($1)`, []string{s.finance[0], s.finance[1]})
	s.sql(t, `insert into steps (namespace, run_id, step, state) values ('finance', $1, 'legacy', 'succeeded')`, s.finance[0])
	for i, task := range []struct {
		run, step           string
		attempt, requeue    int
		shard               int
		state               string
		exit                any
		dispatched, started any
		finished            any
		items               int
	}{
		{s.finance[0], "normalize", 1, 0, 0, "lost", nil, at(60 * time.Second), at(60 * time.Second), at(61 * time.Second), 0},
		{s.finance[0], "normalize", 1, 1, 0, "failed", 108, at(70 * time.Second), at(70 * time.Second), at(75 * time.Second), 0},
		{s.finance[0], "normalize", 2, 0, 0, "succeeded", 0, at(80 * time.Second), at(80 * time.Second), at(82 * time.Second), 0},
		{s.finance[0], "archive", 1, 0, 1, "succeeded", 0, at(90 * time.Second), at(90 * time.Second), at(150 * time.Second), 30},
		{s.finance[0], "archive", 1, 0, 2, "succeeded", 0, at(100 * time.Second), at(100 * time.Second), at(130 * time.Second), 10},
		{s.finance[0], "legacy", 1, 0, 0, "succeeded", 0, at(160 * time.Second), at(160 * time.Second), at(161 * time.Second), 0},
		{s.finance[1], "normalize", 1, 0, 0, "running", nil, at(5*time.Minute + time.Second), at(5*time.Minute + time.Second), nil, 0},
	} {
		id := fmt.Sprintf("01M2T%021d", i)
		var shard, of any
		if task.shard > 0 {
			shard, of = task.shard, 2
		}
		s.sql(t, `insert into tasks (namespace, id, run_id, step, attempt, requeue, shard_index, shard_of, state, exit_code, dispatched_at, started_at, finished_at)
			values ('finance', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			id, task.run, task.step, task.attempt, task.requeue, shard, of, task.state, task.exit, task.dispatched, task.started, task.finished)
		if task.items > 0 {
			s.sql(t, `insert into task_grants (namespace, task_id, hash, expires_at, scope)
				values ('finance', $1, repeat('a', 64), now() + interval '1 hour', $2)`,
				id, fmt.Sprintf(`{"inputs": [{"port": "orders", "digest": "sha256:%s", "items": %d}, {"port": "rates", "digest": "sha256:%s", "items": 1}]}`,
					strings.Repeat("b", 64), task.items, strings.Repeat("c", 64)))
		}
	}
	return s, from
}

func steps(t *testing.T, h http.Handler, as, path string) stepsAnswer {
	t.Helper()
	w := statsOf(t, h, as, path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s asking for %s was answered %d: %s", as, path, w.Code, w.Body)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var out stepsAnswer
	if err := d.Decode(&out); err != nil {
		t.Fatalf("%s answered %s: %v", path, w.Body, err)
	}
	return out
}

// What each step came to, as openapi.json describes it: the attempts it was handed out, a shard an
// attempt and a lost dispatch handed out again the same one; those that ended by the exit code of
// their last dispatch, the most frequent first; how long they ran from that dispatch to their end;
// and a fan-out's items a minute while any of its shards ran. The steps come in the order the head
// runs them, upstream first, which is not their names', and a step it no longer declares after them.
func TestASeriesOfStepsCountsEachStepsAttempts(t *testing.T) {
	s, from := stepsLaidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	stamp := func(d time.Duration) string { return from.Add(d).Format(time.RFC3339Nano) }
	path := fmt.Sprintf("/api/v1/finance/stats/steps?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m", stamp(0), stamp(30*time.Minute))
	got := steps(t, h, "alice", path)

	var names []string
	for _, st := range got.Steps {
		names = append(names, st.Step)
	}
	if want := []string{"normalize", "archive", "legacy"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("%s answered the steps %v, want %v", path, names, want)
	}
	if got.From != stamp(0) || got.To != stamp(30*time.Minute) || got.Bucket != "15m" || got.Workflow != "monthly-invoicing" {
		t.Errorf("%s answered from %s to %s in %s of %s", path, got.From, got.To, got.Bucket, got.Workflow)
	}
	perMinute := 40.0
	quiet := stepsBucket{Since: stamp(15 * time.Minute), Until: stamp(30*time.Minute - time.Nanosecond), ExitCodes: []seriesRetry{}}
	for _, c := range []struct {
		step string
		want stepsBucket
	}{
		{"normalize", stepsBucket{
			Attempts:  3,
			Duration:  &seriesPercents{P50: 3500, P95: 4850, P99: 4970},
			ExitCodes: []seriesRetry{{ExitCode: code(0), Attempts: 1}, {ExitCode: code(108), Attempts: 1}},
		}},
		{"archive", stepsBucket{
			Attempts:       2,
			Duration:       &seriesPercents{P50: 45000, P95: 58500, P99: 59700},
			ExitCodes:      []seriesRetry{{ExitCode: code(0), Attempts: 2}},
			ItemsPerMinute: &perMinute,
		}},
		{"legacy", stepsBucket{
			Attempts:  1,
			Duration:  &seriesPercents{P50: 1000, P95: 1000, P99: 1000},
			ExitCodes: []seriesRetry{{ExitCode: code(0), Attempts: 1}},
		}},
	} {
		c.want.Since, c.want.Until = stamp(0), stamp(15*time.Minute-time.Nanosecond)
		for _, st := range got.Steps {
			if st.Step != c.step {
				continue
			}
			if want := []stepsBucket{c.want, quiet}; !reflect.DeepEqual(st.Buckets, want) {
				gotJSON, _ := json.MarshalIndent(st.Buckets, "", "  ")
				wantJSON, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("%s came to\n%s\nwant\n%s", c.step, gotJSON, wantJSON)
			}
			// The half hour as one bucket: the first quarter's numbers, since the second ran nothing.
			whole := c.want
			whole.Until = stamp(30*time.Minute - time.Nanosecond)
			if st.Overall == nil || !reflect.DeepEqual(*st.Overall, whole) {
				t.Errorf("%s came to %+v over the range, want %+v", c.step, st.Overall, whole)
			}
			if st.Hours != nil || st.Previous != nil || st.PreviousOverall != nil {
				t.Errorf("%s answered hours %v and the span before %v and %v, none of which was asked for", c.step, st.Hours, st.Previous, st.PreviousOverall)
			}
		}
	}

	// The second quarter, with the first as the span before.
	path = fmt.Sprintf("/api/v1/finance/stats/steps?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m&compare=previous", stamp(15*time.Minute), stamp(30*time.Minute))
	got = steps(t, h, "alice", path)
	for _, st := range got.Steps {
		if len(st.Buckets) != 1 || st.Buckets[0].Attempts != 0 || len(st.Previous) != 1 || st.Previous[0].Since != stamp(0) {
			t.Errorf("%s answered %s as %+v, then %+v before", path, st.Step, st.Buckets, st.Previous)
		}
		if st.Overall == nil || !reflect.DeepEqual(*st.Overall, st.Buckets[0]) || st.PreviousOverall == nil || !reflect.DeepEqual(*st.PreviousOverall, st.Previous[0]) {
			t.Errorf("%s answered %s as %+v over the range and %+v before, where each is its one bucket", path, st.Step, st.Overall, st.PreviousOverall)
		}
	}

	w := statsOf(t, h, "alice", fmt.Sprintf("/api/v1/finance/stats/steps?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m", stamp(0), stamp(15*time.Minute)), "text/csv")
	want := "period,step,since,until,attempts,duration_p50_ms,duration_p95_ms,duration_p99_ms,items_per_minute\r\n" +
		"current,normalize," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",3,3500,4850,4970,\r\n" +
		"current,archive," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",2,45000,58500,59700,40\r\n" +
		"current,legacy," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",1,1000,1000,1000,\r\n" +
		"overall,normalize," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",3,3500,4850,4970,\r\n" +
		"overall,archive," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",2,45000,58500,59700,40\r\n" +
		"overall,legacy," + stamp(0) + "," + stamp(15*time.Minute-time.Nanosecond) + ",1,1000,1000,1000,\r\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Errorf("the steps in CSV were answered %d\n%q\nwant\n%q", w.Code, w.Body, want)
	}
}

// by=hour lays each step's attempts out by the weekday and the hour of the day they were dispatched,
// in UTC: the 168 cells of a week, Monday at midnight first, each with the median of how long they
// ran where any did, and no buckets.
func TestASeriesOfStepsByHourIsTheWeeksHeatmap(t *testing.T) {
	s, from := stepsLaidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	path := fmt.Sprintf("/api/v1/finance/stats/steps?workflow=monthly-invoicing&by=hour&from=%s&to=%s&compare=previous", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	got := steps(t, h, "alice", path)
	if got.By != "hour" || got.Bucket != "" {
		t.Errorf("%s answered by %q in buckets of %q", path, got.By, got.Bucket)
	}
	cell := (int(from.Weekday())+6)%7*24 + from.Hour()
	for _, c := range []struct {
		step string
		p50  int64
	}{{"normalize", 3500}, {"archive", 45000}, {"legacy", 1000}} {
		for _, st := range got.Steps {
			if st.Step != c.step {
				continue
			}
			if len(st.Hours) != 168 || st.Buckets != nil || st.Previous != nil {
				t.Fatalf("%s answered %d cells, the buckets %v and the span before %v", c.step, len(st.Hours), st.Buckets, st.Previous)
			}
			for i, hour := range st.Hours {
				if hour.Weekday != i/24+1 || hour.Hour != i%24 {
					t.Fatalf("%s's cell %d is weekday %d at %d", c.step, i, hour.Weekday, hour.Hour)
				}
				switch {
				case i == cell && (hour.Duration == nil || hour.Duration.P50 != c.p50):
					t.Errorf("%s ran %+v in the hour its attempts were dispatched, want a median of %d", c.step, hour.Duration, c.p50)
				case i != cell && hour.Duration != nil:
					t.Errorf("%s ran %+v on weekday %d at %d, when nothing of it was dispatched", c.step, hour.Duration, hour.Weekday, hour.Hour)
				}
			}
		}
	}
	w := statsOf(t, h, "alice", path, "text/csv")
	lines := strings.Split(strings.TrimSuffix(w.Body.String(), "\r\n"), "\r\n")
	if len(lines) != 1+3*168 || lines[0] != "period,step,weekday,hour,duration_p50_ms" ||
		lines[1+cell] != fmt.Sprintf("current,normalize,%d,%d,3500", cell/24+1, cell%24) || lines[1] != "current,normalize,1,0," {
		t.Errorf("the heatmap in CSV was answered %d lines, starting %q", len(lines), lines[:min(3, len(lines))])
	}
}

// A step belongs to one workflow, so the route reads the one the query names, and refuses a query
// naming none. A workflow the caller does not hold run:read on is answered exactly as one that does
// not exist, in a namespace that does or not, having been asked about all the same.
func TestASeriesOfStepsIsAnsweredOnlyAboutAWorkflowItsCallerReads(t *testing.T) {
	s, from := stepsLaidOut(t)
	h := s.servedTo(t, denying{
		allowed: granted{
			"alice": {{api.RunRead, api.Target{Namespace: "finance"}}},
			"bob":   {{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}}},
			"dave":  {{api.RunRead, api.Target{Namespace: "finance"}}},
		},
		denied: granted{"dave": {{api.RunRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}}},
	})
	hour := fmt.Sprintf("&from=%s&to=%s", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	absent := statsOf(t, h, "bob", "/api/v1/finance/stats/steps?workflow=nothing"+hour, "")
	if absent.Code != http.StatusNotFound {
		t.Fatalf("a workflow nobody pushed was answered %d: %s", absent.Code, absent.Body)
	}
	for _, c := range []struct{ as, path string }{
		{"bob", "/api/v1/finance/stats/steps?workflow=monthly-invoicing" + hour},
		{"dave", "/api/v1/finance/stats/steps?workflow=monthly-invoicing" + hour},
		{"bob", "/api/v1/nowhere/stats/steps?workflow=monthly-invoicing" + hour},
		{"carol", "/api/v1/finance/stats/steps?workflow=monthly-invoicing" + hour},
	} {
		w := statsOf(t, h, c.as, c.path, "")
		if w.Code != absent.Code || w.Body.String() != absent.Body.String() {
			t.Errorf("%s asking for %s was answered %d %s, and a workflow nobody pushed %d %s", c.as, c.path, w.Code, w.Body, absent.Code, absent.Body)
		}
	}
	if got := steps(t, h, "bob", "/api/v1/finance/stats/steps?workflow=payroll"+hour); len(got.Steps) == 0 {
		t.Errorf("bob, who reads payroll, was answered no step of it")
	}
	for _, query := range []string{"", "?by=day", "?workflow=monthly-invoicing&by=week", "?workflow=monthly-invoicing&bucket=5m"} {
		if w := statsOf(t, h, "alice", "/api/v1/finance/stats/steps"+query, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%q was answered %d: %s", query, w.Code, w.Body)
		}
	}
}
