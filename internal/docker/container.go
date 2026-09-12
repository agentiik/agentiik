package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// createBody is the one object POST /containers/create takes: the container config at
// the top level, with the host config and the networking config beside it.
type createBody struct {
	Config
	HostConfig       HostConfig       `json:"HostConfig"`
	NetworkingConfig NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

// ContainerCreate creates one container and answers with its identifier.
//
// name may be empty, in which case the daemon invents one. Naming it is what makes a
// second create of the same task fail with a conflict instead of quietly producing a
// second container, which is the other half of adoption by label.
func (c *Client) ContainerCreate(ctx context.Context, name string, cfg Config, hc HostConfig, nc NetworkingConfig) (Created, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	body := createBody{Config: cfg, HostConfig: hc, NetworkingConfig: nc}

	var created Created
	if err := c.call(ctx, http.MethodPost, "/containers/create", q, body, &created); err != nil {
		return Created{}, err
	}
	return created, nil
}

// ContainerStart starts a container that was created and not started.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// ContainerWait opens a wait and answers with the channel the exit will arrive on.
//
// It returns once the daemon has acknowledged the wait, not once the container has
// exited, and that is the whole reason it exists in this shape. With
// condition=next-exit the daemon writes the response header as soon as the wait is
// registered, so a caller that has this function's return value knows the exit cannot be
// missed, and may then start the container. Opening the wait after the start is the
// exit-during-attach race, and it is a race the convenience wrappers of every client
// library invite a caller to run.
//
// Exactly one value arrives on the channel, and then it is closed. A wait that failed on
// the way arrives as a Waited carrying Err, so one channel carries both outcomes.
func (c *Client) ContainerWait(ctx context.Context, id, condition string) (<-chan Waited, error) {
	q := url.Values{}
	if condition != "" {
		q.Set("condition", condition)
	}
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/wait", q, nil)
	if err != nil {
		return nil, err
	}

	ch := make(chan Waited, 1)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		var w Waited
		if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
			// A context that was cancelled is reported as itself rather than
			// as the read failure it caused, so that a caller stopping its own
			// wait does not read it as a daemon that vanished.
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			ch <- Waited{Err: fmt.Errorf("waiting for container %s: %w", short(id), err)}
			return
		}
		ch <- w
	}()
	return ch, nil
}

// LogOptions is what a log request asks for. The driver asks for both streams and no
// timestamps, because the timestamp on a log line is the driver's own and is written
// after the masker has run.
type LogOptions struct {
	Stdout     bool
	Stderr     bool
	Follow     bool
	Timestamps bool
	Since      time.Time
	Tail       string
}

// ContainerLogs reads what the container wrote, from the daemon, after it exited.
//
// This is why AutoRemove is false. A container the daemon removed on exit takes its logs
// with it, and a container that exits in the same instant it starts would lose
// everything it wrote before the attach was established. The daemon keeps both streams,
// and they come back framed exactly as the attach frames them.
//
// The stream carries no standard input, so Stream.Stdin is nil.
func (c *Client) ContainerLogs(ctx context.Context, id string, o LogOptions) (*Stream, error) {
	q := url.Values{}
	q.Set("stdout", boolean(o.Stdout))
	q.Set("stderr", boolean(o.Stderr))
	q.Set("follow", boolean(o.Follow))
	q.Set("timestamps", boolean(o.Timestamps))
	if !o.Since.IsZero() {
		q.Set("since", strconv.FormatInt(o.Since.Unix(), 10))
	}
	if o.Tail != "" {
		q.Set("tail", o.Tail)
	}

	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	return newStream(resp.Body, nil, resp.Body), nil
}

// ContainerArchive reads one path out of a container as a tar stream.
//
// It is how /agk/brick.yaml is read out of an image, through a container created from it
// and never started, and how anything left under /agk/out is read back where the working
// directory is not on this host. The caller closes what comes back.
func (c *Client) ContainerArchive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("path", path)
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/archive", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ContainerInspect is the backstop. The wait is the fast path and the event stream
// catches an exit the wait missed, and this is what is consulted when a task has been
// silent past its deadline: a dropped stream makes a task slow, never wrong.
func (c *Client) ContainerInspect(ctx context.Context, id string) (Inspected, error) {
	var in Inspected
	if err := c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &in); err != nil {
		return Inspected{}, err
	}
	return in, nil
}

// ContainerList lists containers matching the filters, stopped ones included.
//
// all is true here and not a parameter, because both callers want it: adoption looks for
// a container that may have exited while this process was away, and the startup sweep
// looks for exactly those.
func (c *Client) ContainerList(ctx context.Context, f Filters) ([]Summary, error) {
	q := url.Values{}
	q.Set("all", "true")
	f.encode(q)

	var list []Summary
	if err := c.call(ctx, http.MethodGet, "/containers/json", q, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ContainerStop asks the daemon to stop a container: SIGTERM, then SIGKILL after the
// grace.
//
// The escalation belongs to the daemon rather than to a timer on this side, and that is
// the reason this call is used instead of two kills. A runner that dies between the two
// signals leaves a container that the daemon still kills; a runner holding the timer
// itself leaves one running forever.
//
// A negative grace leaves t unset, which is the daemon's own default.
func (c *Client) ContainerStop(ctx context.Context, id string, grace time.Duration) error {
	q := url.Values{}
	if grace >= 0 {
		// The Engine API counts the grace in whole seconds. A grace under a
		// second is rounded up rather than down to zero, because rounding it down
		// would turn a stop into a kill.
		seconds := int64(grace / time.Second)
		if grace > 0 && grace%time.Second != 0 {
			seconds++
		}
		q.Set("t", strconv.FormatInt(seconds, 10))
	}
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil, nil)
}

// ContainerKill sends one signal and returns. It is the backstop for a stop the daemon
// did not carry through, and the way a deadline reaches a container the driver is
// holding.
func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	q := url.Values{}
	if signal != "" {
		q.Set("signal", signal)
	}
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/kill", q, nil, nil)
}

// ContainerRemove removes a container and the anonymous volumes it owns.
//
// force is for the container that is still running, which is the sweep finding what an
// earlier process left behind rather than the ordinary path: a task removes its own
// container after it has exited.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	q.Set("v", "true")
	q.Set("force", boolean(force))
	return c.call(ctx, http.MethodDelete, "/containers/"+id, q, nil, nil)
}

// boolean writes a query flag the way the Engine API reads one.
func boolean(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// short is a container identifier as a person reads it, which is the twelve characters
// the daemon itself prints.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
