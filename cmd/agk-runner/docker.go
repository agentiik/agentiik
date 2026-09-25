package main

import (
	"fmt"
	"strings"
)

// dockerHost is the daemon DOCKER_HOST names, as Docker's own clients read it, and empty where it
// is unset.
//
// The runner drives the daemon on its own host, over its local unix socket, which crosses no
// network and is guarded by its file permission, as the Channels table says. Any other transport
// is refused at the start, naming the variable: a tcp:// daemon is one reached across a network,
// in plaintext or under keys the agent does not hold, and it is refused before the driver would
// refuse it without saying which setting was wrong. Only the scheme is repeated, since an address
// can carry a user.
func dockerHost(e env) (string, error) {
	socket, _ := e.Lookup("DOCKER_HOST")
	if scheme, _, found := strings.Cut(socket, "://"); found && scheme != "unix" {
		return "", fmt.Errorf("DOCKER_HOST names a daemon at %s://, and a runner drives the daemon on its own host over its local unix socket alone, whose file permission is what guards it: write unix:///var/run/docker.sock or a path, or leave it unset", scheme)
	}
	return socket, nil
}
