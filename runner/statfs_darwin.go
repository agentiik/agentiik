package runner

import (
	"math"
	"syscall"
)

// statfsAvailable is the bytes the filesystem at path has for an account that is not root. macOS
// counts f_bavail in blocks of f_bsize. A runner runs on Linux alone, and this is so that join's
// tests measure a real disk on the machine they were written on.
func statfsAvailable(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	unit := uint64(st.Bsize)
	if unit != 0 && st.Bavail > math.MaxInt64/unit {
		return math.MaxInt64, nil
	}
	return int64(st.Bavail * unit), nil
}
