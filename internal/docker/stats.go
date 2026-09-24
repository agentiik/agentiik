package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Stats is one sample of a container's statistics, as GET /containers/{id}/stats writes
// it, with the members the usage block is read from and none of the rest.
//
// A daemon writes a sample carrying nothing, not even the moment it was read, for a
// container it has nothing to read for: one that is not running, or whose cgroup it failed
// to read. Sampled tells that answer from a reading.
type Stats struct {
	Read   time.Time   `json:"read"`
	CPU    CPUStats    `json:"cpu_stats"`
	Memory MemoryStats `json:"memory_stats"`
}

// CPUStats is the processor half of a sample.
type CPUStats struct {
	Usage CPUUsage `json:"cpu_usage"`
}

// CPUUsage carries the container's processor time. Total is in nanoseconds and only ever
// grows over one run of the container, on either cgroup version, since the daemon
// converts cgroup v2's microseconds before it writes them.
type CPUUsage struct {
	Total uint64 `json:"total_usage"`
}

// MemoryStats is the memory half of a sample: the cgroup's usage, and the counters it is
// broken down into, keyed as the kernel names them.
type MemoryStats struct {
	Usage uint64            `json:"usage"`
	Stats map[string]uint64 `json:"stats,omitempty"`
}

// Sampled says the daemon read something, rather than answering for a container it had
// nothing to read for.
func (s Stats) Sampled() bool { return !s.Read.IsZero() }

// Resident is the memory the container holds, which is its usage less the page cache it
// has stopped using.
//
// The cgroup's usage counts every file page the container has read or written, and the
// kernel reclaims the inactive ones the moment anything else wants the memory, so a brick
// that read a large file once would otherwise report the file as its own. It is the
// figure docker stats shows, computed the same way: total_inactive_file is cgroup v1's
// name for the counter and inactive_file cgroup v2's, and a counter above the usage, which
// the two are not read atomically enough to rule out, is passed over.
func (m MemoryStats) Resident() uint64 {
	if v, v1 := m.Stats["total_inactive_file"]; v1 && v < m.Usage {
		return m.Usage - v
	}
	if v := m.Stats["inactive_file"]; v < m.Usage {
		return m.Usage - v
	}
	return m.Usage
}

// ContainerStatsOnce reads one sample now.
//
// one-shot is what makes it now: without it the daemon waits out two of its collections, a
// second apart, so as to write the processor figures beside an earlier reading to compare
// them with, and nothing here reads that comparison. The parameter is v1.41's, which is
// Floor.
func (c *Client) ContainerStatsOnce(ctx context.Context, id string) (Stats, error) {
	q := url.Values{}
	q.Set("stream", "false")
	q.Set("one-shot", "true")
	var s Stats
	if err := c.call(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil, &s); err != nil {
		return Stats{}, err
	}
	return s, nil
}

// StatsStream is a container's statistics as the daemon goes on writing them, one sample
// per collection, about a second apart.
//
// It does not end when the container exits. The daemon writes a sample carrying nothing
// at every collection after the exit and ends the stream only when the container is
// removed, so a caller reads it until it has what it came for and closes it.
type StatsStream struct {
	body io.ReadCloser
	dec  *json.Decoder
}

// ContainerStats opens the stream of a container's statistics.
func (c *Client) ContainerStats(ctx context.Context, id string) (*StatsStream, error) {
	q := url.Values{}
	q.Set("stream", "true")
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil)
	if err != nil {
		return nil, err
	}
	return &StatsStream{body: resp.Body, dec: json.NewDecoder(resp.Body)}, nil
}

// Next reads the next sample, and answers with an error once the stream has ended or its
// context is done.
func (s *StatsStream) Next() (Stats, error) {
	var st Stats
	if err := s.dec.Decode(&st); err != nil {
		return Stats{}, fmt.Errorf("reading a container's statistics: %w", err)
	}
	return st, nil
}

// Close ends the stream.
func (s *StatsStream) Close() error { return s.body.Close() }
