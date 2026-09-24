package runner

import (
	"fmt"
	"net"
	"strings"
)

// NotifySocket is the variable systemd names its notification socket in.
const NotifySocket = "NOTIFY_SOCKET"

// Ready is the state a service manager is told once the agent holds its floor and its daemon.
const Ready = "READY=1"

// Notify tells the service manager one state, over the socket NOTIFY_SOCKET names, and does
// nothing where it names none.
//
// The unit is Type=notify, so systemd counts the agent as started only when it hears READY=1, and
// a unit that never hears it is timed out and restarted. That is the point of it: a runner that
// refused its daemon, or could not read its settings, is a failed start in systemctl status rather
// than an active service taking no work. It is the protocol of sd_notify(3), one datagram on a
// unix socket, written here with the standard library because a libsystemd binding would be cgo,
// and the agent is a static binary.
//
// A socket name beginning with @ is in the abstract namespace, which is Linux's and which systemd
// uses where it can. A vsock: address, which systemd 254 added for virtual machines, is refused
// rather than misread as a path: a start that fails saying so is better than one that times out.
func Notify(socket, state string) error {
	if socket == "" {
		return nil
	}
	name := socket
	switch {
	case strings.HasPrefix(socket, "@"):
		name = "\x00" + socket[1:]
	case strings.HasPrefix(socket, "/"):
	default:
		return fmt.Errorf("runner: %s names %q, which is neither a path nor an abstract socket, and this runner tells systemd it is ready over a unix socket alone", NotifySocket, socket)
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("runner: the service manager could not be told %s: %w", state, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("runner: the service manager could not be told %s: %w", state, err)
	}
	return nil
}
