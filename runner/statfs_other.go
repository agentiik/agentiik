//go:build !linux && !darwin

package runner

import "errors"

// statfsAvailable is refused where the platform's statfs is not read. A runner runs on Linux
// alone, and this is so that the package still builds where agk does.
func statfsAvailable(string) (int64, error) {
	return 0, errors.New("this platform's filesystems are not measured, and a runner runs on Linux")
}
