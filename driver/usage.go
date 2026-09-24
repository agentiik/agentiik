package driver

import (
	"context"
	"sync"
	"time"

	"github.com/agentiik/agentiik/internal/docker"
)

// statsDrain is how long the statistics of a container that has exited are read on for.
//
// A sample the daemon read just before the exit can still be on its way when the exit is
// seen, since the two arrive on connections of their own, and the daemon writes a sample
// carrying nothing at its next collection, a second at most after the exit. The drain
// ends at that sample, so that everything read before the exit is counted, and at this
// bound where the daemon is slower than that. The bound is short because it is a wait on
// every task: a sample read in the last moment before the exit is on the wire long before
// the exit has travelled through the wait, and the drain runs while the outputs are
// collected, so a task is kept longer only by however much of it is left then.
const statsDrain = 250 * time.Millisecond

// sampler reads what one container consumes while it runs, from the daemon's statistics.
//
// The figures have to be read while the container runs: its cgroup goes when it exits,
// and with it everything the kernel counted, so nothing can be asked afterwards. So a
// sample is read at once, and then the daemon's stream is followed, one sample per
// collection, a second apart. The processor total only grows, and the highest total seen
// is the last one read. The memory is the highest resident set any sample saw, which is
// the usage less the page cache the container has stopped using, as docker stats shows
// it.
//
// Sampling makes both figures what was seen and never more: the processor time spent after
// the last sample, and a peak between two samples, are not counted. A container that
// exits before any sample was read reports neither, rather than a figure made up for it:
// a sample the daemon wrote while it had nothing to read, which is every sample after the
// exit, carries nothing and counts for nothing. A daemon that refuses the statistics costs
// the task its figures and nothing else, since what a task consumed is a report about it
// and not a condition of its ending.
type sampler struct {
	cancel context.CancelFunc

	// over is closed once the container is known to have exited, which is when the
	// first sample carrying nothing means there is nothing more to come.
	over     chan struct{}
	overOnce sync.Once

	// done is closed when the goroutine reading the samples has returned, and the
	// three fields below are its until then.
	done chan struct{}
	cpu  uint64
	peak uint64
	seen bool
}

// sample starts reading the statistics of a container that has just been started, or
// adopted running.
func (d *Docker) sample(ctx context.Context, container string) *sampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &sampler{cancel: cancel, over: make(chan struct{}), done: make(chan struct{})}
	go s.read(ctx, d.cli, container)
	return s
}

// read is the goroutine: one sample now, then the stream until the container is over.
//
// The one sample now is what a container gets that exits before the stream's first
// collection, which on a daemon collecting once a second is every container that runs
// for less than one.
func (s *sampler) read(ctx context.Context, cli *docker.Client, container string) {
	defer close(s.done)
	if now, err := cli.ContainerStatsOnce(ctx, container); err == nil {
		s.fold(now)
	}
	stream, err := cli.ContainerStats(ctx, container)
	if err != nil {
		return
	}
	defer stream.Close()
	for {
		st, err := stream.Next()
		if err != nil {
			return
		}
		if st.Sampled() {
			s.fold(st)
			continue
		}
		// A sample carrying nothing while the container runs is a collection the
		// daemon failed, and the next may not be. Once it has exited, it is the daemon
		// saying there is nothing left to read.
		select {
		case <-s.over:
			return
		default:
		}
	}
}

// fold counts one sample the daemon read.
func (s *sampler) fold(st docker.Stats) {
	s.seen = true
	s.cpu = max(s.cpu, st.CPU.Usage.Total)
	s.peak = max(s.peak, st.Memory.Resident())
}

// exited says the container has exited, so that the drain begins while the outputs are
// collected rather than after.
func (s *sampler) exited() {
	if s == nil {
		return
	}
	s.overOnce.Do(func() { close(s.over) })
}

// stop ends the reading at once, and is what every path out of a task that did not come
// to an ending takes.
func (s *sampler) stop() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

// usage is the usage block of a task whose container has exited: what the samples saw,
// once the drain is over, and how long the image took to pull. A nil sampler is a
// container nobody watched run, which an adopted container found exited is, and it
// reports the pull alone.
func (s *sampler) usage(image resolved) Usage {
	u := Usage{ImagePullMS: image.PullMillis}
	if s == nil {
		return u
	}
	s.exited()
	drain := time.NewTimer(statsDrain)
	defer drain.Stop()
	select {
	case <-s.done:
	case <-drain.C:
	}
	s.stop()
	if s.seen {
		u.CPUSeconds = float64(s.cpu) / 1e9
		u.MaxRSSBytes = int64(s.peak)
	}
	return u
}
