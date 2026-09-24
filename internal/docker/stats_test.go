package docker_test

import (
	"net/http"
	"testing"

	"github.com/agentiik/agentiik/internal/docker"
	"github.com/agentiik/agentiik/internal/dockertest"
)

// A sample as Docker 29.8 writes it on cgroup v2, trimmed of the members nothing reads
// but keeping enough of them that the decoding is shown to ignore what it does not name.
const sampleV2 = `{"id":"0edefadc699c","name":"/confident_napier","os_type":"linux","read":"2026-09-24T17:59:47.479030501Z","preread":"0001-01-01T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":41833000,"usage_in_kernelmode":10987000,"usage_in_usermode":30846000},"system_cpu_usage":176459960000000,"online_cpus":8},"memory_stats":{"usage":1556480,"stats":{"active_anon":65536,"anon":65536,"file":8192,"inactive_file":4096},"limit":8217473024}}`

// What a daemon writes for a container it has nothing to read for, one that has exited
// among them.
const sampleEmpty = `{"read":"0001-01-01T00:00:00Z","preread":"0001-01-01T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":0},"throttling_data":{}},"memory_stats":{}}`

// The stream is read one sample at a time, in the order the daemon wrote them, and says
// so once it has ended.
func TestTheStatisticsStreamIsReadSampleBySample(t *testing.T) {
	client, daemon := dial(t)
	var asked string
	daemon.Handle("GET", "/containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("stream")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleV2 + "\n" + sampleEmpty + "\n"))
	})

	stream, err := client.ContainerStats(ctxOf(t), "0edefadc699c")
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer stream.Close()
	if asked != "true" {
		t.Errorf("the stream was asked for with stream=%q", asked)
	}

	first, err := stream.Next()
	if err != nil {
		t.Fatalf("reading the first sample: %v", err)
	}
	if !first.Sampled() {
		t.Errorf("a sample the daemon read reads as one it had nothing to read for: %+v", first)
	}
	if first.CPU.Usage.Total != 41833000 {
		t.Errorf("the processor total is %d nanoseconds, and the daemon wrote 41833000", first.CPU.Usage.Total)
	}
	if got := first.Memory.Resident(); got != 1556480-4096 {
		t.Errorf("the resident set is %d, and it is the usage less the inactive page cache, %d", got, 1556480-4096)
	}

	second, err := stream.Next()
	if err != nil {
		t.Fatalf("reading the second sample: %v", err)
	}
	if second.Sampled() {
		t.Errorf("a sample carrying nothing reads as a reading: %+v", second)
	}

	if _, err := stream.Next(); err == nil {
		t.Error("a stream the daemon ended answered another sample")
	}
}

// A sample now is asked for as one, since without one-shot the daemon waits out two of
// its collections before it answers.
func TestASampleNowIsAskedForAsAOneShot(t *testing.T) {
	client, daemon := dial(t)
	var stream, oneShot string
	daemon.Handle("GET", "/containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		stream, oneShot = r.URL.Query().Get("stream"), r.URL.Query().Get("one-shot")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleV2))
	})

	got, err := client.ContainerStatsOnce(ctxOf(t), "0edefadc699c")
	if err != nil {
		t.Fatalf("reading one sample: %v", err)
	}
	if stream != "false" || oneShot != "true" {
		t.Errorf("the sample was asked for with stream=%q and one-shot=%q", stream, oneShot)
	}
	if !got.Sampled() || got.CPU.Usage.Total != 41833000 {
		t.Errorf("the sample reads as %+v", got)
	}
}

// The resident set leaves out the page cache the container has stopped using, under
// whichever name the cgroup version gives it, and never goes below nothing.
func TestTheResidentSetLeavesOutTheInactivePageCache(t *testing.T) {
	for _, c := range []struct {
		name string
		mem  docker.MemoryStats
		want uint64
	}{
		{"cgroup v2", docker.MemoryStats{Usage: 100 << 20, Stats: map[string]uint64{"inactive_file": 30 << 20}}, 70 << 20},
		{"cgroup v1", docker.MemoryStats{Usage: 100 << 20, Stats: map[string]uint64{"total_inactive_file": 30 << 20, "inactive_file": 10 << 20}}, 70 << 20},
		{"a counter above the usage", docker.MemoryStats{Usage: 10 << 20, Stats: map[string]uint64{"inactive_file": 30 << 20}}, 10 << 20},
		{"a cgroup v1 counter above the usage", docker.MemoryStats{Usage: 10 << 20, Stats: map[string]uint64{"total_inactive_file": 30 << 20}}, 10 << 20},
		{"a cgroup v1 counter above the usage beside the other", docker.MemoryStats{Usage: 10 << 20, Stats: map[string]uint64{"total_inactive_file": 30 << 20, "inactive_file": 4 << 20}}, 6 << 20},
		{"no breakdown", docker.MemoryStats{Usage: 10 << 20}, 10 << 20},
	} {
		if got := c.mem.Resident(); got != c.want {
			t.Errorf("%s: the resident set is %d, want %d", c.name, got, c.want)
		}
	}
}

// The fake daemon has no cgroup to read a container's figures from, and answers as a
// daemon answers for a container it has nothing to read for: samples carrying nothing,
// and a 404 for a container it does not hold.
func TestTheFakeDaemonMeasuresNothing(t *testing.T) {
	client, _ := dial(t, dockertest.With(dockertest.Options{
		Images: map[string]dockertest.Image{"brick": {Digest: digest}},
	}))
	created, err := client.ContainerCreate(ctxOf(t), "", docker.Config{Image: "brick"}, docker.HostConfig{}, docker.NetworkingConfig{})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	once, err := client.ContainerStatsOnce(ctxOf(t), created.ID)
	if err != nil {
		t.Fatalf("reading one sample: %v", err)
	}
	if once.Sampled() {
		t.Errorf("a daemon with nothing to measure answered a reading: %+v", once)
	}

	stream, err := client.ContainerStats(ctxOf(t), created.ID)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer stream.Close()
	for range 2 {
		s, err := stream.Next()
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if s.Sampled() {
			t.Errorf("a daemon with nothing to measure streamed a reading: %+v", s)
		}
	}

	if _, err := client.ContainerStatsOnce(ctxOf(t), "nothing-here"); !docker.IsNotFound(err) {
		t.Errorf("the statistics of a container the daemon does not hold answered %v, and a daemon answers 404", err)
	}
}
