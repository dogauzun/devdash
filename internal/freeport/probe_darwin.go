package freeport

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseAddr: on macOS SO_REUSEADDR would let the wildcard bind succeed beside another socket's
// bind to a specific address, so the probe goes without it and a port in TIME_WAIT counts as
// taken.
const reuseAddr = false

// socket is a close-on-exec TCP socket of domain. macOS has no SOCK_CLOEXEC, so the flag is set
// under ForkLock, as the net package does, so no child forked meanwhile inherits the socket.
func socket(domain int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Socket(domain, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	return fd, err
}
