package helper

// None is what --helper=none is written as: mount nothing, deliberately.
//
// It is a spelling and not the empty string, because an empty --helper is a flag somebody
// passed a variable that was not set and a run that bound nothing on that would be a run that
// silently dropped what the person asked for.
const None = "none"

// Resolve says which file is bound read-only at /agk/bin/agk, in one order so that no command
// takes it differently.
//
//	--helper <path>   the file to mount, whatever it is
//	--helper=none     mount nothing, deliberately
//	$AGK_HELPER       the same, for a machine that keeps one in a fixed place
//	the embedded binary for the daemon's platform, laid down in dir at 0555
//	nothing, and the caller says so once
//
// The architecture is the daemon's own and never this host's, which is why it arrives as an
// argument: that is the architecture a container runs natively, and an amd64 binary bound into
// an arm64 container gives a script an exec format error rather than a program.
//
// Nothing searches beside os.Executable. That would be a guess about how somebody installed
// this binary, and the embedded copy is what makes the ordinary case work without one.
//
// Anything about to be bound is checked first, and a file that fails refuses: naming the path
// and what was found is worth more than a mount that makes every script step say agk: not
// found. Finding nothing at all is not a failure, because the helper "is a convenience, never
// a requirement; jq and a redirect do the same job".
func Resolve(flagValue, env, ostype, arch, dir string) (string, bool, error) {
	switch {
	case flagValue == None:
		return "", false, nil
	case flagValue != "":
		if err := Check(flagValue, ostype, arch); err != nil {
			return "", false, err
		}
		return flagValue, true, nil
	case env == None:
		return "", false, nil
	case env != "":
		if err := Check(env, ostype, arch); err != nil {
			return "", false, err
		}
		return env, true, nil
	}

	path, ok, err := Extract(dir, ostype, arch)
	if err != nil || !ok {
		return "", false, err
	}
	// Checked even though these are this build's own bytes. The check is about what is
	// being bound rather than about where it came from, and a release that shipped the
	// wrong architecture under a name should be found here rather than inside a container.
	if err := Check(path, ostype, arch); err != nil {
		return "", false, err
	}
	return path, true, nil
}
