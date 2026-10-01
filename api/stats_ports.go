package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// portStatistics answers GET /api/v1/{ns}/stats/ports: what each step of the one workflow the query
// names published, port by port, bucket by bucket, in JSON or, to Accept: text/csv, in CSV.
//
// run:read and never run:read_data: the items of an envelope are its contents, which run:read does
// not see, and what is counted is the count its step's row records, "from each envelope's meta.count
// and never its items".
func (s *Server) portStatistics(w http.ResponseWriter, r *http.Request, who Principal, within Target, _ Holds) {
	query := r.URL.Query()
	workflow := query.Get("workflow")
	if workflow == "" {
		fail(w, http.StatusBadRequest, "workflow is missing, and a step belongs to one workflow: this route reads the steps of the one the query names")
		return
	}
	rng, err := readStatsRange(query, s.now(), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	of, ok := s.oneWorkflow(w, r, within.Namespace, workflow)
	if !ok {
		return
	}
	latest, err := s.latestGraph(r.Context(), of)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}

	out := statsPorts{From: stamp(rng.From), To: stamp(rng.To), Bucket: rng.Bucket, Workflow: workflow, Steps: []statsPortsStep{}}
	err = s.pool.Installation(r.Context(), db.RunListing, func(ctx context.Context, wide *db.Wide) error {
		current, err := wide.PortStatistics(ctx, of, rng.Buckets)
		if err != nil {
			return err
		}
		var previous map[string][]map[string]int64
		_, _, before := rng.before()
		if rng.Previous {
			if previous, err = wide.PortStatistics(ctx, of, before); err != nil {
				return err
			}
		}
		for _, step := range stepsIn(stepOrder(latest), current, previous) {
			ports := portsOf(latest, step, current[step], previous[step])
			st := statsPortsStep{Step: step, Ports: ports, Buckets: portBuckets(current[step], ports, rng.Buckets)}
			if rng.Previous {
				st.Previous = portBuckets(previous[step], ports, before)
			}
			out.Steps = append(out.Steps, st)
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writePortsCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

// portsOf is the output ports of one step, those the latest version declares in the order it
// declares them, then, by name, any other a run in the range published on: a port an older version
// declared. "Every port the step declares is named, 0 where it published nothing."
func portsOf(latest *graph.Graph, step string, counted ...[]map[string]int64) []string {
	var ports []string
	if latest != nil {
		if st, ok := latest.Step(agk.Step(step)); ok {
			for _, port := range st.Outputs {
				ports = append(ports, string(port))
			}
		}
	}
	var more []string
	for _, c := range counted {
		for _, bucket := range c {
			for port := range bucket {
				if !slices.Contains(ports, port) && !slices.Contains(more, port) {
					more = append(more, port)
				}
			}
		}
	}
	slices.Sort(more)
	return append(ports, more...)
}

// portBuckets is what one step published on each of ports in each of b's buckets, 0 where it
// published nothing.
func portBuckets(counted []map[string]int64, ports []string, b db.Buckets) []statsPortBucket {
	out := make([]statsPortBucket, b.Count)
	for i := range out {
		since := b.First.Add(time.Duration(i) * b.Width)
		out[i] = statsPortBucket{Since: stamp(since), Until: stamp(since.Add(b.Width - time.Nanosecond)), Items: make(map[string]int64, len(ports))}
		for _, port := range ports {
			if i < len(counted) {
				out[i].Items[port] = counted[i][port]
			} else {
				out[i].Items[port] = 0
			}
		}
	}
	return out
}

// statsPorts is what GET /api/v1/{ns}/stats/ports answers, as openapi.json describes it field by
// field.
type statsPorts struct {
	From     string           `json:"from"`
	To       string           `json:"to"`
	Bucket   string           `json:"bucket"`
	Workflow string           `json:"workflow"`
	Steps    []statsPortsStep `json:"steps"`
}

type statsPortsStep struct {
	Step     string            `json:"step"`
	Buckets  []statsPortBucket `json:"buckets"`
	Previous []statsPortBucket `json:"previous,omitzero"`

	// Ports are the step's ports in the order the CSV writes them, which a JSON object has none of.
	Ports []string `json:"-"`
}

type statsPortBucket struct {
	Since string           `json:"since"`
	Until string           `json:"until"`
	Items map[string]int64 `json:"items"`
}

// writePortsCSV answers what the steps published in RFC 4180: a row a step, a port and a bucket, in
// that order, the span before after them where compare=previous added it.
func writePortsCSV(w http.ResponseWriter, out statsPorts) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	defer c.Flush()
	c.Write([]string{"period", "step", "port", "since", "until", "items"})
	rows := func(period string, of func(statsPortsStep) []statsPortBucket) {
		for _, st := range out.Steps {
			for _, port := range st.Ports {
				for _, b := range of(st) {
					c.Write([]string{period, st.Step, port, b.Since, b.Until, strconv.FormatInt(b.Items[port], 10)})
				}
			}
		}
	}
	rows("current", func(st statsPortsStep) []statsPortBucket { return st.Buckets })
	rows("previous", func(st statsPortsStep) []statsPortBucket { return st.Previous })
}
