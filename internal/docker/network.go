package docker

import (
	"context"
	"net/http"
	"net/url"
)

// NetworkSpec is one network to create. Every task gets its own, so two containers on
// one host never see each other whatever the posture the step asked for.
//
// Internal is the network with no outbound route, which is what network: internal means:
// a step that talks to a sidecar and to nothing else.
type NetworkSpec struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	Internal   bool              `json:"Internal,omitempty"`
	Attachable bool              `json:"Attachable,omitempty"`
	EnableIPv6 bool              `json:"EnableIPv6,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// NetworkCreated is what a create answered.
type NetworkCreated struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning,omitempty"`
}

// NetworkSummary is one network of a list, which is how a network left behind by an
// earlier process is found by its label.
type NetworkSummary struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver,omitempty"`
	Internal bool              `json:"Internal,omitempty"`
	Labels   map[string]string `json:"Labels,omitempty"`
}

// NetworkCreate creates one network.
func (c *Client) NetworkCreate(ctx context.Context, spec NetworkSpec) (NetworkCreated, error) {
	var created NetworkCreated
	if err := c.call(ctx, http.MethodPost, "/networks/create", nil, spec, &created); err != nil {
		return NetworkCreated{}, err
	}
	return created, nil
}

// NetworkList lists the networks matching the filters.
func (c *Client) NetworkList(ctx context.Context, f Filters) ([]NetworkSummary, error) {
	q := url.Values{}
	f.encode(q)

	var list []NetworkSummary
	if err := c.call(ctx, http.MethodGet, "/networks", q, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// NetworkRemove removes one network. It is removed with the container that used it, in
// the defer that runs on every path, so that a host does not accumulate one network per
// task that ever ran on it.
func (c *Client) NetworkRemove(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/networks/"+id, nil, nil, nil)
}
