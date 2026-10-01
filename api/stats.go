package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/db"
)

// Statistics are served by aggregate routes of the API rather than read from Prometheus or computed
// in the browser: "an aggregate over runs discloses the runs, so it answers to the same grants: the
// API filters it by run:read as it filters GET /api/v1/runs". So a series counts the workflows the
// listing of runs would list, found and asked about the same way, and a namespace or a workflow the
// caller cannot read counts nothing, which is what one that does not exist counts.

// statsBuckets are how long a bucket may be, by the name the query gives it.
var statsBuckets = map[string]time.Duration{
	"1m": time.Minute, "15m": 15 * time.Minute, "1h": time.Hour, "1d": 24 * time.Hour,
}

// statsSpan is the range a series covers where the query leaves from out: "the last 24 hours",
// which is the day an operator looks back over first.
const statsSpan = 24 * time.Hour

// statsHistogramBins is the most bins a histogram is asked in: "more than a chart has room to
// draw".
const statsHistogramBins = 100

// statsRange is what a request for a series asks for: the range as it was given or taken, the
// buckets it is counted in, and whether the span before is added to it.
type statsRange struct {
	From, To time.Time
	Bucket   string
	Buckets  db.Buckets
	Previous bool
}

// before is the span just before the range, in as many buckets of the same length, so that a chart
// draws the one behind the other point for point. It is shifted by whole buckets rather than by the
// range's length, which it is wherever from and to fall on bucket boundaries, so that its buckets
// fall on the same boundaries and end where the range's first begins.
func (s statsRange) before() (from, to time.Time, b db.Buckets) {
	shift := time.Duration(s.Buckets.Count) * s.Buckets.Width
	b = s.Buckets
	b.First = b.First.Add(-shift)
	return s.From.Add(-shift), s.To.Add(-shift), b
}

// readStatsRange reads the range a series is asked over, now being when the request is answered.
// Where bucketed is false, as for a heatmap of the hours of a week, a bucket is read to be refused
// where it is none and neither shapes the answer nor bounds the range, and the span before is not
// taken.
func readStatsRange(query url.Values, now time.Time, bucketed bool) (statsRange, error) {
	// Whether each was given rather than whether it is zero, since 0001-01-01T00:00:00Z is a time
	// a query can name.
	rng := statsRange{To: now.UTC()}
	given := map[string]bool{}
	for _, c := range []struct {
		name string
		into *time.Time
	}{{"from", &rng.From}, {"to", &rng.To}} {
		written := query.Get(c.name)
		if written == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, written)
		if err != nil {
			return statsRange{}, fmt.Errorf("%s is %q, which is not a time: one is written in RFC 3339, 2026-09-30T06:00:00Z", c.name, written)
		}
		*c.into, given[c.name] = at.UTC(), true
	}
	if !given["from"] {
		rng.From = rng.To.Add(-statsSpan)
	}
	if !rng.From.Before(rng.To) {
		return statsRange{}, fmt.Errorf("from is %s and to is %s, and a range runs from an instant to a later one", stamp(rng.From), stamp(rng.To))
	}

	rng.Bucket = query.Get("bucket")
	switch span := rng.To.Sub(rng.From); {
	case rng.Bucket != "":
		if _, known := statsBuckets[rng.Bucket]; !known {
			return statsRange{}, fmt.Errorf("bucket is %q, and a bucket is 1m, 15m, 1h or 1d", rng.Bucket)
		}
	// A chart of a few hundred points at most, whatever the range.
	case span <= 2*time.Hour:
		rng.Bucket = "1m"
	case span <= 48*time.Hour:
		rng.Bucket = "15m"
	case span <= 14*24*time.Hour:
		rng.Bucket = "1h"
	default:
		rng.Bucket = "1d"
	}
	switch compare := query.Get("compare"); compare {
	case "":
	case "previous":
		rng.Previous = bucketed
	default:
		return statsRange{}, fmt.Errorf("compare is %q, and the one comparison is previous, the same span just before", compare)
	}
	if !bucketed {
		rng.Bucket = ""
		return rng, nil
	}

	// Whole minutes, quarters, hours and days in UTC: the zero time Truncate counts from is a
	// midnight in UTC, and every length divides a day.
	width := statsBuckets[rng.Bucket]
	first := rng.From.Truncate(width)
	last := rng.To.Add(-time.Nanosecond).Truncate(width)
	// A range too long to count in time.Duration saturates to one far past the limit, which is
	// refused all the same.
	count := int64(last.Sub(first)/width) + 1
	if count > db.MaxBuckets {
		return statsRange{}, fmt.Errorf("from %s to %s is %d buckets of %s, and a series takes at most %d: more than any chart draws, which a query would pay for all the same", stamp(rng.From), stamp(rng.To), count, rng.Bucket, db.MaxBuckets)
	}
	rng.Buckets = db.Buckets{First: first, Width: width, Count: int(count)}
	return rng, nil
}

// readHistogram reads how many bins a histogram of durations is asked in, and 0 where none is.
func readHistogram(query url.Values) (int, error) {
	written := query.Get("histogram")
	if written == "" {
		return 0, nil
	}
	bins, err := strconv.Atoi(written)
	if err != nil || bins < 1 || bins > statsHistogramBins {
		return 0, fmt.Errorf("histogram is %q, and a histogram takes 1 to %d bins, more than a chart has room to draw", written, statsHistogramBins)
	}
	return bins, nil
}

// stamp writes an instant as the API writes every one, in RFC 3339 in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// runStatistics answers GET /api/v1/{ns}/stats/runs: the runs of the namespace's workflows the
// caller holds run:read on, as series, in JSON or, to Accept: text/csv, in CSV.
//
// run:read and never run:read_data, since a series carries counts, durations and exit codes and
// never an item.
func (s *Server) runStatistics(w http.ResponseWriter, r *http.Request, who Principal, within Target, _ Holds) {
	rng, err := readStatsRange(r.URL.Query(), s.now(), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	bins, err := readHistogram(r.URL.Query())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	workflow := r.URL.Query().Get("workflow")
	among := []db.Workflow{}
	if storable(within.Namespace) && storable(workflow) {
		// A name PostgreSQL could not hold is none anything was written under, and counts nothing,
		// as the listing lists nothing for it.
		var ok bool
		if among, ok = s.readable(w, r, within.Namespace, workflow, "the statistics could not be read"); !ok {
			return
		}
	}

	var out statsRuns
	err = s.pool.Installation(r.Context(), db.RunListing, func(ctx context.Context, wide *db.Wide) error {
		var err error
		out.Buckets, out.Overall, out.Histogram, err = runSeries(ctx, wide, among, rng.Buckets, bins)
		if err != nil || !rng.Previous {
			return err
		}
		from, to, b := rng.before()
		out.Previous = &statsRunsBefore{From: stamp(from), To: stamp(to)}
		out.Previous.Buckets, out.Previous.Overall, out.Previous.Histogram, err = runSeries(ctx, wide, among, b, bins)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}
	out.From, out.To, out.Bucket, out.Workflow = stamp(rng.From), stamp(rng.To), rng.Bucket, workflow

	// What is answered depends on Accept, which a cache that keeps nothing is told all the same.
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writeRunsCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

// runSeries reads the buckets of one span, the span taken as one bucket, and, where bins is not
// zero, its histogram.
func runSeries(ctx context.Context, wide *db.Wide, among []db.Workflow, b db.Buckets, bins int) ([]statsRunsBucket, statsRunsBucket, []statsBin, error) {
	counted, err := wide.RunStatistics(ctx, among, b)
	if err != nil {
		return nil, statsRunsBucket{}, nil, err
	}
	// The span as one bucket is counted apart rather than summed from the buckets, since its
	// percentiles are taken over every run of the span and those of the buckets do not combine.
	whole, err := wide.RunStatistics(ctx, among, b.Whole())
	if err != nil {
		return nil, statsRunsBucket{}, nil, err
	}
	buckets := runBuckets(counted, b)
	overall := runBuckets(whole, b.Whole())[0]
	if bins == 0 {
		return buckets, overall, nil, nil
	}
	durations, err := wide.RunDurations(ctx, among, b, bins)
	if err != nil {
		return nil, statsRunsBucket{}, nil, err
	}
	histogram := make([]statsBin, len(durations))
	for i, d := range durations {
		histogram[i] = statsBin{FromMS: d.From, UntilMS: d.Until, Runs: d.Runs}
	}
	return buckets, overall, histogram, nil
}

// runCounts is runs counted by state, as the API writes them.
func runCounts(runs map[string]int) statsRunCounts {
	return statsRunCounts{
		Queued: runs["queued"], Running: runs["running"], Waiting: runs["waiting"],
		Succeeded: runs["succeeded"], Failed: runs["failed"], Cancelled: runs["cancelled"],
		TimedOut: runs["timed_out"],
	}
}

// runBuckets is what the runs came to in each of b's buckets, as the API writes it.
func runBuckets(counted []db.RunBucket, b db.Buckets) []statsRunsBucket {
	buckets := make([]statsRunsBucket, len(counted))
	for i, c := range counted {
		since := b.First.Add(time.Duration(i) * b.Width)
		buckets[i] = statsRunsBucket{
			Since:     stamp(since),
			Until:     stamp(since.Add(b.Width - time.Nanosecond)),
			Runs:      runCounts(c.Runs),
			Duration:  percentiles(c.Duration),
			QueueWait: percentiles(c.QueueWait),
			Retries:   make([]statsExitCode, len(c.Retries)),
		}
		for j, r := range c.Retries {
			buckets[i].Retries[j] = statsExitCode{ExitCode: r.ExitCode, Attempts: r.Attempts}
		}
	}
	return buckets
}

func percentiles(p db.Percentiles) *statsPercentiles {
	if !p.Taken {
		return nil
	}
	return &statsPercentiles{P50: p.P50, P95: p.P95, P99: p.P99}
}

// statsRuns is what GET /api/v1/{ns}/stats/runs answers, as openapi.json describes it field by
// field.
type statsRuns struct {
	From     string            `json:"from"`
	To       string            `json:"to"`
	Bucket   string            `json:"bucket"`
	Workflow string            `json:"workflow,omitempty"`
	Buckets  []statsRunsBucket `json:"buckets"`
	// The range as one bucket, which a figure beside the charts reads.
	Overall statsRunsBucket `json:"overall"`
	// Present, and empty where no run has ended, wherever histogram was asked for.
	Histogram []statsBin       `json:"histogram,omitzero"`
	Previous  *statsRunsBefore `json:"previous,omitempty"`
}

type statsRunsBefore struct {
	From      string            `json:"from"`
	To        string            `json:"to"`
	Buckets   []statsRunsBucket `json:"buckets"`
	Overall   statsRunsBucket   `json:"overall"`
	Histogram []statsBin        `json:"histogram,omitzero"`
}

type statsRunsBucket struct {
	Since     string            `json:"since"`
	Until     string            `json:"until"`
	Runs      statsRunCounts    `json:"runs"`
	Duration  *statsPercentiles `json:"duration_ms,omitempty"`
	QueueWait *statsPercentiles `json:"queue_wait_ms,omitempty"`
	Retries   []statsExitCode   `json:"retries"`
}

// statsRunCounts are the runs of a bucket by state, in the order the documentation names them.
type statsRunCounts struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Waiting   int `json:"waiting"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	TimedOut  int `json:"timed_out"`
}

type statsPercentiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

type statsExitCode struct {
	ExitCode *int `json:"exit_code"`
	Attempts int  `json:"attempts"`
}

type statsBin struct {
	FromMS  int64 `json:"from_ms"`
	UntilMS int64 `json:"until_ms"`
	Runs    int   `json:"runs"`
}

// wantsCSV says whether an Accept header ranks text/csv above application/json. Each is given the
// weight of the most specific range naming it, as RFC 9110 weighs them, and JSON is answered where
// they tie, where neither is named and where there is no Accept at all, as every other route of the
// API answers.
func wantsCSV(accept string) bool {
	if accept == "" {
		return false
	}
	weigh := func(kind, sub string) float64 {
		q, specific := 0.0, -1
		for _, part := range strings.Split(accept, ",") {
			media, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			level := -1
			switch media {
			case kind + "/" + sub:
				level = 2
			case kind + "/*":
				level = 1
			case "*/*":
				level = 0
			}
			if level <= specific {
				continue
			}
			weight := 1.0
			if written, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(written, 64); err == nil && f >= 0 && f <= 1 {
					weight = f
				}
			}
			q, specific = weight, level
		}
		return q
	}
	csv := weigh("text", "csv")
	return csv > 0 && csv > weigh("application", "json")
}

// runsCSVHeader is the header row of the CSV a series is answered in: "one row a bucket and a column
// a figure".
var runsCSVHeader = []string{
	"period", "since", "until",
	"queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out",
	"duration_p50_ms", "duration_p95_ms", "duration_p99_ms",
	"queue_wait_p50_ms", "queue_wait_p95_ms", "queue_wait_p99_ms",
	"retried",
}

// writeRunsCSV answers a series in RFC 4180: a header row, then a row a bucket, the span before
// after the range where compare=previous added it, then each span as one bucket, under period
// overall and previous_overall. A figure the JSON leaves out is an empty field, retried is the
// attempts retried whatever their exit code, and the histogram, which is no series of buckets, is
// not in it.
func writeRunsCSV(w http.ResponseWriter, out statsRuns) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	c.Write(runsCSVHeader)
	rows := func(period string, buckets []statsRunsBucket) {
		for _, b := range buckets {
			retried := 0
			for _, r := range b.Retries {
				retried += r.Attempts
			}
			row := []string{period, b.Since, b.Until}
			for _, n := range []int{b.Runs.Queued, b.Runs.Running, b.Runs.Waiting, b.Runs.Succeeded, b.Runs.Failed, b.Runs.Cancelled, b.Runs.TimedOut} {
				row = append(row, strconv.Itoa(n))
			}
			for _, p := range []*statsPercentiles{b.Duration, b.QueueWait} {
				if p == nil {
					row = append(row, "", "", "")
					continue
				}
				row = append(row, strconv.FormatInt(p.P50, 10), strconv.FormatInt(p.P95, 10), strconv.FormatInt(p.P99, 10))
			}
			c.Write(append(row, strconv.Itoa(retried)))
		}
	}
	rows("current", out.Buckets)
	if out.Previous != nil {
		rows("previous", out.Previous.Buckets)
	}
	rows("overall", []statsRunsBucket{out.Overall})
	if out.Previous != nil {
		rows("previous_overall", []statsRunsBucket{out.Previous.Overall})
	}
	c.Flush()
}
