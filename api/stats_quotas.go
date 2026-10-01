package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/db"
)

// statistics answers GET /api/v1/{ns}/stats/quotas: the namespace's load, bucket by bucket, beside
// the quotas it meets as they stand now, in JSON or, to Accept: text/csv, in CSV. Read by whoever
// reads the namespace's quotas: an administrator, and whoever holds a grant in it.
func (s *NamespaceAPI) statistics(w http.ResponseWriter, r *http.Request, _ Principal, over Target) {
	rng, err := readStatsRange(r.URL.Query(), s.now(), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	n, ok := s.read(w, r, over.Namespace)
	if !ok {
		return
	}
	out := statsQuotas{From: stamp(rng.From), To: stamp(rng.To), Bucket: rng.Bucket, Quotas: quotasOf(n.Quotas)}
	err = s.pool.In(r.Context(), over.Namespace, func(ctx context.Context, ns *db.NS) error {
		current, err := ns.QuotaStatistics(ctx, rng.Buckets)
		if err != nil {
			return err
		}
		out.Buckets = quotaBuckets(current, rng.Buckets)
		if !rng.Previous {
			return nil
		}
		from, to, before := rng.before()
		previous, err := ns.QuotaStatistics(ctx, before)
		if err != nil {
			return err
		}
		out.Previous = &statsQuotasBefore{From: stamp(from), To: stamp(to), Buckets: quotaBuckets(previous, before)}
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writeQuotasCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

func quotaBuckets(counted []db.QuotaBucket, b db.Buckets) []statsQuotaBucket {
	out := make([]statsQuotaBucket, len(counted))
	for i, c := range counted {
		since := b.First.Add(time.Duration(i) * b.Width)
		out[i] = statsQuotaBucket{
			Since: stamp(since), Until: stamp(since.Add(b.Width - time.Nanosecond)),
			RunsCreated: c.RunsCreated, RunsRefused: c.RunsRefused, TasksInFlightMax: c.TasksInFlightMax,
			ArtifactBytes: c.ArtifactBytes, ArtifactBytesAdded: c.ArtifactBytesAdded,
		}
	}
	return out
}

// statsQuotas is what GET /api/v1/{ns}/stats/quotas answers, as openapi.json describes it field by
// field.
type statsQuotas struct {
	From     string             `json:"from"`
	To       string             `json:"to"`
	Bucket   string             `json:"bucket"`
	Quotas   Quotas             `json:"quotas"`
	Buckets  []statsQuotaBucket `json:"buckets"`
	Previous *statsQuotasBefore `json:"previous,omitempty"`
}

type statsQuotasBefore struct {
	From    string             `json:"from"`
	To      string             `json:"to"`
	Buckets []statsQuotaBucket `json:"buckets"`
}

type statsQuotaBucket struct {
	Since              string `json:"since"`
	Until              string `json:"until"`
	RunsCreated        int    `json:"runs_created"`
	RunsRefused        int    `json:"runs_refused"`
	TasksInFlightMax   int    `json:"tasks_in_flight_max"`
	ArtifactBytes      int64  `json:"artifact_bytes"`
	ArtifactBytesAdded int64  `json:"artifact_bytes_added"`
}

// writeQuotasCSV answers the namespace's load in RFC 4180: a row a bucket, each quota a column
// beside the series it bounds, empty where it bounds nothing, and the span before after the range
// where compare=previous added it.
func writeQuotasCSV(w http.ResponseWriter, out statsQuotas) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	defer c.Flush()
	c.Write([]string{"period", "since", "until", "runs_created", "runs_refused", "max_runs_per_hour",
		"tasks_in_flight_max", "max_concurrent_tasks", "artifact_bytes", "artifact_bytes_added", "max_artifact_bytes"})
	quota := func(n int64) string {
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	rows := func(period string, buckets []statsQuotaBucket) {
		for _, b := range buckets {
			c.Write([]string{period, b.Since, b.Until,
				strconv.Itoa(b.RunsCreated), strconv.Itoa(b.RunsRefused), quota(int64(out.Quotas.MaxRunsPerHour)),
				strconv.Itoa(b.TasksInFlightMax), quota(int64(out.Quotas.MaxConcurrentTasks)),
				strconv.FormatInt(b.ArtifactBytes, 10), strconv.FormatInt(b.ArtifactBytesAdded, 10), quota(out.Quotas.MaxArtifactBytes)})
		}
	}
	rows("current", out.Buckets)
	if out.Previous != nil {
		rows("previous", out.Previous.Buckets)
	}
}
