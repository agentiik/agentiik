package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/db"
)

// activityStatistics answers GET /api/v1/stats/activity, to administrators alone: what every
// namespace together did, bucket by bucket, the runs created by state and the most tasks in flight
// at once, and what the installation is doing as it is asked, in JSON or, to Accept: text/csv, its
// buckets in CSV. Counts alone, naming no run, workflow or namespace: an administrator holds no
// run:read by being one, and what they watch here is how busy the installation is, not what it
// runs.
//
// Served at this path alone, as GET /api/v1/stats/pools is and for its reason: a route under
// /api/v1/{namespace} would take the routes of a namespace named stats made before v0.3.0. The
// span before is not taken.
func (s *RunnerAPI) activityStatistics(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	rng, err := readStatsRange(r.URL.Query(), s.now(), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var buckets []db.ActivityBucket
	var now db.Activity
	err = s.pool.Installation(r.Context(), db.InstallationActivity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		buckets, now, err = wide.ActivityStatistics(ctx, rng.Buckets)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the activity could not be read")
		return
	}
	out := statsActivity{From: stamp(rng.From), To: stamp(rng.To), Bucket: rng.Bucket, Buckets: make([]statsActivityBucket, len(buckets))}
	for i, b := range buckets {
		since := rng.Buckets.First.Add(time.Duration(i) * rng.Buckets.Width)
		out.Buckets[i] = statsActivityBucket{
			Since: stamp(since), Until: stamp(since.Add(rng.Buckets.Width - time.Nanosecond)),
			Runs: runCounts(b.Runs), TasksInFlightMax: b.TasksInFlightMax,
		}
	}
	out.Now = statsActivityNow{
		At:            stamp(now.At),
		Runs:          statsUnended{Queued: now.Runs["queued"], Running: now.Runs["running"], Waiting: now.Runs["waiting"]},
		TasksInFlight: now.TasksInFlight, Slots: now.Slots, RunnersReady: now.RunnersReady, Runners: now.Runners,
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writeActivityCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

// statsActivity is what GET /api/v1/stats/activity answers, as openapi.json describes it field by
// field.
type statsActivity struct {
	From    string                `json:"from"`
	To      string                `json:"to"`
	Bucket  string                `json:"bucket"`
	Buckets []statsActivityBucket `json:"buckets"`
	Now     statsActivityNow      `json:"now"`
}

type statsActivityBucket struct {
	Since            string         `json:"since"`
	Until            string         `json:"until"`
	Runs             statsRunCounts `json:"runs"`
	TasksInFlightMax int            `json:"tasks_in_flight_max"`
}

type statsActivityNow struct {
	At            string       `json:"at"`
	Runs          statsUnended `json:"runs"`
	TasksInFlight int          `json:"tasks_in_flight"`
	Slots         int64        `json:"slots"`
	RunnersReady  int          `json:"runners_ready"`
	Runners       int          `json:"runners"`
}

type statsUnended struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
	Waiting int `json:"waiting"`
}

// writeActivityCSV answers the buckets in RFC 4180, a row a bucket and a column a state. What is
// running now, which is no table of buckets, is not in it.
func writeActivityCSV(w http.ResponseWriter, out statsActivity) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	defer c.Flush()
	c.Write([]string{"since", "until", "queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out", "tasks_in_flight_max"})
	for _, b := range out.Buckets {
		n := b.Runs
		c.Write([]string{b.Since, b.Until,
			strconv.Itoa(n.Queued), strconv.Itoa(n.Running), strconv.Itoa(n.Waiting), strconv.Itoa(n.Succeeded),
			strconv.Itoa(n.Failed), strconv.Itoa(n.Cancelled), strconv.Itoa(n.TimedOut), strconv.Itoa(b.TasksInFlightMax)})
	}
}
