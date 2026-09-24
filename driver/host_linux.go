package driver

import (
	"syscall"
	"unsafe"
)

// linuxCapabilityVersion3 is _LINUX_CAPABILITY_VERSION_3, the version of capget's header
// that carries the sets as two 32-bit words each, which is every capability the kernel
// has.
const linuxCapabilityVersion3 = 0x20080522

// effectiveCapabilities asks the kernel for this process's effective set with capget.
//
// The effective set and not the permitted one, because it is the set a chown is checked
// against. A unit's AmbientCapabilities puts the three there for an account that is not
// root, and root holds every one of them already.
func effectiveCapabilities() (uint64, error) {
	header := struct {
		version uint32
		pid     int32
	}{version: linuxCapabilityVersion3}
	var data [2]struct {
		effective   uint32
		permitted   uint32
		inheritable uint32
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return 0, errno
	}
	return uint64(data[0].effective) | uint64(data[1].effective)<<32, nil
}

// tmpfsMagic is TMPFS_MAGIC, the f_type statfs answers for a tmpfs.
const tmpfsMagic = 0x01021994

// The ST_ flags statfs answers in f_flags, which carry the mount's own flags.
const (
	stNoSUID = 0x2
	stNoDev  = 0x4
	stNoExec = 0x8
)

// filesystemOf asks statfs what the mount a directory sits on is.
func filesystemOf(dir string) (Filesystem, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return Filesystem{}, err
	}
	flags := uint64(s.Flags)
	return Filesystem{
		Tmpfs:  uint64(s.Type) == tmpfsMagic,
		NoExec: flags&stNoExec != 0,
		NoSUID: flags&stNoSUID != 0,
		NoDev:  flags&stNoDev != 0,
	}, nil
}
