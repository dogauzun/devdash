package model

import (
	"slices"
	"strings"
)

// Holder is one holder of a TCP port: a process, the PID 0 unknown owner included, or a
// container publishing the port.
type Holder struct {
	Key     RowKey   // the process's key, or RowKey{ContainerID: id} for a container
	Process *Process // the holding process, in the snapshot; nil for a container
}

// TCP reports whether l holds a TCP port: any proto but udp*. It is "not UDP" rather than "is
// tcp" so that a listener of a proto the collectors do not emit yet still holds its port: a
// free port that is not free is the worse mistake.
func (l Listener) TCP() bool { return !strings.HasPrefix(l.Proto, "udp") }

// TCP reports whether m publishes a TCP port: its proto is exactly "tcp", as Docker writes it.
func (m PortMapping) TCP() bool { return m.Proto == "tcp" }

// Holders returns the holders of TCP port n in s, as `kill N` targets them and checks the port
// after the kill: each process with a listener on n, in snapshot order, and each container
// publishing n over tcp, once. A listener reconciled to a container (Listener.ContainerID)
// stands for that container, not for its holder, which is a port proxy or an unknown owner
// (Docker Desktop's com.docker.backend holds several containers' ports in one process); the
// holder is one too only when it also holds a socket on n that matched no container. The
// containers a listener stands for come at its holder's place, the others after every process.
// UDP listeners and mappings hold nothing (UDP is not shown yet): see Listener.TCP and
// PortMapping.TCP.
func Holders(s Snapshot, n uint16) []Holder {
	var hs []Holder
	seen := map[string]bool{}
	container := func(id string) {
		if !seen[id] {
			seen[id] = true
			hs = append(hs, Holder{Key: RowKey{ContainerID: id}})
		}
	}
	for i := range s.Processes {
		p := &s.Processes[i]
		own := false
		for _, l := range p.Listeners {
			switch {
			case l.Port != n || !l.TCP():
			case l.ContainerID != "":
				container(l.ContainerID)
			case !own:
				own = true
				hs = append(hs, Holder{Key: p.Key(), Process: p})
			}
		}
	}
	for _, c := range s.Containers {
		if slices.ContainsFunc(c.Ports, func(m PortMapping) bool { return m.HostPort == n && m.TCP() }) {
			container(c.ID)
		}
	}
	return hs
}
