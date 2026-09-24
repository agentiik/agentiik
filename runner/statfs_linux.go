package runner

import (
	"math"
	"syscall"
)

// statfsAvailable is the bytes the filesystem at path has for an account that is not root.
//
// Linux counts f_bavail in fragments of f_frsize, which is the block size on every filesystem that
// does not say otherwise, and f_bsize is only the preferred size of a transfer.
func statfsAvailable(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	unit := uint64(st.Frsize)
	if unit == 0 {
		unit = uint64(st.Bsize)
	}
	if unit != 0 && st.Bavail > math.MaxInt64/unit {
		return math.MaxInt64, nil
	}
	return int64(st.Bavail * unit), nil
}
