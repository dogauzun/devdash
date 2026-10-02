package model

import "net/netip"

// Reconcile matches listeners to published container ports (spec "Docker integration",
// Reconciliation). A listener matches a tcp mapping with a host port when the ports are equal
// and its owner is a port proxy (proxyNames) or unknown (PID 0), and its bind address equals
// the mapping's host IP or, failing any such mapping, is unspecified; an unspecified listener
// prefers a mapping on every interface (an empty host IP counts as one, as Podman reports it).
//
// A matched listener gets ContainerID, and its process kind container. The process itself gets
// ContainerID only when all its matched listeners are one container's: Docker Desktop's
// com.docker.backend holds every container's ports, so it stays one row and each container
// becomes its own row through Flatten. Containers with no matching listener stay only in
// Snapshot.Containers; Flatten turns them into rows. Reconcile modifies procs and their
// Listeners in place (Build passes fresh slices) and returns procs.
func Reconcile(procs []Process, containers []Container) []Process {
	type mapping struct {
		id string
		ip netip.Addr // unspecified for every interface
	}
	byPort := map[uint16][]mapping{}
	for _, c := range containers {
		for _, m := range c.Ports {
			if m.Proto != "tcp" || m.HostPort == 0 {
				continue
			}
			ip := m.HostIP.Unmap()
			if !ip.IsValid() {
				ip = netip.IPv4Unspecified()
			}
			byPort[m.HostPort] = append(byPort[m.HostPort], mapping{c.ID, ip})
		}
	}
	if len(byPort) == 0 {
		return procs
	}
	match := func(l Listener) string {
		addr := l.Addr.Unmap()
		fallback := ""
		for _, m := range byPort[l.Port] {
			switch {
			case addr == m.ip || addr.IsUnspecified() && m.ip.IsUnspecified():
				return m.id
			case addr.IsUnspecified() && fallback == "":
				fallback = m.id
			}
		}
		return fallback
	}
	for i := range procs {
		p := &procs[i]
		if p.PID != 0 && !isProxy(*p) {
			continue
		}
		owner, shared := "", false
		for j := range p.Listeners {
			id := match(p.Listeners[j])
			if id == "" {
				continue
			}
			p.Listeners[j].ContainerID = id
			if owner != "" && owner != id {
				shared = true
			}
			owner = id
		}
		if owner == "" {
			continue
		}
		p.Kind = KindContainer
		if !shared {
			p.ContainerID = owner
		}
	}
	return procs
}
