package model

import "net/netip"

// Reconcile matches listeners to published container ports (spec "Docker integration",
// Reconciliation). A listener matches a tcp mapping with a host port when the ports are equal
// and its owner is a port proxy (proxyNames) or unknown (PID 0), tried in order: the bind
// address equals the mapping's host IP (an unspecified listener also takes an empty host IP,
// which Podman reports for every interface of either family); an unspecified listener takes the
// other family's every-interface mapping (Docker Desktop holds 0.0.0.0 mappings on [::]); an
// unspecified listener takes a specific mapping on its port only when they are all one
// container's. A specific listener never takes an every-interface mapping.
//
// A matched listener gets ContainerID, and its process kind container. The process itself gets
// ContainerID only when every one of its listeners is the same container's: Docker Desktop's
// com.docker.backend and OrbStack Helper hold every container's ports next to ports of their
// own, so such a process stays its own row and each container becomes its own row through
// Flatten. Containers with no matching listener stay only in
// Snapshot.Containers; Flatten turns them into rows. Reconcile modifies procs and their
// Listeners in place (Build passes fresh slices) and returns procs.
func Reconcile(procs []Process, containers []Container) []Process {
	type mapping struct {
		id string
		ip netip.Addr // invalid for every interface of either family (Podman's empty host IP)
	}
	byPort := map[uint16][]mapping{}
	for _, c := range containers {
		for _, m := range c.Ports {
			if m.Proto != "tcp" || m.HostPort == 0 {
				continue
			}
			byPort[m.HostPort] = append(byPort[m.HostPort], mapping{c.ID, m.HostIP.Unmap()})
		}
	}
	if len(byPort) == 0 {
		return procs
	}
	match := func(l Listener) string {
		addr, ms := l.Addr.Unmap(), byPort[l.Port]
		for _, m := range ms {
			if addr == m.ip || addr.IsUnspecified() && !m.ip.IsValid() {
				return m.id
			}
		}
		if !addr.IsUnspecified() {
			return ""
		}
		for _, m := range ms {
			if m.ip.IsUnspecified() {
				return m.id // the other family's: the same family's matched above
			}
		}
		owner := ""
		for _, m := range ms {
			if owner != "" && owner != m.id {
				return "" // specific mappings of several containers: no way to tell which
			}
			owner = m.id
		}
		return owner
	}
	for i := range procs {
		p := &procs[i]
		if p.PID != 0 && !isProxy(*p) {
			continue
		}
		// whole: every socket p holds is owner's. A second container's socket, or one of its
		// own (OrbStack Helper's 32222, Docker Desktop's Kubernetes on 6443), keeps p its own row.
		owner, whole := "", true
		for j := range p.Listeners {
			id := match(p.Listeners[j])
			if id == "" || owner != "" && owner != id {
				whole = false
			}
			if id == "" {
				continue
			}
			p.Listeners[j].ContainerID = id
			owner = id
		}
		if owner == "" {
			continue
		}
		p.Kind = KindContainer
		if whole {
			p.ContainerID = owner
		}
	}
	return procs
}
