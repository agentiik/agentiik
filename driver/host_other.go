//go:build !linux

package driver

import "os"

// effectiveCapabilities answers for a platform with no Linux capabilities, where the one
// account that may chown to another uid is root. A daemon that remaps is a Linux daemon,
// and a process reaching it from elsewhere prepares nothing it could chown.
func effectiveCapabilities() (uint64, error) {
	if os.Geteuid() == 0 {
		return ^uint64(0), nil
	}
	return 0, nil
}

// filesystemOf answers for a platform with no tmpfs of Linux's, which is every directory
// failing the secrets floor. macOS is the case that matters, and agk run --local, which
// runs there, lifts the floor.
func filesystemOf(dir string) (Filesystem, error) {
	if _, err := os.Stat(dir); err != nil {
		return Filesystem{}, err
	}
	return Filesystem{}, nil
}
