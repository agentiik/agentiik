package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// Depths is how many task messages each pool's queue holds: published and not yet acknowledged by
// a runner, which is waiting for a runner with room and being redeemed by one.
//
// Read off the stream rather than off each pool's consumer. With WorkQueue retention a message
// stays on the stream until a runner acknowledges it, so what the stream holds under a pool's
// subject is that pool's queue, whether or not its consumer exists yet, and one request answers
// for every pool. A pool whose queue is empty is not named, and reads as zero.
//
// It is the control plane's: a runner's credential may not ask.
func (b *Bus) Depths(ctx context.Context) (map[string]int, error) {
	if b.stream == nil {
		return nil, errors.New("bus: only the control plane reads the depth of the queues")
	}
	info, err := b.stream.Info(ctx, jetstream.WithSubjectFilter(Subject(">")))
	if err != nil {
		return nil, fmt.Errorf("bus: the depth of the queues could not be read: %w", err)
	}
	out := make(map[string]int, len(info.State.Subjects))
	for subject, n := range info.State.Subjects {
		pool, ok := strings.CutPrefix(subject, Subject(""))
		if !ok || pool == "" {
			continue
		}
		out[pool] = int(n)
	}
	return out, nil
}
