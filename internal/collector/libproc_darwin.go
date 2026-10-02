//go:build darwin

package collector

import (
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libproc and sysctl are called through purego so the darwin build stays CGO_ENABLED=0.
// SyscallN captures errno on darwin, which is how EPERM (other uid) is told apart from ESRCH (exited).

// Flavors and sizes from sys/proc_info.h (MacOSX27.sdk), sizes checked with clang offsetof.
const (
	procPidListFDs         = 1 // PROC_PIDLISTFDS, array of struct proc_fdinfo
	procPidTaskInfo        = 4 // PROC_PIDTASKINFO, struct proc_taskinfo
	procPidVnodePathInfo   = 9 // PROC_PIDVNODEPATHINFO, struct proc_vnodepathinfo
	procPidFDSocketInfo    = 3 // PROC_PIDFDSOCKETINFO, struct socket_fdinfo
	sizeofProcFDInfo       = 8
	sizeofProcTaskInfo     = 96
	sizeofVnodePathInfo    = 2352
	sizeofSocketFDInfo     = 792
	ctlKern, kernProcArgs2 = 1, 49 // CTL_KERN, KERN_PROCARGS2 from sys/sysctl.h
)

type libSystem struct {
	procPidinfo, procPidfdinfo, sysctl uintptr
	timebaseNumer, timebaseDenom       uint64 // mach_timebase_info: mach ticks * numer / denom = ns
}

var loadLibSystem = sync.OnceValues(func() (*libSystem, error) {
	h, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	l := &libSystem{}
	for name, p := range map[string]*uintptr{"proc_pidinfo": &l.procPidinfo, "proc_pidfdinfo": &l.procPidfdinfo, "sysctl": &l.sysctl} {
		if *p, err = purego.Dlsym(h, name); err != nil {
			return nil, err
		}
	}
	tb, err := purego.Dlsym(h, "mach_timebase_info")
	if err != nil {
		return nil, err
	}
	var info [2]uint32 // struct mach_timebase_info { uint32_t numer, denom; }
	if r, _, _ := purego.SyscallN(tb, uintptr(unsafe.Pointer(&info))); r != 0 || info[1] == 0 {
		info = [2]uint32{1, 1}
	}
	l.timebaseNumer, l.timebaseDenom = uint64(info[0]), uint64(info[1])
	return l, nil
})

// pidinfo calls proc_pidinfo and returns the number of bytes written.
func (l *libSystem) pidinfo(pid, flavor int, buf []byte) (int, error) {
	r, _, e := purego.SyscallN(l.procPidinfo, uintptr(pid), uintptr(flavor), 0,
		uintptr(unsafe.Pointer(unsafe.SliceData(buf))), uintptr(len(buf)))
	return result(r, e)
}

// pidfdinfo calls proc_pidfdinfo and returns the number of bytes written.
func (l *libSystem) pidfdinfo(pid, fd, flavor int, buf []byte) (int, error) {
	r, _, e := purego.SyscallN(l.procPidfdinfo, uintptr(pid), uintptr(fd), uintptr(flavor),
		uintptr(unsafe.Pointer(unsafe.SliceData(buf))), uintptr(len(buf)))
	return result(r, e)
}

// procArgs2 reads sysctl kern.procargs2.<pid> into buf (sized kern.argmax) without allocating.
func (l *libSystem) procArgs2(pid int, buf []byte) (int, error) {
	mib := [3]int32{ctlKern, kernProcArgs2, int32(pid)}
	n := uintptr(len(buf))
	r, _, e := purego.SyscallN(l.sysctl, uintptr(unsafe.Pointer(&mib)), 3,
		uintptr(unsafe.Pointer(unsafe.SliceData(buf))), uintptr(unsafe.Pointer(&n)), 0, 0)
	if int32(r) != 0 {
		return 0, syscall.Errno(uint32(e))
	}
	return int(n), nil
}

// result maps a C int return (libproc returns 0 or -1 on failure) to (n, errno).
func result(r, e uintptr) (int, error) {
	if n := int32(r); n > 0 {
		return int(n), nil
	}
	if errno := syscall.Errno(uint32(e)); errno != 0 {
		return 0, errno
	}
	return 0, syscall.EINVAL // failed without errno (for example a short struct)
}

// machToNs converts mach absolute time units (not ns on arm64) to nanoseconds.
func (l *libSystem) machToNs(ticks uint64) uint64 {
	return ticks/l.timebaseDenom*l.timebaseNumer + ticks%l.timebaseDenom*l.timebaseNumer/l.timebaseDenom
}
