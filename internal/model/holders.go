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

// Holders returns the holders of TCP port n in s, as `kill N` targets them and checks the port
// after the kill: each process with a listener on n, in snapshot order, and each container
// publishing n over tcp, once. A listener reconciled to a container (Listener.ContainerID)
// stands for that container, not for its holder, which is a port proxy or an unknown owner
// (Docker Desktop's com.docker.backend holds several containers' ports in one process); the
// holder is one too only when it also holds a socket on n that matched no container. The
// containers a listener stands for come at its holder's place, the others after every process.
// UDP listeners and mappings hold nothing (UDP is planned for v1.2).
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
			case l.Port != n || strings.HasPrefix(l.Proto, "udp"):
			case l.ContainerID != "":
				container(l.ContainerID)
			case !own:
				own = true
				hs = append(hs, Holder{Key: p.Key(), Process: p})
			}
		}
	}
	for _, c := range s.Containers {
		if slices.ContainsFunc(c.Ports, func(m PortMapping) bool { return m.HostPort == n && m.Proto == "tcp" }) {
			container(c.ID)
		}
	}
	return hs
}
