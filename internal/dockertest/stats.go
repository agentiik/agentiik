package dockertest

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// statsInterval is how often a stream of statistics is written. A daemon collects once a
// second; this one has nothing to collect, and writes more often so that a client reading
// the stream to the sample after an exit is not kept waiting a second per test.
const statsInterval = 10 * time.Millisecond

// containerStats answers GET /containers/{id}/stats the way a daemon answers it for a
// container whose cgroup it has nothing to read from: with a sample carrying nothing,
// once for a one-shot and at every collection for a stream, until the container is
// removed. Here that is every container, since a container is a function with no cgroup,
// so what a driver makes of a daemon that measured nothing is what every task shows. A
// test that needs figures replaces the route with Handle and writes them.
func (d *Daemon) containerStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.container(id) == nil {
		writeError(w, http.StatusNotFound, "No such container: "+id)
		return
	}
	if r.URL.Query().Get("stream") == "false" {
		writeJSON(w, http.StatusOK, docker.Stats{})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	tick := time.NewTicker(statsInterval)
	defer tick.Stop()
	for {
		if err := enc.Encode(docker.Stats{}); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		// The stream ends with the container, as a daemon ends it once the container
		// is removed.
		if d.container(id) == nil {
			return
		}
	}
}
