package bus

import (
	"testing"
	"time"
)

// What the tests of package bus_test reach for inside this one. They put the API, the database and
// package bus/control beside the bus, and the API and bus/control import this package, so they
// cannot be written inside it.

// Opened is a bus with empty queues and the consumers the control plane makes, which is what every
// test of this package starts from.
func Opened(t *testing.T) *Bus { return open(t) }

// Waiting remakes one pool's consumer with its AckWait cut to wait, so that a test sees what follows
// the wait without spending AckWait on it.
func (b *Bus) Waiting(t *testing.T, pool string, wait time.Duration) {
	t.Helper()
	if err := b.consumer(t.Context(), pool, wait); err != nil {
		t.Fatal(err)
	}
}

// Outstanding is how many messages of one pool the bus still means to hand out: those nobody has
// been handed yet, and those handed and not acknowledged.
func (b *Bus) Outstanding(t *testing.T, pool string) int {
	t.Helper()
	consumer, err := b.js.Consumer(t.Context(), Stream, Durable(pool))
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return int(info.NumPending) + info.NumAckPending
}
