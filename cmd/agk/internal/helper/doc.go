// Package helper is where the static /agk/bin/agk is carried, found, checked and laid
// down. The binary itself is cmd/agk-helper; this is the half that belongs to the laptop.
//
// The driver already does the mounting and needs no change for it: driver.Policy.Helper
// names the file on this host, driver/mounts.go stats it and refuses a path that is not a
// regular file rather than letting the daemon create a directory where a program was
// promised, and binds it read-only at driver.BinPath for a script step. What this package
// adds is where that file comes from when nobody installed one.
//
// # How one is found
//
// In order, and the order stops at the first answer:
//
//	--helper <path>   the file to mount, whatever it is
//	--helper=none     mount nothing, deliberately
//	$AGK_HELPER       the same, for a machine that has one in a fixed place
//	the embedded binary for the daemon's platform, extracted into the run's working
//	    directory at 0555
//	nothing, said once
//
// The architecture is the daemon's own, read through driver.Probe before driver.New,
// because that is the architecture a container runs natively. An amd64 binary bound into an
// arm64 container gives a script exec format error rather than a program, which is a
// failure nobody can read. Probe answers before New because Policy.Helper has to be set
// before New, and because asking the daemon through driver keeps cmd/agk off the Engine
// API, which internal/docker would otherwise let it reach since internal is module-wide.
//
// Nothing searches beside os.Executable. A guess about how somebody installed the binary is
// a guess, and the embedded copy is what makes the ordinary case work without one.
//
// # What is checked before anything is mounted
//
// A helper that cannot run is worse than no helper, because the step says agk: not found
// either way and one of the two took a mount to say it. So a file that is going to be bound
// is read first: a regular file, a linux ELF, the machine the daemon named, and no
// PT_INTERP, which is what "static" means spelled as something a machine can check through
// debug/elf rather than asserted in a sentence. A file that fails refuses the run naming the
// path, what was found and what /agk/bin/agk has to be.
//
// Finding nothing is not a failure. driver/doc.go and driver.Policy.Helper both already
// describe a runner with none to offer as one that binds nothing, and the page says the
// helper "is a convenience, never a requirement; jq and a redirect do the same job". So
// nothing is bound and one sentence says so, once.
//
// # How the embedded copy gets there
//
// An embed.FS over bin/, holding agk-linux-amd64 and agk-linux-arm64 as the release build
// puts them there. A build that did not run that stage has neither, Extract answers that
// there is none, Policy.Helper stays empty and the driver binds nothing, which is the
// documented behaviour rather than a broken build.
//
// bin/ carries a committed README naming what goes in it, because //go:embed refuses a
// directory it matches nothing in, and a build of this module on a machine that has never
// built the helper has to compile. That file is why the directive can be unconditional.
//
// Extracting rather than binding the embedded bytes directly is not avoidable: a bind mount
// needs a path on the host, and bytes in a binary have none. It is written at 0555 into the
// working directory, once per directory rather than once per run, and the driver's own stat
// is still the thing that decides whether it gets mounted.
//
// # What this package refuses to be
//
// It holds no daemon handle, opens no socket and reads no policy file. It is handed the
// daemon's OSType and Architecture as two strings and answers with a path or with nothing.
//
// # Layout
//
//	embed.go     the embed.FS and Embedded, which says what this build carries
//	extract.go   Extract, which lays one down and answers with its path
//	check.go     the regular file, the ELF machine and the absent PT_INTERP
//	resolve.go   the order above, as one function so that no command takes it differently
//	bin/         where the built binaries are put, and a README saying so
package helper
