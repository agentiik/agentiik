package docker

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// VolumeSpec is one volume to create. The driver's options are where a local volume is
// told to be a tmpfs rather than a directory on the daemon's disk.
type VolumeSpec struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	DriverOpts map[string]string `json:"DriverOpts,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// Volume is one volume as the daemon describes it.
//
// Options are the ones it was created with. A create naming a volume that is already there
// answers with that volume and its own options, whatever the create asked for, which is how
// a caller tells the volume it asked for from one somebody else made under the name.
type Volume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver,omitempty"`
	Options   map[string]string `json:"Options,omitempty"`
	Labels    map[string]string `json:"Labels,omitempty"`
	CreatedAt time.Time         `json:"CreatedAt"`
}

// VolumeCreate creates one volume, or answers with the one already there under its name.
func (c *Client) VolumeCreate(ctx context.Context, spec VolumeSpec) (Volume, error) {
	var v Volume
	if err := c.call(ctx, http.MethodPost, "/volumes/create", nil, spec, &v); err != nil {
		return Volume{}, err
	}
	return v, nil
}

// VolumeList lists the volumes matching the filters, which is how a volume an earlier
// process left is found by its label.
func (c *Client) VolumeList(ctx context.Context, f Filters) ([]Volume, error) {
	q := url.Values{}
	f.encode(q)

	var list struct {
		Volumes []Volume `json:"Volumes"`
	}
	if err := c.call(ctx, http.MethodGet, "/volumes", q, nil, &list); err != nil {
		return nil, err
	}
	return list.Volumes, nil
}

// VolumeRemove removes one volume. A volume a container still names is refused with a 409,
// created or running, and without force, which is the refusal a caller relies on to leave
// a volume a container still needs.
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
}
