package model

import (
	"net/netip"
	"slices"
	"testing"
	"time"
)

// TestHolders: the holders of a TCP port in snapshot order, a socket reconciled to a container
// standing for that container once, a published container port with no socket, and the PID 0
// unknown owner; UDP holds nothing.
func TestHolders(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	any4 := netip.IPv4Unspecified()
	vite := Process{PID: 101, StartTime: t0, Name: "node", Listeners: []Listener{{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 5173}}}
	backend := Process{PID: 30, StartTime: t0, Name: "com.docker.backend", Listeners: []Listener{
		{Proto: "tcp4", Addr: any4, Port: 5432, ContainerID: "db"},
		{Proto: "tcp4", Addr: any4, Port: 5432, ContainerID: "db"}, // the same container twice: one holder
		{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 5432},
		{Proto: "tcp6", Addr: netip.IPv6Loopback(), Port: 5432}, // a second own socket: still one holder
	}}
	unknown := Process{Name: "unknown", Listeners: []Listener{{Proto: "tcp4", Addr: any4, Port: 631}}}
	statsd := Process{PID: 40, StartTime: t0, Name: "statsd", Listeners: []Listener{{Proto: "udp4", Addr: any4, Port: 8125}}}
	s := Snapshot{
		Processes: []Process{vite, backend, unknown, statsd},
		Containers: []Container{
			{ID: "db", Ports: []PortMapping{{HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}},
			{ID: "web", Ports: []PortMapping{{HostPort: 5432, ContainerPort: 80, Proto: "tcp"}, {HostPort: 8000, ContainerPort: 80, Proto: "tcp"}}},
			{ID: "dns", Ports: []PortMapping{{HostPort: 8125, ContainerPort: 53, Proto: "udp"}}},
		},
	}
	keys := func(hs []Holder) []RowKey {
		var ks []RowKey
		for _, h := range hs {
			if (h.Process == nil) != (h.Key.ContainerID != "") || h.Process != nil && h.Process.Key() != h.Key {
				t.Errorf("holder %+v: Process does not match Key", h)
			}
			ks = append(ks, h.Key)
		}
		return ks
	}
	for _, tt := range []struct {
		port uint16
		want []RowKey
	}{
		{5173, []RowKey{vite.Key()}},
		{5432, []RowKey{{ContainerID: "db"}, backend.Key(), {ContainerID: "web"}}},
		{8000, []RowKey{{ContainerID: "web"}}},
		{631, []RowKey{unknown.Key()}},
		{8125, nil}, // UDP, from a process and a container: not a TCP port's holder
		{22, nil},
	} {
		if got := keys(Holders(s, tt.port)); !slices.Equal(got, tt.want) {
			t.Errorf("Holders(%d) = %+v, want %+v", tt.port, got, tt.want)
		}
	}
}

// TestListenerTCP: a listener holds a TCP port unless its proto is UDP, an unknown one included.
func TestListenerTCP(t *testing.T) {
	for proto, want := range map[string]bool{"tcp4": true, "tcp6": true, "": true, "udp4": false, "udp6": false} {
		if got := (Listener{Proto: proto}).TCP(); got != want {
			t.Errorf("Listener{Proto: %q}.TCP() = %v, want %v", proto, got, want)
		}
	}
}

// TestPortMappingTCP: a published port is TCP only when Docker says exactly tcp.
func TestPortMappingTCP(t *testing.T) {
	for proto, want := range map[string]bool{"tcp": true, "udp": false, "sctp": false, "": false} {
		if got := (PortMapping{Proto: proto}).TCP(); got != want {
			t.Errorf("PortMapping{Proto: %q}.TCP() = %v, want %v", proto, got, want)
		}
	}
}
