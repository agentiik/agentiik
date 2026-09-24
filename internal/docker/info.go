package docker

import (
	"context"
	"net/http"
	"strings"
)

// Info is the daemon describing itself. Two of its fields are why this call exists.
//
// SecurityOptions is where the daemon says how it confines a container, one option per
// mechanism: name=userns where user namespace remapping is on, which is the floor the
// runner refuses to start under, and name=seccomp, name=apparmor and name=selinux for the
// three profiles the settings table's SecurityOpt row speaks of. An option the daemon does
// not list is a mechanism it does not apply.
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

	// DefaultRuntime is the runtime a container that names none is created with, runc
	// unless the daemon was configured otherwise. It is what a runner reports at join as
	// the runtime its containers run under.
	DefaultRuntime string `json:"DefaultRuntime,omitempty"`
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
	_, ok := i.securityOption("userns")
	return ok
}

// SeccompProfile is the seccomp profile the daemon gives a container that names none, and
// whether the daemon filters system calls at all.
//
// The daemon lists name=seccomp,profile=<p> where it was built with seccomp and the kernel
// offers it, and leaves the option out otherwise. The profile is builtin for Docker's own
// default, the path of the file it was started with where an operator named one, and
// unconfined where it was started with --seccomp-profile=unconfined, which lists the
// option and filters nothing: a daemon that answers ok with unconfined confines no
// container that does not bring a profile of its own.
func (i Info) SeccompProfile() (profile string, ok bool) {
	fields, ok := i.securityOption("seccomp")
	if !ok {
		return "", false
	}
	return fields["profile"], true
}

// AppArmor says whether the daemon confines a container with an AppArmor profile, which it
// does, and lists name=apparmor for, wherever the host kernel has AppArmor enabled.
func (i Info) AppArmor() bool {
	_, ok := i.securityOption("apparmor")
	return ok
}

// SELinux says whether the daemon labels a container for SELinux. It lists name=selinux
// only where it was started with --selinux-enabled on a host where SELinux is enabled,
// which is narrower than the host having SELinux: a daemon started without the flag labels
// nothing on a host that has it.
func (i Info) SELinux() bool {
	_, ok := i.securityOption("selinux")
	return ok
}

// securityOption finds the option called name among the daemon's security options and
// answers its fields: name=seccomp,profile=builtin is the option seccomp, whose profile
// field is builtin.
func (i Info) securityOption(name string) (map[string]string, bool) {
	for _, option := range i.SecurityOptions {
		fields := map[string]string{}
		for _, field := range strings.Split(option, ",") {
			key, value, _ := strings.Cut(strings.TrimSpace(field), "=")
			fields[key] = value
		}
		if fields["name"] == name {
			return fields, true
		}
	}
	return nil, false
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
