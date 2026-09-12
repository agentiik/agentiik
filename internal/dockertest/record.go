package dockertest

// Created is every container this daemon was asked to create, in the order it was asked,
// as it was created.
//
// It is how a test reads the settings table back: what comes out of here went through
// JSON on a real socket, so a field with the wrong wire name, or one omitted where the
// table says it is always sent, shows up here as a zero value rather than passing
// because the struct was never marshalled.
//
// A removed container is still in this list. What was created and what was destroyed are
// two questions, and Removed answers the second.
func (d *Daemon) Created() []Container {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]Container, 0, len(d.order))
	for _, l := range d.order {
		out = append(out, l.container())
	}
	return out
}

// Removed is every container and network this daemon was asked to destroy, in order.
//
// It exists so that the container, the network and the working directory being destroyed
// is an assertion and not an assumption: "the working directory of a task is created
// fresh, owned by an unprivileged account, and removed with the container, so no residue
// of one namespace survives into the next task on that host".
func (d *Daemon) Removed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.removed...)
}

// Events is every event this daemon emitted, which is what a test asserting that an
// out-of-memory kill was reported reads.
func (d *Daemon) Events() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.events)
}
