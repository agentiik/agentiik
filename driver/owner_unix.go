//go:build unix

package driver

import (
	"io/fs"
	"syscall"
)

// ownerOf reads the account that owns a file, where the platform says.
func ownerOf(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
