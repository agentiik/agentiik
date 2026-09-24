package driver

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// reading is a sample of what a container has consumed, as the daemon writes one.
func reading(cpu time.Duration, usage, inactive uint64) docker.Stats {
	return docker.Stats{
		Read:   time.Now().UTC(),
		CPU:    docker.CPUStats{Usage: docker.CPUUsage{Total: uint64(cpu)}},
		Memory: docker.MemoryStats{Usage: usage, Stats: map[string]uint64{"inactive_file": inactive}},
	}
}

// statsDaemon answers the statistics of every container as a daemon does: once with the
// one-shot sample where there is one and nothing otherwise, and on a stream with the
// samples given, then, once served is closed, a sample carrying nothing at every
// collection, which is what a daemon writes after the exit. served is closed once the
// samples are on the wire, which is what a container waits on before it exits, so that
// they are read while it runs, as a daemon reads them.
func statsDaemon(r *runner, served chan struct{}, oneShot *docker.Stats, samples ...docker.Stats) {
	r.daemon.Handle("GET", "/containers/{id}/stats", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if req.URL.Query().Get("stream") == "false" {
			if oneShot != nil {
				enc.Encode(oneShot)
			} else {
				enc.Encode(docker.Stats{})
			}
			return
		}
		flusher := w.(http.Flusher)
		for _, s := range samples {
			enc.Encode(s)
		}
		flusher.Flush()
		close(served)
		for {
			if enc.Encode(docker.Stats{}) != nil {
				return
			}
			flusher.Flush()
			select {
			case <-req.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}

// waitFor is a container that exits with code once the statistics it is sampled with
// have been served.
func waitFor(served <-chan struct{}, code int, then func(dockertest.Container) error) func(dockertest.Container) (int, error) {
	return func(c dockertest.Container) (int, error) {
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			return 1, errors.New("the statistics were never asked for while the container ran")
		}
		if then != nil {
			return code, then(c)
		}
		return code, nil
	}
}

// ending is the terminal event the observer was told.
func (r *runner) ending(t *testing.T) Event {
	t.Helper()
	r.observed.mu.Lock()
	defer r.observed.mu.Unlock()
	for i := len(r.observed.es) - 1; i >= 0; i-- {
		if e := r.observed.es[i]; e.State.Terminal() {
			return e
		}
	}
	t.Fatalf("the observer was never told the task ended: %v", r.observed.es)
	return Event{}
}

// What a container consumed reaches the terminal event, sampled while it ran: the last
// processor total, and the highest memory any sample saw less the page cache it had
// stopped using. It is carried on every ending of a container that ran, a failure and a
// refusal of its outputs included, since those are the endings an author reads the
// figures for.
func TestUsageIsSampledFromTheDaemonWhileTheContainerRuns(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	samples := []docker.Stats{
		reading(1500*time.Millisecond, 80<<20, 20<<20),
		reading(2500*time.Millisecond, 50<<20, 0),
	}

	for _, c := range []struct {
		name  string
		code  int
		state agk.TaskState
		then  func(dockertest.Container) error
		limit func(*Config)
	}{
		{name: "a success", state: agk.TaskSucceeded, then: func(c dockertest.Container) error {
			return wrote(c, "out", agk.NewItem(map[string]any{"ok": true}))
		}},
		{name: "a failure", code: 3, state: agk.TaskFailed},
		{name: "outputs refused", state: agk.TaskFailed, then: func(c dockertest.Container) error {
			return wrote(c, "out", agk.NewItem(map[string]any{"page": 1}), agk.NewItem(map[string]any{"page": 2}))
		}, limit: func(cfg *Config) {
			cfg.Limits = agk.DefaultLimits()
			cfg.Limits.MaxItems = 1
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			served := make(chan struct{})
			r := newRunner(t, oneImage(ref, goodManifest), waitFor(served, c.code, c.then))
			if c.limit != nil {
				r = reopen(t, r, c.limit)
			}
			statsDaemon(r, served, nil, samples...)

			r.Run(t.Context(), oneTask(ref))

			e := r.ending(t)
			if e.State != c.state {
				t.Fatalf("the task ended %s, want %s", e.State, c.state)
			}
			if e.Usage.CPUSeconds != 2.5 {
				t.Errorf("the task spent %v CPU seconds, and the last total the daemon read was 2.5", e.Usage.CPUSeconds)
			}
			if e.Usage.MaxRSSBytes != 60<<20 {
				t.Errorf("the task's peak is %d bytes, and the highest sample less its inactive page cache is %d", e.Usage.MaxRSSBytes, 60<<20)
			}
		})
	}
}

// A container that exits before the stream's first collection reports the one sample
// read as it started, and one of which no sample was read at all reports neither figure:
// what the daemon writes once a container has exited carries nothing, and it is never
// read as a container that consumed nothing.
func TestAContainerGoneBeforeTheFirstCollectionReportsTheSampleNowOrNothing(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest

	t.Run("the sample now", func(t *testing.T) {
		served := make(chan struct{})
		r := newRunner(t, oneImage(ref, goodManifest), waitFor(served, 0, nil))
		now := reading(40*time.Millisecond, 3<<20, 1<<20)
		statsDaemon(r, served, &now)

		if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
			t.Fatalf("running: %v", err)
		}
		u := r.ending(t).Usage
		if u.CPUSeconds != 0.04 || u.MaxRSSBytes != 2<<20 {
			t.Errorf("the usage is %+v, and the one sample read was 0.04 CPU seconds and %d bytes", u, 2<<20)
		}
	})

	t.Run("nothing", func(t *testing.T) {
		// The fake daemon's own statistics: it has no cgroup to read, and answers as a
		// daemon answers for a container that has exited.
		r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })

		if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
			t.Fatalf("running: %v", err)
		}
		if u := r.ending(t).Usage; u.CPUSeconds != 0 || u.MaxRSSBytes != 0 {
			t.Errorf("the usage is %+v, and no sample was ever read", u)
		}
	})

	t.Run("a daemon refusing the statistics", func(t *testing.T) {
		r := newRunner(t, oneImage(ref, goodManifest), func(dockertest.Container) (int, error) { return 0, nil })
		r.daemon.Handle("GET", "/containers/{id}/stats", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"cgroup unreadable"}`, http.StatusInternalServerError)
		})

		result, err := r.Run(t.Context(), oneTask(ref))
		if err != nil || result.State != agk.TaskSucceeded {
			t.Fatalf("a task whose figures could not be read ended %s: %v", result.State, err)
		}
		if u := r.ending(t).Usage; u.CPUSeconds != 0 || u.MaxRSSBytes != 0 {
			t.Errorf("the usage is %+v, and the daemon refused every sample", u)
		}
	})
}

// A pull that actually happened reports how long it took on the terminal event, and
// never 0, which is what the usage block says of an image the host already held.
func TestAPullThatHappenedIsReportedOnTheEnding(t *testing.T) {
	const ref = "ghcr.io/agentiik/http-request@" + imageDigest
	images := oneImage(ref, goodManifest)
	cold := images[ref]
	cold.Remote = true
	images[ref] = cold
	r := newRunner(t, images, func(dockertest.Container) (int, error) { return 0, nil })

	if _, err := r.Run(t.Context(), oneTask(ref)); err != nil {
		t.Fatalf("running: %v", err)
	}
	if got := r.ending(t).Usage.ImagePullMS; got <= 0 {
		t.Errorf("the image was pulled and the ending says the pull took %dms", got)
	}
}
