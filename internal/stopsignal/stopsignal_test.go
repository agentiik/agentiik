package stopsignal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// windowVariable makes a process of this test binary one that takes the first signal through
// Context, with the window before the reset widened, and then never finishes stopping. "wide"
// widens it; "second" also has a second signal arrive inside it.
const windowVariable = "AGENTIIK_STOPSIGNAL_TEST_WINDOW"

func TestMain(m *testing.M) {
	if window := os.Getenv(windowVariable); window != "" {
		betweenFirstAndReset = func() {
			if window == "second" {
				self, _ := os.FindProcess(os.Getpid())
				self.Signal(syscall.SIGTERM)
			}
			time.Sleep(300 * time.Millisecond)
		}
		ctx, _ := Context()
		fmt.Println("waiting")
		<-ctx.Done()
		fmt.Println("stopping")
		select {}
	}
	os.Exit(m.Run())
}

// A second signal sent the moment the stop is seen under way ends the process, however long the
// reset takes after the first: a context done before it would have let the second be taken and
// dropped.
func TestASecondSignalSentOnceTheStopIsUnderWayEndsTheProcess(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, out, exited := slowStop(t, "wide")
			for _, want := range []string{"waiting", "stopping"} {
				if want == "stopping" {
					if err := cmd.Process.Signal(sig); err != nil {
						t.Fatal(err)
					}
				}
				eventually(t, "the process "+want, func() bool { return strings.Contains(out.String(), want) }, out)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			endedBy(t, sig, exited, out)
		})
	}
}

// A second signal arriving while the first is still being taken, before the signals are back to
// their default, is raised again once they are rather than lost.
func TestASecondSignalArrivingBeforeTheResetIsNotLost(t *testing.T) {
	cmd, out, exited := slowStop(t, "second")
	eventually(t, "the process waiting", func() bool { return strings.Contains(out.String(), "waiting") }, out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	endedBy(t, syscall.SIGTERM, exited, out)
}

func TestStopReleasesTheContextWithNoSignal(t *testing.T) {
	ctx, stop := Context()
	if ctx.Err() != nil {
		t.Fatalf("the context is done before any signal or stop: %v", ctx.Err())
	}
	stop()
	if ctx.Err() == nil {
		t.Error("stop returned with the context still not done")
	}
	stop()
}

// slowStop starts a process of this test binary that takes the first signal and never finishes
// stopping.
func slowStop(t *testing.T, window string) (*exec.Cmd, *output, chan error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := &output{}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), windowVariable+"="+window)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, out, exited
}

// endedBy fails unless the process ends of sig, as a process that never asked for signals would.
func endedBy(t *testing.T, sig syscall.Signal, exited chan error, out *output) {
	t.Helper()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != sig {
			t.Errorf("the second %v ended the process with %v:\n%s", sig, err, out)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the second %v did not end a process still stopping:\n%s", sig, out)
	}
}

func eventually(t *testing.T, what string, done func() bool, out *output) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("never saw %s:\n%s", what, out)
}

// output is what a process wrote, kept for a failure to show.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}
