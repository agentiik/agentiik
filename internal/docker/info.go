package docker

import (
	"context"
	"net/http"
	"strings"
)

// Info is the daemon describing itself. Two of its fields are why this call exists.
//
// SecurityOptions is where user namespace remapping is read: a daemon with the remapping
// on carries name=userns in the list, and a daemon without it does not. That is the
// floor the runner refuses to start under.
//
// DockerRootDir ends in <uid>.<gid> when the remapping is on, which is the one place the
// remapped range is readable without parsing /etc/subuid, and it is the ownership a
// task's working directory has to be given before the container is created.
type Info struct {
	ID              string   `json:"ID"`
	Name            string   `json:"Name,omitempty"`
	ServerVersion   string   `json:"ServerVersion,omitempty"`
	SecurityOptions []string `json:"SecurityOptions,omitempty"`
	DockerRootDir   string   `json:"DockerRootDir,omitempty"`
	OSType          string   `json:"OSType,omitempty"`
	Architecture    string   `json:"Architecture,omitempty"`
	NCPU            int      `json:"NCPU,omitempty"`
	MemTotal        int64    `json:"MemTotal,omitempty"`
}

// Info asks the daemon what it is.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	if err := c.call(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// UsernsRemapped says whether the daemon remaps user namespaces.
//
// The daemon writes its security options as a list of comma-separated key=value strings,
// name=userns among them, and this reads that list rather than a boolean nobody sends.
// It is here rather than in the driver because it is a fact about the wire: which string
// a daemon writes is this package's business, and what to do when it is missing is the
// driver's.
func (i Info) UsernsRemapped() bool {
	for _, option := range i.SecurityOptions {
		for _, field := range strings.Split(option, ",") {
			if strings.TrimSpace(field) == "name=userns" {
				return true
			}
		}
	}
	return false
}

// RemappedRange is the uid and gid a remapped daemon owns its root directory with, read
// off the <uid>.<gid> suffix the daemon appends to DockerRootDir when the remapping is
// on.
//
// It answers false where the suffix is absent, which is every daemon without the
// remapping and any future daemon that stops writing it. A caller that cannot read the
// range must refuse rather than guess at one, because a working directory chowned to the
// wrong account is a container that silently fails to write its outputs.
func (i Info) RemappedRange() (uid, gid string, ok bool) {
	base := i.DockerRootDir
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}
	uid, gid, ok = strings.Cut(base, ".")
	if !ok || uid == "" || gid == "" {
		return "", "", false
	}
	if !allDigits(uid) || !allDigits(gid) {
		return "", "", false
	}
	return uid, gid, true
}

// allDigits says whether every byte is a decimal digit, which is what an account number
// written into a directory name is.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
