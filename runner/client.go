package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is how the agent reaches the API: one base URL, one runner credential at a time, and JSON
// both ways. Each route the agent calls is a file of this package over it, with types of its own
// held to the wire, since a runner imports nothing of package api.
type Client struct {
	base string
	http *http.Client

	// credential is replaced whole when the credential is rotated, and every call reads it
	// once, as it sends.
	mu         sync.Mutex
	credential Secret
}

// The classes of answer a route file tells apart. Each is what the runner does next rather than
// what the API said, which is why several statuses share one: no answer and a 5xx are both "ask
// again", and nothing about which of the two it was changes that.
var (
	// ErrCredentialRefused is a 401. The credential opens nothing: it was revoked, or rotated
	// past, or never existed, and asking again gets the same answer. A runner that gets it
	// joins again rather than retrying.
	ErrCredentialRefused = errors.New("runner: the API refused this runner's credential")

	// ErrForbidden is a 403: the credential is good and this runner may not do this, such as
	// redeeming while draining or for a namespace this host does not accept.
	ErrForbidden = errors.New("runner: the API refused this runner that request")

	// ErrConflict is a 409: what the request is about is somebody else's, or over.
	ErrConflict = errors.New("runner: the API answered that what the request is about is not this runner's to act on")

	// ErrUnprocessable is a 422: the request can never be answered, by this runner or any.
	ErrUnprocessable = errors.New("runner: the API answered that the request can never be answered")

	// ErrUnavailable is no answer, a 429 or a 5xx: the API is not answering now, and the same
	// request later may be.
	ErrUnavailable = errors.New("runner: the API did not answer")
)

// APIError is an answer that was not a success, with what the API said of it.
type APIError struct {
	Method, Path string
	Status       int
	// Message is the API's own error string, which is a sentence the API chose and never a
	// value of this runner's.
	Message string
	class   error
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("runner: %s %s answered %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap is the class of the answer, so that a caller asks errors.Is(err, ErrConflict) and never
// compares a status.
func (e *APIError) Unwrap() error { return e.class }

// answerMaxBytes is the most a success is read to.
//
// The largest answer a runner reads is a redemption, which names every file of the task's tree
// with a presigned URL for each: a few hundred bytes a file, for at most the 4096 files the API
// takes in a tree at push, which is a few megabytes. The bound leaves room for URLs longer than
// the store's own, and past it an answer is refused rather than held, since a runner reads it
// into memory whole.
const answerMaxBytes = 32 << 20

// refusalMaxBytes is the most a refusal is read to. The API writes one sentence.
const refusalMaxBytes = 4 << 10

// requestTimeout bounds a call whose context carries no earlier deadline. The API answers every
// route the agent calls in well under a second; one that has not answered in this long is not
// going to, and a runner waiting on it for ever is a runner whose heartbeat stopped.
const requestTimeout = 30 * time.Second

// NewClient builds the client of one runner.
//
// api is Config.API, which ReadConfig has already held to https or a loopback address. Without an
// http.Client, calls go through one that follows no redirect: every call carries the credential,
// and a runner sends it to the address it was given and nowhere else.
func NewClient(api string, credential Secret, client *http.Client) (*Client, error) {
	switch {
	case api == "":
		return nil, errors.New("runner: a client needs the address of the API")
	case credential == "":
		return nil, errors.New("runner: a client needs this runner's credential, and every call to the API carries it")
	}
	return newClient(api, credential, client), nil
}

// newClient is NewClient without its checks, for the one call a runner makes before it holds a
// credential: the join, which is authenticated by the token in its body and carries no
// Authorization at all. It follows no redirect either, since that body is the token.
func newClient(api string, credential Secret, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	return &Client{base: strings.TrimRight(api, "/"), credential: credential, http: client}
}

// Credential is the runner credential every call now carries.
func (c *Client) Credential() Secret {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credential
}

// use makes every call from now on carry credential, all at once: the API stops taking the one it
// replaces the first time it sees the new one, so a call that went on carrying the old one after
// that would be refused.
func (c *Client) use(credential Secret) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credential = credential
}

// Do sends one request and reads its answer.
//
// in, where it is not nil, is the JSON body. out, where it is not nil, is what a success is decoded
// into, strictly: a field it does not declare is refused, and so is anything after the value.
// Every repository of an installation carries one version, so a field this runner does not know is
// an API of another version, which is worth a refusal naming it rather than a field silently
// dropped. Anything but a 2xx comes back as an *APIError whose class says what to do next, and no
// answer at all as ErrUnavailable.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, nil, in, out)
}

// do is Do with headers of the route's own beside the ones every call carries, which is how a
// shipment carries its task's grant outside its body.
//
// A call refused 401 is sent once more where the credential was replaced while it was on its way.
// The API stops taking a rotated credential the first time it sees the new one, so a call sent with
// the old one a moment before another call carried the new one is refused for no fault of the
// runner's, and a refusal is answered before anything is done, so sending it again does nothing
// twice. A 401 to the credential still held is the runner's to act on.
func (c *Client) do(ctx context.Context, method, path string, header http.Header, in, out any) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("runner: %s is not a path below the API", path)
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("runner: %s %s: the request could not be written: %w", method, path, err)
		}
		body = b
	}
	sent := c.Credential()
	err := c.send(ctx, method, path, header, body, in != nil, sent, out)
	if errors.Is(err, ErrCredentialRefused) {
		if now := c.Credential(); now != sent {
			return c.send(ctx, method, path, header, body, in != nil, now, out)
		}
	}
	return err
}

// send sends one request with credential and reads its answer.
func (c *Client) send(ctx context.Context, method, path string, header http.Header, body []byte, hasBody bool, credential Secret, out any) error {
	var r io.Reader
	if hasBody {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return fmt.Errorf("runner: %s %s: %w", method, path, err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+string(credential))
	}
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agk-runner/"+Version())
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		return fmt.Errorf("runner: %s %s: %w: %v", method, path, ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return decode(resp.Body, out, method, path)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &APIError{Method: method, Path: path, Status: resp.StatusCode,
			Message: "a redirect, which this runner does not follow: its credential goes to the address it was given and nowhere else"}
	}
	return refusal(resp, method, path)
}

// decode reads a success into out, strictly and within answerMaxBytes.
func decode(r io.Reader, out any, method, path string) error {
	limited := io.LimitReader(r, answerMaxBytes+1)
	if out == nil {
		n, err := io.Copy(io.Discard, limited)
		if err == nil && n > answerMaxBytes {
			err = fmt.Errorf("the answer is more than %d bytes", answerMaxBytes)
		}
		if err != nil {
			return fmt.Errorf("runner: %s %s: %w", method, path, err)
		}
		return nil
	}
	b, err := io.ReadAll(limited)
	switch {
	case err != nil:
		return fmt.Errorf("runner: %s %s: %w: the answer could not be read: %v", method, path, ErrUnavailable, err)
	case len(b) > answerMaxBytes:
		return fmt.Errorf("runner: %s %s: the answer is more than %d bytes, which is more than any answer a runner is owed", method, path, answerMaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("runner: %s %s: the answer is not the one this runner reads, which is an API of another version or no API at all: %w", method, path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("runner: %s %s: the answer carries something after its value, and an answer is one value", method, path)
	}
	return nil
}

// refusal reads a non-success into an *APIError, classed by what the runner does next.
func refusal(resp *http.Response, method, path string) error {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode}
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized:
		e.class = ErrCredentialRefused
	case s == http.StatusForbidden:
		e.class = ErrForbidden
	case s == http.StatusConflict:
		e.class = ErrConflict
	case s == http.StatusUnprocessableEntity:
		e.class = ErrUnprocessable
	case s == http.StatusTooManyRequests || s >= 500:
		e.class = ErrUnavailable
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, refusalMaxBytes))
	var said struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &said) == nil {
		e.Message = printable(said.Error)
	}
	return e
}

// printable keeps a message from the API to one line of printable text, since it goes into the
// agent's log as it is and a line break in it would forge a line of its own.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
