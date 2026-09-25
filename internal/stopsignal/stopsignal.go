// Package stopsignal is how a long-running program of the engine is asked to stop: the first
// SIGINT or SIGTERM is a stop asked for, which the program takes and finishes its work under, and
// the second ends the process as it would any program that never asked for signals. A person
// pressing Ctrl-C twice, or a service manager that has waited long enough, means now.
//
// agentiik-api, agentiik-controller and agk-runner each start through it, so that the three keep
// one rule rather than three copies of it drifting apart.
package stopsignal

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Signals are the two that ask for a stop: a person's interrupt, and what systemd and docker stop
// send before they kill.
var Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// betweenFirstAndReset runs after the first signal is taken and before the signals go back to
// their default, which is the window a second signal can arrive in. A test widens it.
var betweenFirstAndReset = func() {}

// Context is done at the first of Signals, and a second one ends the process whenever it comes.
// signal.NotifyContext with a goroutine calling its stop once it was done lost that second signal
// when it arrived between the two: the context was done, a program or a person watching it saw
// the stop under way, and the signal was still taken, into a channel nobody read again.
//
// So a second signal arriving before the reset is raised again once the default is back, which is
// what keeps it. The context is also done only once the default is back, so that a program seeing
// it done is past that window altogether. stop gives the signals back to their default, releases
// the context, and returns once the default is back.
func Context() (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	// Room for both, so that a second signal arriving before the first is read is kept too
	// rather than dropped by a full channel.
	taken := make(chan os.Signal, 2)
	signal.Notify(taken, Signals...)
	reset := make(chan struct{})
	go func() {
		defer close(reset)
		asked := false
		select {
		case <-taken:
			asked = true
			betweenFirstAndReset()
		case <-ctx.Done():
		}
		// Stop returns once nothing more reaches taken, so what is in it now is everything
		// that arrived before the reset. Where no stop was asked for, a signal in it is a
		// first one arriving as the program ends on its own, and it has nothing left to stop.
		signal.Stop(taken)
		select {
		case second := <-taken:
			if self, err := os.FindProcess(os.Getpid()); asked && err == nil {
				self.Signal(second)
			}
		default:
		}
		cancel()
	}()
	return ctx, func() {
		cancel()
		<-reset
	}
}
