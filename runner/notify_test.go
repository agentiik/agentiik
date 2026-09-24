package runner

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// notifySocket listens where a service manager would, and answers its name and the socket.
//
// The directory is made short by hand, since the path of a unix socket is bounded at about a
// hundred bytes and a macOS temporary directory is most of that on its own.
func notifySocket(t *testing.T) (string, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agkn")
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(dir, "notify")) > 100 {
		os.RemoveAll(dir)
		if dir, err = os.MkdirTemp("/tmp", "agkn"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	name := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return name, conn
}

// heard reads the one datagram a service manager would, or fails.
func heard(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4096)
	n, err := conn.Read(b)
	if err != nil {
		t.Fatalf("the service manager heard nothing: %s", err)
	}
	return string(b[:n])
}

func TestReadyIsOneDatagramOnTheSocketNotifySocketNames(t *testing.T) {
	name, conn := notifySocket(t)
	if err := Notify(name, Ready); err != nil {
		t.Fatal(err)
	}
	if got := heard(t, conn); got != "READY=1" {
		t.Errorf("the service manager heard %q, want READY=1", got)
	}
}

func TestAnAbstractSocketIsReachedInTheAbstractNamespace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the abstract namespace is Linux's, and this machine is " + runtime.GOOS)
	}
	name := "agk-notify-" + filepath.Base(t.TempDir())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: "\x00" + name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Notify("@"+name, Ready); err != nil {
		t.Fatal(err)
	}
	if got := heard(t, conn); got != "READY=1" {
		t.Errorf("the service manager heard %q, want READY=1", got)
	}
}

func TestNoNotifySocketIsNothingToTell(t *testing.T) {
	if err := Notify("", Ready); err != nil {
		t.Errorf("a start outside systemd was refused: %s", err)
	}
}

func TestASocketNobodyListensOnIsAFailedStartAndNotASilentOne(t *testing.T) {
	err := Notify(filepath.Join(t.TempDir(), "gone"), Ready)
	if err == nil || !strings.Contains(err.Error(), "READY=1") {
		t.Errorf("telling a socket nobody listens on answered %v", err)
	}
}

func TestAnAddressThatIsNotAUnixSocketIsRefusedRatherThanMisread(t *testing.T) {
	for _, socket := range []string{"vsock:2:1234", "notify.sock"} {
		err := Notify(socket, Ready)
		if err == nil || !strings.Contains(err.Error(), NotifySocket) {
			t.Errorf("%s answered %v", socket, err)
		}
	}
}
