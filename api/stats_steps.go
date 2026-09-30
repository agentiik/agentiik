package api

import (
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/version"
)

// stepStatistics answers GET /api/v1/{ns}/stats/steps: the steps of the one workflow the query names,
// bucket by bucket or, with by=hour, by the hour of the week, in JSON or, to Accept: text/csv, in CSV.
//
// A step belongs to one workflow, so the route reads one, and answers a workflow the caller does not
// hold run:read on exactly as one that does not exist, having asked about it all the same: "no
// matching grant means refusal: the same 404 whether the workflow is absent or merely invisible".
func (s *Server) stepStatistics(w http.ResponseWriter, r *http.Request, who Principal, within Target, _ Holds) {
	query := r.URL.Query()
	workflow := query.Get("workflow")
	if workflow == "" {
		fail(w, http.StatusBadRequest, "workflow is missing, and a step belongs to one workflow: this route reads the steps of the one the query names")
		return
	}
	by := query.Get("by")
	if by != "" && by != "hour" {
		fail(w, http.StatusBadRequest, "by is "+strconv.Quote(by)+", and the one way a step's durations are laid out otherwise than in buckets is hour, by the hour of the week")
		return
	}
	rng, err := readStatsRange(query, s.now(), by == "")
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var among []db.Workflow
	if storable(within.Namespace) && storable(workflow) {
		var ok bool
		if among, ok = s.readable(w, r, within.Namespace, workflow, "the statistics could not be read"); !ok {
			return
		}
	}
	if len(among) != 1 {
		// Not there, or not the caller's to read, which are answered alike.
		refuse(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	of := among[0]

	order, err := s.stepOrder(r.Context(), of)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}
	out := statsSteps{From: stamp(rng.From), To: stamp(rng.To), Bucket: rng.Bucket, By: by, Workflow: workflow}
	err = s.pool.Installation(r.Context(), db.RunListing, func(ctx context.Context, wide *db.Wide) error {
		if by == "hour" {
			hours, err := wide.StepHours(ctx, of, rng.From, rng.To)
			if err != nil {
				return err
			}
			for _, step := range stepsIn(order, hours) {
				out.Steps = append(out.Steps, statsStep{Step: step, Hours: hoursOf(hours[step])})
			}
			return nil
		}
		current, err := wide.StepStatistics(ctx, of, rng.Buckets)
		if err != nil {
			return err
		}
		var previous map[string][]db.StepBucket
		_, _, before := rng.before()
		if rng.Previous {
			if previous, err = wide.StepStatistics(ctx, of, before); err != nil {
				return err
			}
		}
		for _, step := range stepsIn(order, current, previous) {
			st := statsStep{Step: step, Buckets: stepBuckets(current[step], rng.Buckets)}
			if rng.Previous {
				st.Previous = stepBuckets(previous[step], before)
			}
			out.Steps = append(out.Steps, st)
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}
	if out.Steps == nil {
		out.Steps = []statsStep{}
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writeStepsCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

// stepOrder is the steps of a workflow in the order its latest version runs them, upstream first and
// those no edge orders by name, which is graph.Graph.Order: the order a reader of the workflow meets
// them in. The graph keeps no order of declaration, a step being a key of a map. The latest version
// is the head of the default branch, or, for a workflow no git push has filled, the last version a
// tree push recorded. Nothing where there is none, or where it builds no graph, as a library's does:
// the steps are then the ones the range's runs ran, by name.
func (s *Server) stepOrder(ctx context.Context, of db.Workflow) ([]string, error) {
	var v db.Version
	latest := false
	err := s.pool.In(ctx, of.Namespace, func(ctx context.Context, n *db.NS) error {
		rec, err := n.WorkflowRecord(ctx, of.Name)
		if err != nil {
			return err
		}
		commit := rec.Head
		if commit == "" {
			pushed, _, err := n.TreeVersions(ctx, of.Name, "", 1)
			if err != nil || len(pushed) == 0 {
				return err
			}
			commit = pushed[0].Commit
		}
		latest = true
		v, err = n.Version(ctx, of.Name, commit)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoWorkflow), errors.Is(err, db.ErrNoVersion):
		// Deleted or moved since it was found, or a head recorded before its version.
		return nil, nil
	case err != nil:
		return nil, err
	case !latest:
		return nil, nil
	}
	g, err := version.Build(v)
	if err != nil {
		return nil, nil
	}
	order := make([]string, 0, len(g.Order()))
	for _, step := range g.Order() {
		order = append(order, string(step))
	}
	return order, nil
}

// stepsIn is order, then every step the series counted that order does not name, by name: a step
// the head no longer declares, which runs of an older version ran within the range.
func stepsIn[T any](order []string, counted ...map[string]T) []string {
	out := slices.Clone(order)
	var more []string
	for _, c := range counted {
		for step := range c {
			if !slices.Contains(out, step) && !slices.Contains(more, step) {
				more = append(more, step)
			}
		}
	}
	slices.Sort(more)
	return append(out, more...)
}

// stepBuckets is what one step came to in each of b's buckets, those it ran nothing in included.
func stepBuckets(counted []db.StepBucket, b db.Buckets) []statsStepBucket {
	out := make([]statsStepBucket, b.Count)
	for i := range out {
		since := b.First.Add(time.Duration(i) * b.Width)
		out[i] = statsStepBucket{Since: stamp(since), Until: stamp(since.Add(b.Width - time.Nanosecond)), ExitCodes: []statsExitCode{}}
		if i >= len(counted) {
			continue
		}
		c := counted[i]
		out[i].Attempts, out[i].Duration, out[i].ItemsPerMinute = c.Attempts, percentiles(c.Duration), c.ItemsPerMinute
		for _, e := range c.ExitCodes {
			out[i].ExitCodes = append(out[i].ExitCodes, statsExitCode{ExitCode: e.ExitCode, Attempts: e.Attempts})
		}
	}
	return out
}

// hoursOf is the 168 cells of a week, Monday at midnight first, each with its median where
// something ran in it.
func hoursOf(medians []db.Percentiles) []statsStepHour {
	out := make([]statsStepHour, db.HoursOfTheWeek)
	for i := range out {
		out[i] = statsStepHour{Weekday: i/24 + 1, Hour: i % 24}
		if i < len(medians) && medians[i].Taken {
			out[i].Duration = &statsMedian{P50: medians[i].P50}
		}
	}
	return out
}

// statsSteps is what GET /api/v1/{ns}/stats/steps answers, as openapi.json describes it field by
// field.
type statsSteps struct {
	From     string      `json:"from"`
	To       string      `json:"to"`
	Bucket   string      `json:"bucket,omitempty"`
	By       string      `json:"by,omitempty"`
	Workflow string      `json:"workflow"`
	Steps    []statsStep `json:"steps"`
}

type statsStep struct {
	Step     string            `json:"step"`
	Buckets  []statsStepBucket `json:"buckets,omitzero"`
	Hours    []statsStepHour   `json:"hours,omitzero"`
	Previous []statsStepBucket `json:"previous,omitzero"`
}

type statsStepBucket struct {
	Since          string            `json:"since"`
	Until          string            `json:"until"`
	Duration       *statsPercentiles `json:"duration_ms,omitempty"`
	Attempts       int               `json:"attempts"`
	ExitCodes      []statsExitCode   `json:"exit_codes"`
	ItemsPerMinute *float64          `json:"items_per_minute,omitempty"`
}

type statsStepHour struct {
	Weekday  int          `json:"weekday"`
	Hour     int          `json:"hour"`
	Duration *statsMedian `json:"duration_ms,omitempty"`
}

type statsMedian struct {
	P50 int64 `json:"p50"`
}

// writeStepsCSV answers the steps' series in RFC 4180: a row a step and a bucket, every step's
// buckets in turn and the span before after them where compare=previous added it; with by=hour, a
// row a step, a weekday and an hour. A figure the JSON leaves out is an empty field, and the exit
// codes, which are no one figure, are not in it.
func writeStepsCSV(w http.ResponseWriter, out statsSteps) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	defer c.Flush()
	if out.By == "hour" {
		c.Write([]string{"period", "step", "weekday", "hour", "duration_p50_ms"})
		for _, st := range out.Steps {
			for _, h := range st.Hours {
				p50 := ""
				if h.Duration != nil {
					p50 = strconv.FormatInt(h.Duration.P50, 10)
				}
				c.Write([]string{"current", st.Step, strconv.Itoa(h.Weekday), strconv.Itoa(h.Hour), p50})
			}
		}
		return
	}
	c.Write([]string{"period", "step", "since", "until", "attempts", "duration_p50_ms", "duration_p95_ms", "duration_p99_ms", "items_per_minute"})
	rows := func(period string, of func(statsStep) []statsStepBucket) {
		for _, st := range out.Steps {
			for _, b := range of(st) {
				row := []string{period, st.Step, b.Since, b.Until, strconv.Itoa(b.Attempts), "", "", ""}
				if b.Duration != nil {
					row[5], row[6], row[7] = strconv.FormatInt(b.Duration.P50, 10), strconv.FormatInt(b.Duration.P95, 10), strconv.FormatInt(b.Duration.P99, 10)
				}
				perMinute := ""
				if b.ItemsPerMinute != nil {
					perMinute = strconv.FormatFloat(*b.ItemsPerMinute, 'f', -1, 64)
				}
				c.Write(append(row, perMinute))
			}
		}
	}
	rows("current", func(st statsStep) []statsStepBucket { return st.Buckets })
	rows("previous", func(st statsStep) []statsStepBucket { return st.Previous })
}
