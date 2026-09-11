package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
)

// AttachOptions is what an attach asks for. The driver asks for all three streams and
// for stream rather than logs, because it takes the log from the daemon afterwards.
type AttachOptions struct {
	Stdin  bool
	Stdout bool
	Stderr bool

	// Stream asks for what the container writes from now on. Logs asks for what it
	// wrote before the attach, which this product reads through ContainerLogs
	// instead, after the exit, where nothing can be lost to a fast start.
	Stream bool
	Logs   bool
}

// ContainerAttach opens the hijacked stream a container is spoken to on.
//
// The attach is the one call of the Engine API that stops being HTTP. The daemon answers
// 101 and the connection becomes the container's three streams, so it is dialed raw,
// written by hand and never handed back to the pool: a connection the pool reused after
// an upgrade would hand somebody else's request the tail of a container's output.
//
// The handshake is sixty lines of standard library and nothing more: write the request
// onto a net.Conn with Request.Write, read the answer back with http.ReadResponse, and
// keep the bufio.Reader that read it, because it already holds the first bytes of the
// stream.
func (c *Client) ContainerAttach(ctx context.Context, id string, o AttachOptions) (*Stream, error) {
	q := url.Values{}
	q.Set("stdin", boolean(o.Stdin))
	q.Set("stdout", boolean(o.Stdout))
	q.Set("stderr", boolean(o.Stderr))
	q.Set("stream", boolean(o.Stream))
	q.Set("logs", boolean(o.Logs))

	req, err := c.request(ctx, http.MethodPost, "/containers/"+id+"/attach", q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	conn, err := c.dialRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("attaching to container %s: %w", short(id), err)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, transportError(c.socket, http.MethodPost, "/containers/"+id+"/attach", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, transportError(c.socket, http.MethodPost, "/containers/"+id+"/attach", err)
	}
	// 101 is the upgrade this asked for. 200 is a daemon that answered without
	// upgrading, which is still the stream, on the same connection.
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer conn.Close()
		return nil, fmt.Errorf("attaching to container %s: %w", short(id), refusal(resp))
	}

	// A raw connection is not bound by a context the way a pooled request is, so the
	// context is given a hand on it: cancelling closes the connection, which is what
	// unblocks a read that would otherwise wait for a container that will never
	// write again.
	hijacked := &attached{conn: conn}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			hijacked.Close()
		case <-stop:
		}
	}()
	hijacked.stop = stop

	var stdin io.WriteCloser
	if o.Stdin {
		stdin = writeHalf{hijacked}
	}
	return newStream(br, stdin, hijacked), nil
}

// attached is the hijacked connection, closed once however many times it is asked.
//
// Closing twice is the ordinary path rather than an accident: the context watcher closes
// it when a task is stopped, and the caller closes it in the defer that runs on every
// path.
type attached struct {
	conn net.Conn

	once sync.Once
	stop chan struct{}
	err  error
}

// Close closes the connection and releases the context watcher.
func (a *attached) Close() error {
	a.once.Do(func() {
		if a.stop != nil {
			close(a.stop)
		}
		a.err = a.conn.Close()
	})
	return a.err
}

// CloseWrite closes the writing half, which is the end of standard input.
//
// A unix socket has a write half to close. Where the connection turns out not to have
// one, the whole connection is not closed instead: that would throw away the output the
// container is about to write, which is the opposite of what closing standard input is
// for. The brick reading to the end is the one that waits, and its own timeout is what
// ends it.
func (a *attached) CloseWrite() error {
	if w, ok := a.conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return fmt.Errorf("the connection to the daemon has no write half to close, so standard input cannot be ended without ending the stream")
}

// writeHalf is the container's standard input: writes go to the connection, and a close
// is a half-close.
type writeHalf struct{ a *attached }

func (w writeHalf) Write(b []byte) (int, error) { return w.a.conn.Write(b) }

func (w writeHalf) Close() error { return w.a.CloseWrite() }
