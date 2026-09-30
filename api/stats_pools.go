package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/agentiik/agentiik/db"
)

// poolStatistics answers GET /api/v1/stats/pools, to administrators alone: every pool's slots and
// each of its runners', bucket by bucket, and every silence between a runner's heartbeats over the
// range, in JSON or, to Accept: text/csv, in CSV. A runner's inventory is the installation's rather
// than a namespace's, and so is what it did.
//
// Served at this path alone, since its word, stats, was a namespace's name a user could have taken
// before v0.3.0 reserved it: a route under /api/v1/{namespace} would have taken that namespace's.
// The span before is not taken: what a chart of the pools compares a runner with is its capacity.
func (s *RunnerAPI) poolStatistics(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	rng, err := readStatsRange(r.URL.Query(), s.now(), true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var pools []db.PoolSeries
	err = s.pool.Installation(r.Context(), db.RunnerInventory, func(ctx context.Context, wide *db.Wide) error {
		var err error
		pools, err = wide.PoolStatistics(ctx, rng.Buckets)
		return err
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "the statistics could not be read")
		return
	}
	out := statsPools{From: stamp(rng.From), To: stamp(rng.To), Bucket: rng.Bucket, Pools: make([]statsPool, len(pools))}
	for i, p := range pools {
		out.Pools[i] = statsPool{Pool: p.Pool, Buckets: slotBuckets(p.Buckets, rng.Buckets), Runners: make([]statsPoolRunner, len(p.Runners))}
		for j, rn := range p.Runners {
			silences := make([]statsSilence, len(rn.Silences))
			for k, sl := range rn.Silences {
				silences[k] = statsSilence{At: stamp(sl.At), LengthMS: sl.Length.Milliseconds(), TasksLost: sl.TasksLost}
			}
			out.Pools[i].Runners[j] = statsPoolRunner{Runner: rn.Runner, Buckets: slotBuckets(rn.Buckets, rng.Buckets), Silences: silences}
		}
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if wantsCSV(r.Header.Get("Accept")) {
		writePoolsCSV(w, out)
		return
	}
	write(w, http.StatusOK, out)
}

func slotBuckets(counted []db.SlotBucket, b db.Buckets) []statsSlotBucket {
	out := make([]statsSlotBucket, len(counted))
	for i, c := range counted {
		since := b.First.Add(time.Duration(i) * b.Width)
		out[i] = statsSlotBucket{Since: stamp(since), Until: stamp(since.Add(b.Width - time.Nanosecond)), InUseMax: c.InUseMax, Capacity: c.Capacity}
	}
	return out
}

// statsPools is what GET /api/v1/stats/pools answers, as openapi.json describes it field by field.
type statsPools struct {
	From   string      `json:"from"`
	To     string      `json:"to"`
	Bucket string      `json:"bucket"`
	Pools  []statsPool `json:"pools"`
}

type statsPool struct {
	Pool    string            `json:"pool"`
	Buckets []statsSlotBucket `json:"buckets"`
	Runners []statsPoolRunner `json:"runners"`
}

type statsPoolRunner struct {
	Runner   string            `json:"runner"`
	Buckets  []statsSlotBucket `json:"buckets"`
	Silences []statsSilence    `json:"silences"`
}

type statsSlotBucket struct {
	Since    string `json:"since"`
	Until    string `json:"until"`
	InUseMax int    `json:"slots_in_use_max"`
	Capacity int64  `json:"capacity"`
}

type statsSilence struct {
	At        string `json:"at"`
	LengthMS  int64  `json:"length_ms"`
	TasksLost int    `json:"tasks_lost"`
}

// writePoolsCSV answers the pools' slots in RFC 4180: a row a pool and a bucket, its runner empty,
// then a row each of its runners and a bucket. The silences, which are no table of buckets, are not
// in it.
func writePoolsCSV(w http.ResponseWriter, out statsPools) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8; header=present")
	w.WriteHeader(http.StatusOK)
	c := csv.NewWriter(w)
	c.UseCRLF = true
	defer c.Flush()
	c.Write([]string{"pool", "runner", "since", "until", "slots_in_use_max", "capacity"})
	rows := func(pool, runner string, buckets []statsSlotBucket) {
		for _, b := range buckets {
			c.Write([]string{pool, runner, b.Since, b.Until, strconv.Itoa(b.InUseMax), strconv.FormatInt(b.Capacity, 10)})
		}
	}
	for _, p := range out.Pools {
		rows(p.Pool, "", p.Buckets)
		for _, r := range p.Runners {
			rows(p.Pool, r.Runner, r.Buckets)
		}
	}
}
