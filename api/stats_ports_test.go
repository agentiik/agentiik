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

// The series of GET /api/v1/{ns}/stats/ports, over what finance/monthly-invoicing's steps published
// in the hour starting two days back.

type portsAnswer struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Bucket   string `json:"bucket"`
	Workflow string `json:"workflow"`
	Steps    []struct {
		Step     string        `json:"step"`
		Buckets  []portsBucket `json:"buckets"`
		Previous []portsBucket `json:"previous,omitempty"`
	} `json:"steps"`
}

type portsBucket struct {
	Since string           `json:"since"`
	Until string           `json:"until"`
	Items map[string]int64 `json:"items"`
}

// portsLaidOut is stepsLaidOut with what the steps published: the first run's normalize 12 items
// on ok and 3 on rejected, its archive 40 on ok and its legacy 1 on out, a port the head no longer
// declares, and the second run's normalize 5 on ok and nothing on rejected.
func portsLaidOut(t *testing.T) (someRuns, time.Time) {
	t.Helper()
	s, from := stepsLaidOut(t)
	port := func(items int) string {
		return fmt.Sprintf(`{"digest": "sha256:%s", "size": %d, "items": %d}`, strings.Repeat("d", 64), 10*items, items)
	}
	for _, c := range []struct {
		run, step, ports string
	}{
		{s.finance[0], "normalize", `{"ok": ` + port(12) + `, "rejected": ` + port(3) + `}`},
		{s.finance[0], "archive", `{"ok": ` + port(40) + `}`},
		{s.finance[0], "legacy", `{"out": ` + port(1) + `}`},
		{s.finance[1], "normalize", `{"ok": ` + port(5) + `}`},
	} {
		s.sql(t, `update steps set ports = $3 where run_id = $1 and step = $2`, c.run, c.step, c.ports)
	}
	return s, from
}

func ports(t *testing.T, h http.Handler, as, path string) portsAnswer {
	t.Helper()
	w := statsOf(t, h, as, path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s asking for %s was answered %d: %s", as, path, w.Code, w.Body)
	}
	d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	d.DisallowUnknownFields()
	var out portsAnswer
	if err := d.Decode(&out); err != nil {
		t.Fatalf("%s answered %s: %v", path, w.Body, err)
	}
	return out
}

// What each step published, port by port, summed over the runs created in a bucket from the count
// each step's row records: every port the step declares named, 0 where it published nothing, and a
// port only an older version declared after them. The steps come in the order the head runs them.
func TestASeriesOfPortsCountsWhatEachStepPublished(t *testing.T) {
	s, from := portsLaidOut(t)
	h := s.servedTo(t, granted{"alice": {{api.RunRead, api.Target{Namespace: "finance"}}}})
	stamp := func(d time.Duration) string { return from.Add(d).Format(time.RFC3339Nano) }
	path := fmt.Sprintf("/api/v1/finance/stats/ports?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m", stamp(0), stamp(30*time.Minute))
	got := ports(t, h, "alice", path)
	if got.From != stamp(0) || got.To != stamp(30*time.Minute) || got.Bucket != "15m" || got.Workflow != "monthly-invoicing" {
		t.Errorf("%s answered from %s to %s in %s of %s", path, got.From, got.To, got.Bucket, got.Workflow)
	}
	bucket := func(i int, items map[string]int64) portsBucket {
		return portsBucket{Since: stamp(time.Duration(i) * 15 * time.Minute), Until: stamp(time.Duration(i+1)*15*time.Minute - time.Nanosecond), Items: items}
	}
	want := map[string][]portsBucket{
		"normalize": {bucket(0, map[string]int64{"ok": 17, "rejected": 3}), bucket(1, map[string]int64{"ok": 0, "rejected": 0})},
		"archive":   {bucket(0, map[string]int64{"ok": 40}), bucket(1, map[string]int64{"ok": 0})},
		"legacy":    {bucket(0, map[string]int64{"out": 1}), bucket(1, map[string]int64{"out": 0})},
	}
	var names []string
	for _, st := range got.Steps {
		names = append(names, st.Step)
		if !reflect.DeepEqual(st.Buckets, want[st.Step]) || st.Previous != nil {
			t.Errorf("%s published %+v, then %+v before, want %+v", st.Step, st.Buckets, st.Previous, want[st.Step])
		}
	}
	if !reflect.DeepEqual(names, []string{"normalize", "archive", "legacy"}) {
		t.Errorf("%s answered the steps %v", path, names)
	}

	path = fmt.Sprintf("/api/v1/finance/stats/ports?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m&compare=previous", stamp(15*time.Minute), stamp(30*time.Minute))
	for _, st := range ports(t, h, "alice", path).Steps {
		if len(st.Previous) != 1 || !reflect.DeepEqual(st.Previous[0], want[st.Step][0]) {
			t.Errorf("%s answered %s's span before as %+v, want %+v", path, st.Step, st.Previous, want[st.Step][0])
		}
	}

	w := statsOf(t, h, "alice", fmt.Sprintf("/api/v1/finance/stats/ports?workflow=monthly-invoicing&from=%s&to=%s&bucket=15m", stamp(0), stamp(15*time.Minute)), "text/csv")
	first, last := stamp(0), stamp(15*time.Minute-time.Nanosecond)
	csv := "period,step,port,since,until,items\r\n" +
		"current,normalize,ok," + first + "," + last + ",17\r\n" +
		"current,normalize,rejected," + first + "," + last + ",3\r\n" +
		"current,archive,ok," + first + "," + last + ",40\r\n" +
		"current,legacy,out," + first + "," + last + ",1\r\n"
	if w.Code != http.StatusOK || w.Body.String() != csv {
		t.Errorf("the ports in CSV were answered %d\n%q\nwant\n%q", w.Code, w.Body, csv)
	}
}

// As a step's series is, what its ports published is answered only about a workflow the caller
// reads, and one it does not read exactly as one that does not exist.
func TestASeriesOfPortsIsAnsweredOnlyAboutAWorkflowItsCallerReads(t *testing.T) {
	s, from := portsLaidOut(t)
	h := s.servedTo(t, denying{
		allowed: granted{
			"bob":  {{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}}},
			"dave": {{api.RunRead, api.Target{Namespace: "finance"}}},
		},
		denied: granted{"dave": {{api.RunRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}}},
	})
	hour := fmt.Sprintf("&from=%s&to=%s", from.Format(time.RFC3339), from.Add(time.Hour).Format(time.RFC3339))
	absent := statsOf(t, h, "bob", "/api/v1/finance/stats/ports?workflow=nothing"+hour, "")
	if absent.Code != http.StatusNotFound {
		t.Fatalf("a workflow nobody pushed was answered %d: %s", absent.Code, absent.Body)
	}
	for _, c := range []struct{ as, path string }{
		{"bob", "/api/v1/finance/stats/ports?workflow=monthly-invoicing" + hour},
		{"dave", "/api/v1/finance/stats/ports?workflow=monthly-invoicing" + hour},
		{"bob", "/api/v1/nowhere/stats/ports?workflow=monthly-invoicing" + hour},
		{"carol", "/api/v1/finance/stats/ports?workflow=monthly-invoicing" + hour},
	} {
		w := statsOf(t, h, c.as, c.path, "")
		if w.Code != absent.Code || w.Body.String() != absent.Body.String() {
			t.Errorf("%s asking for %s was answered %d %s, and a workflow nobody pushed %d %s", c.as, c.path, w.Code, w.Body, absent.Code, absent.Body)
		}
	}
	if got := ports(t, h, "bob", "/api/v1/finance/stats/ports?workflow=payroll"+hour); len(got.Steps) == 0 {
		t.Errorf("bob, who reads payroll, was answered no step of it")
	}
	for _, query := range []string{"", "?workflow=monthly-invoicing&bucket=5m", "?workflow=monthly-invoicing&compare=next"} {
		if w := statsOf(t, h, "dave", "/api/v1/finance/stats/ports"+query, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%q was answered %d: %s", query, w.Code, w.Body)
		}
	}
}
