// Package freeport finds a TCP port this user can start a server on: one that no listener or
// container in a snapshot holds and that the OS lets this user bind right now.
package freeport

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/model"
)

// span is how many ports a search tries, from included: a wider range would hide a whole
// block of taken ports behind one answer.
const span = 100

// Prober reports whether this user can bind port now (see Probe).
type Prober func(port uint16) (free bool, err error)

// Last is the last port the search from `from` tries: min(from+99, 65535).
func Last(from uint16) uint16 {
	return uint16(min(int(from)+span-1, 65535))
}

// Find returns the first port in from..Last(from) that s does not hold (no listener on any
// address, no container publishing it on any host address, tcp) and that probe reports free;
// ok is false when none is; err is the first probe error (the search stops there).
//
// The snapshot comes first because a port published only by iptables (Docker without its
// userland proxy) has no socket for the probe's bind to hit; the probe catches what the
// snapshot cannot see, such as another user's listener when devdash runs without root.
func Find(s model.Snapshot, from uint16, probe Prober) (port uint16, ok bool, err error) {
	held := map[uint16]bool{}
	for _, p := range s.Processes {
		for _, l := range p.Listeners {
			// Only UDP is skipped (planned for v1.2), so a listener of any other proto holds the port.
			if !strings.HasPrefix(l.Proto, "udp") {
				held[l.Port] = true
			}
		}
	}
	for _, c := range s.Containers {
		for _, m := range c.Ports {
			if m.Proto == "tcp" {
				held[m.HostPort] = true // any host IP, an invalid one (every interface) included
			}
		}
	}
	for p := int(from); p <= int(Last(from)); p++ {
		if held[uint16(p)] {
			continue
		}
		free, err := probe(uint16(p))
		if err != nil {
			return 0, false, err
		}
		if free {
			return uint16(p), true, nil
		}
	}
	return 0, false, nil
}

// Probe is the real Prober. It binds a TCP socket to 0.0.0.0:port, then an IPV6_V6ONLY one to
// [::]:port, and closes both: the bind fails beside any listener on the port, whatever user
// owns it and whatever address it is on, so it catches the listeners of other users that a
// snapshot without root cannot see. Raw sockets, not net.Listen: net.Listen sets SO_REUSEADDR
// everywhere, and on macOS that lets a wildcard bind succeed beside another socket's bind to a
// specific address. SO_REUSEADDR is set on Linux only (reuseAddr), where it admits a port in
// TIME_WAIT, as a dev server's own bind would, and still fails beside a listener; on macOS a
// port in TIME_WAIT counts as taken, the safe side.
//
// A bind that fails with EADDRINUSE, or EACCES (below 1024), means not free. EAFNOSUPPORT or
// EADDRNOTAVAIL from the IPv6 socket or bind skips the IPv6 check (a host without IPv6). Any
// other failure is an error, so a probe that could not look never reads as an answer.
func Probe(port uint16) (bool, error) {
	fd4, err := bound(unix.AF_INET, &unix.SockaddrInet4{Port: int(port)})
	switch {
	case taken(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("probing port %d: %w", port, err)
	}
	defer func() { _ = unix.Close(fd4) }()

	fd6, err := bound(unix.AF_INET6, &unix.SockaddrInet6{Port: int(port)})
	switch {
	case taken(err):
		return false, nil
	case noIPv6(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("probing port %d: %w", port, err)
	}
	_ = unix.Close(fd6)
	return true, nil
}

// bound is a new TCP socket of domain bound to sa, with SO_REUSEADDR where reuseAddr says so
// and IPV6_V6ONLY for AF_INET6, so the IPv6 bind does not collide with the IPv4 one. The error
// is an *os.SyscallError naming the call that failed; the socket is closed then.
func bound(domain int, sa unix.Sockaddr) (int, error) {
	fd, err := socket(domain)
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	if reuseAddr {
		err = os.NewSyscallError("setsockopt SO_REUSEADDR", unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1))
	}
	if err == nil && domain == unix.AF_INET6 {
		err = os.NewSyscallError("setsockopt IPV6_V6ONLY", unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1))
	}
	if err == nil {
		err = os.NewSyscallError("bind", unix.Bind(fd, sa))
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// taken reports whether err is a bind that failed because the port is in use or reserved for root.
func taken(err error) bool {
	se, ok := errors.AsType[*os.SyscallError](err)
	return ok && se.Syscall == "bind" && (errors.Is(err, unix.EADDRINUSE) || errors.Is(err, unix.EACCES))
}

// noIPv6 reports whether err is a socket or bind that failed because the host has no IPv6.
func noIPv6(err error) bool {
	se, ok := errors.AsType[*os.SyscallError](err)
	return ok && (se.Syscall == "socket" || se.Syscall == "bind") &&
		(errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EADDRNOTAVAIL))
}
