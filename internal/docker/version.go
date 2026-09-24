package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Ceiling is the API version the documentation names, and the highest this package will
// speak. It is written once, here, and never compiled into a path prefix: a hard pin
// refuses to run where agk run --local has to run, because a daemon offering 1.55
// refuses every path under /v1.56.
const Ceiling = "1.56"

// Floor is the oldest daemon this project intends to support, and it is a policy choice
// rather than a technical one. The newest endpoint this package needs is the wait with
// condition=next-exit, which arrived at v1.30, so nothing here would break far below
// this; the number says which daemons are supported, not which ones would happen to
// work, and it belongs on the site beside the rest of what an installation must provide.
//
// v1.41 is Docker 20.10, the oldest release line still seen on a host somebody expects
// to install a runner on.
const Floor = "1.41"

// Version is what the daemon answered when asked who it is.
//
// The negotiation needs one number, the highest version the daemon speaks, and /_ping
// carries it in a header. That is why this package asks /_ping and not /version: the
// ping is the cheapest call the daemon has and it answers the only question being asked.
type Version struct {
	APIVersion   string
	OSType       string
	Experimental bool
	Builder      string
}

// Ping asks the daemon who it is. It is unversioned, because the version is what it is
// being asked for.
func (c *Client) Ping(ctx context.Context) (Version, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/_ping", nil)
	if err != nil {
		return Version{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Version{}, transportError(c.socket, http.MethodGet, "/_ping", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// A daemon answers the ping with its version headers whatever the status,
	// including where it answers 500 while it is still starting, so the headers are
	// read before the status is judged.
	v := Version{
		APIVersion:   resp.Header.Get("Api-Version"),
		OSType:       resp.Header.Get("Ostype"),
		Experimental: resp.Header.Get("Docker-Experimental") == "true",
		Builder:      resp.Header.Get("Builder-Version"),
	}
	if resp.StatusCode >= 400 && v.APIVersion == "" {
		return Version{}, fmt.Errorf("GET /_ping: %w", &Error{Status: resp.StatusCode})
	}
	if v.APIVersion == "" {
		return Version{}, fmt.Errorf("the daemon at %s answered the ping without an Api-Version header, so there is no version to speak", c.socket)
	}
	return v, nil
}

// negotiate settles on min(Ceiling, what the daemon offers), and refuses below the floor
// naming both versions, because a refusal that names only one of them leaves the reader
// to guess which end is wrong.
func negotiate(offered string) (string, error) {
	daemon, err := parseAPIVersion(offered)
	if err != nil {
		return "", err
	}
	floor, _ := parseAPIVersion(Floor)
	ceiling, _ := parseAPIVersion(Ceiling)

	if daemon.before(floor) {
		return "", fmt.Errorf("the daemon speaks API version %s and this runner needs %s or later: %s is the oldest daemon Agentiik supports", offered, Floor, Floor)
	}
	if daemon.before(ceiling) {
		return offered, nil
	}
	return Ceiling, nil
}

// apiVersion is a major and a minor, which is all an Engine API version is.
type apiVersion struct{ major, minor int }

func (v apiVersion) before(o apiVersion) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	return v.minor < o.minor
}

// parseAPIVersion reads a version the daemon wrote, refusing anything that is not two
// numbers with a dot between them rather than guessing at it.
func parseAPIVersion(s string) (apiVersion, error) {
	major, minor, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		return apiVersion{}, fmt.Errorf("%q is not an Engine API version: one is written major.minor, as %s is", s, Ceiling)
	}
	a, err := strconv.Atoi(major)
	if err != nil {
		return apiVersion{}, fmt.Errorf("%q is not an Engine API version: %w", s, err)
	}
	b, err := strconv.Atoi(minor)
	if err != nil {
		return apiVersion{}, fmt.Errorf("%q is not an Engine API version: %w", s, err)
	}
	return apiVersion{major: a, minor: b}, nil
}

// Speaks says whether the version this client settled on is v or later, which is how a
// caller asks for something only a daemon of some release on understands. A daemon older
// than v may take a request it cannot honour without saying so, as a bridge that predates
// an option ignores it, so the version is the only thing that can be asked.
func (c *Client) Speaks(v string) bool {
	spoken, err := parseAPIVersion(c.version)
	if err != nil {
		return false
	}
	wanted, err := parseAPIVersion(v)
	if err != nil {
		return false
	}
	return !spoken.before(wanted)
}
