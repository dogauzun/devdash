package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// runPort answers "who has port N" from one snapshot (no CPU, so one sample): exit 0 and one
// line per listener when found, exit 1 and "free" when nothing listens.
func runPort(ctx context.Context, o engine.Options, port uint16, stdout, stderr io.Writer) int {
	s, err := engine.Snapshot(ctx, o)
	var found bool
	if err == nil {
		found, err = writePort(stdout, s, port)
	}
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	case !found:
		return 1
	}
	return 0
}

// writePort prints one line per listener on port: pid, name, project (or -) and bind
// address. A socket bound to both IPv4 and IPv6 (dual-stack ::) is one line; two sockets are
// two lines. A socket reconciled to a container prints the container instead of its holder:
// the holder's pid (- when unknown), the container's name, its compose project (or -), the
// address, then its image and the proxy holding the socket. A container that publishes port
// with no socket on it (iptables only) gets one line per published address, with pid -. It
// prints "free" and returns false when nothing listens on port.
func writePort(w io.Writer, s model.Snapshot, port uint16) (bool, error) {
	projects := map[string]string{}
	for _, p := range s.Projects {
		projects[p.ID] = p.Name
	}
	hint := "run with sudo to see it"
	for _, x := range s.Warnings {
		if x.Code == "listener_owner_unreadable" {
			hint = x.Hint
		}
	}
	containers := map[string]model.Container{}
	for _, c := range s.Containers {
		containers[c.ID] = c
	}
	// container is the name, project and description columns of id's line.
	container := func(id string) (string, string, string) {
		c, ok := containers[id]
		if !ok {
			c = model.Container{ID: id}
		}
		name, project, desc := c.Name, c.ComposeProject, "container"
		if name == "" {
			name = c.ID
		}
		if project == "" {
			project = "-"
		}
		if c.Image != "" {
			desc += " (" + c.Image + ")"
		}
		return name, project, desc
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	found := false
	held := map[string]bool{} // containers with a socket on port
	for _, p := range s.Processes {
		for _, l := range p.Listeners {
			if l.Port != port {
				continue
			}
			found = true
			addr := netip.AddrPortFrom(l.Addr, l.Port)
			if l.ContainerID != "" {
				held[l.ContainerID] = true
				name, project, desc := container(l.ContainerID)
				pid := "-"
				if p.PID != 0 {
					pid, desc = strconv.Itoa(p.PID), desc+" via "+p.Name
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", pid, name, project, addr, desc)
				continue
			}
			project := projects[p.ProjectID]
			if project == "" {
				project = "-"
			}
			if p.PID == 0 {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\towner unknown: %s\n", p.PID, p.Name, project, addr, hint)
			} else {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", p.PID, p.Name, project, addr)
			}
		}
	}
	for _, c := range s.Containers {
		if held[c.ID] {
			continue
		}
		var addrs []netip.Addr
		for _, m := range c.Ports {
			ip := m.HostIP
			if !ip.IsValid() { // Podman's empty host IP: every interface
				ip = netip.IPv4Unspecified()
			}
			if m.HostPort == port && m.Proto == "tcp" && !slices.Contains(addrs, ip) {
				addrs = append(addrs, ip)
			}
		}
		for _, ip := range addrs {
			found = true
			name, project, desc := container(c.ID)
			fmt.Fprintf(tw, "-\t%s\t%s\t%s\t%s, no listening socket\n", name, project, netip.AddrPortFrom(ip, port), desc)
		}
	}
	if !found {
		_, err := fmt.Fprintln(w, "free")
		return false, err
	}
	return true, tw.Flush()
}
