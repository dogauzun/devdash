package freeport

import "golang.org/x/sys/unix"

// reuseAddr: on Linux SO_REUSEADDR lets the probe bind a port in TIME_WAIT, as a dev server's
// own bind would, and the bind still fails beside any listener.
const reuseAddr = true

// socket is a close-on-exec TCP socket of domain.
func socket(domain int) (int, error) {
	return unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}
