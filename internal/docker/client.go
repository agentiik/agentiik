package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dialTimeout bounds the one call that is not long-lived, the version negotiation Dial
// makes. Everything after it is bounded by the context its caller passed, because
// attach, wait, logs and events have no timeout that would be right.
const dialTimeout = 10 * time.Second

// host is the authority the request line carries. A unix socket has none, and the daemon
// does not read it, so this is a constant that makes a URL parse rather than a value
// anybody configures.
const host = "http://docker"

// Client is one handle on one daemon, safe for many tasks at once.
//
// It holds two transports on purpose: a pooled one for the unary calls, and a raw
// net.Conn dialed per attach, so that a hijacked connection is never handed back to the
// pool. There is no client-level timeout on either.
type Client struct {
	socket  string
	http    *http.Client
	version string
}

// DefaultSocket is where a daemon is looked for when nothing said. It consults
// DOCKER_HOST first, then the per-user path Docker Desktop puts a socket at, then the
// system path.
//
// The per-user path is in the list because of the machine this was written on: Docker
// Desktop puts its socket under the home directory and /var/run/docker.sock is a symlink
// that may or may not exist. A client that knows only the system path refuses to run
// exactly where agk run --local has to run.
func DefaultSocket() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		desktop := filepath.Join(home, ".docker", "run", "docker.sock")
		if _, err := os.Stat(desktop); err == nil {
			return desktop
		}
	}
	return "/var/run/docker.sock"
}

// Dial opens a client on a socket and negotiates the API version with it, once.
//
// The negotiation is part of opening rather than a call a caller has to remember,
// because every path this package builds carries the version and there is no correct
// path to build before the daemon has answered.
func Dial(socket string) (*Client, error) {
	path, err := SocketPath(socket)
	if err != nil {
		return nil, err
	}

	c := &Client{
		socket: path,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	v, err := c.Ping(ctx)
	if err != nil {
		return nil, err
	}
	spoken, err := negotiate(v.APIVersion)
	if err != nil {
		return nil, err
	}
	c.version = spoken
	return c, nil
}

// SocketPath is the filesystem path of the socket Dial opens for socket, where empty is
// DefaultSocket, so that a caller asking something of the socket file itself, such as the
// group that owns it, asks it of the one Dial will reach.
func SocketPath(socket string) (string, error) {
	if socket == "" {
		socket = DefaultSocket()
	}
	return socketPath(socket)
}

// socketPath reads a DOCKER_HOST or a plain path and answers with the filesystem path of
// a unix socket, refusing anything else by name rather than by failing to connect to it.
func socketPath(socket string) (string, error) {
	switch {
	case strings.HasPrefix(socket, "unix://"):
		return strings.TrimPrefix(socket, "unix://"), nil
	case strings.HasPrefix(socket, "/"):
		return socket, nil
	case strings.Contains(socket, "://"):
		scheme := socket[:strings.Index(socket, "://")]
		return "", fmt.Errorf("the Docker daemon is reached over its unix socket, and %s:// is not one: this package speaks no other transport", scheme)
	default:
		return socket, nil
	}
}

// Close releases the pooled connections. A hijacked stream is closed by whoever holds
// it, since it left the pool the moment it was upgraded.
func (c *Client) Close() error {
	if t, ok := c.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// APIVersion is the version being spoken, which is min(Ceiling, what the daemon offers)
// and not what was compiled in.
func (c *Client) APIVersion() string { return c.version }

// Socket is where this client is talking, which is what an error naming the daemon says.
func (c *Client) Socket() string { return c.socket }

// path composes a versioned request path. Everything but the ping goes through here, so
// there is one place the negotiated version is spliced in.
func (c *Client) path(p string, q url.Values) string {
	u := host
	if c.version != "" {
		u += "/v" + c.version
	}
	u += p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// request builds one request with the body marshalled as JSON when there is one.
func (c *Client) request(ctx context.Context, method, p string, q url.Values, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("writing the body of %s %s: %w", method, p, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.path(p, q), r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends one request and hands back the response with its body still open, refusing on
// a status the daemon uses for a refusal. The caller closes the body.
func (c *Client) do(ctx context.Context, method, p string, q url.Values, body any) (*http.Response, error) {
	req, err := c.request(ctx, method, p, q, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(c.socket, method, p, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %w", method, p, refusal(resp))
	}
	return resp, nil
}

// call sends one request, decodes the answer into out where there is one, and closes the
// body. It is the shape of every unary call in this package.
func (c *Client) call(ctx context.Context, method, p string, q url.Values, body, out any) error {
	resp, err := c.do(ctx, method, p, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		// The body is drained rather than abandoned, so the connection goes back
		// to the pool instead of being closed under it.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s %s: %w", method, p, err)
	}
	return nil
}

// refusal reads the daemon's own message off a refused response. The Engine API writes
// one JSON object carrying one message, and a daemon that wrote something else still
// gets its status reported rather than swallowed.
func refusal(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(b) == 0 {
		return e
	}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		e.Message = m.Message
		return e
	}
	e.Message = strings.TrimSpace(string(b))
	return e
}

// transportError tells a daemon that is not there from a call that went wrong, which is
// the distinction a driver needs before it charges a failure to anybody.
func transportError(socket, method, p string, err error) error {
	if IsUnreachable(err) {
		return &unreachable{socket: socket, err: fmt.Errorf("%s %s: %w", method, p, err)}
	}
	return fmt.Errorf("%s %s: %w", method, p, err)
}

// dialRaw opens a connection of its own, outside the pool. It is what the attach is
// spoken on, because a hijacked connection stops being HTTP and must never be reused.
func (c *Client) dialRaw(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, &unreachable{socket: c.socket, err: err}
	}
	return conn, nil
}
