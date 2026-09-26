//go:build !unix

package driver

import "io/fs"

// ownerOf answers that the platform does not say, which a test then does not ask.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
