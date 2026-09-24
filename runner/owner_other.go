//go:build !unix

package runner

import "io/fs"

// ownerOf is the account that owns a file, which a platform without unix accounts does not say.
// A runner runs on Linux alone, and this is so that the package still builds where agk does.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
