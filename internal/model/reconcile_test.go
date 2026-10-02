package model

import (
	"math"
	"net/netip"
	"slices"
	"testing"
	"time"
)

var (
	any6 = netip.IPv6Unspecified()
	lo2  = netip.MustParseAddr("127.0.0.2")
)

// lst is a listener; rp is a process holding them, named name (pid 0 for the unknown owner).
func lst(proto string, addr netip.Addr, port uint16) Listener {
	return Listener{Proto: proto, Addr: addr, Port: port}
}

func rp(pid int, name string, kind Kind, ls ...Listener) Process {
	p := Process{PID: pid, PPID: 1, Name: name, Argv: []string{"/usr/bin/" + name}, Kind: kind, Listeners: ls, CPUPercent: math.NaN()}
	if pid == 0 {
		p.Argv, p.Unknown = nil, unknownOwner
	}
	return p
}

func pm(ip string, host, ctr uint16, proto string) PortMapping {
	m := PortMapping{HostPort: host, ContainerPort: ctr, Proto: proto}
	if ip != "" {
		m.HostIP = netip.MustParseAddr(ip)
	}
	return m
}

func TestReconcile(t *testing.T) {
	db := Container{ID: "db", Name: "shop-db-1", ComposeProject: "shop",
		Ports: []PortMapping{pm("0.0.0.0", 5432, 5432, "tcp"), pm("::", 5432, 5432, "tcp")}}
	cache := Container{ID: "cache", Name: "shop-cache-1", Ports: []PortMapping{pm("0.0.0.0", 6379, 6379, "tcp")}}
	local1 := Container{ID: "l1", Ports: []PortMapping{pm("127.0.0.1", 8080, 80, "tcp")}}
	local2 := Container{ID: "l2", Ports: []PortMapping{pm("127.0.0.2", 8080, 80, "tcp")}}
	open := Container{ID: "open", Ports: []PortMapping{pm("0.0.0.0", 8080, 80, "tcp")}}
	podman := Container{ID: "pod", Ports: []PortMapping{pm("", 8000, 80, "tcp")}}
	v4 := Container{ID: "v4", Ports: []PortMapping{pm("0.0.0.0", 8080, 80, "tcp")}}
	v6 := Container{ID: "v6", Ports: []PortMapping{pm("::", 8080, 80, "tcp")}}
	orbProbe := Container{ID: "probe", Name: "devdash-orb-probe", Image: "nginx:alpine",
		Ports: []PortMapping{pm("0.0.0.0", 18090, 80, "tcp"), pm("::", 18090, 80, "tcp")}}
	dns := Container{ID: "dns", Ports: []PortMapping{pm("0.0.0.0", 53, 53, "udp"), pm("", 0, 9000, "tcp")}}

	type want struct {
		ctr       string   // Process.ContainerID
		kind      Kind     // Process.Kind
		listeners []string // Listener.ContainerID, in order
	}
	tests := []struct {
		name       string
		procs      []Process
		containers []Container
		want       []want
	}{
		{"docker engine on linux, as a user: one unknown owner per socket",
			[]Process{rp(0, "unknown", KindOther, lst("tcp4", any4, 5432)), rp(0, "unknown", KindOther, lst("tcp6", any6, 5432))},
			[]Container{db},
			[]want{{"db", KindContainer, []string{"db"}}, {"db", KindContainer, []string{"db"}}}},
		{"docker engine as root: a docker-proxy per port and family",
			[]Process{rp(10, "docker-proxy", KindServer, lst("tcp4", any4, 5432)), rp(11, "docker-proxy", KindServer, lst("tcp6", any6, 5432))},
			[]Container{db},
			[]want{{"db", KindContainer, []string{"db"}}, {"db", KindContainer, []string{"db"}}}},
		{"docker engine 28+ with userland-proxy off, as root: dockerd holds the published port",
			[]Process{rp(12, "dockerd", KindServer, lst("tcp4", any4, 5432), lst("tcp6", any6, 5432))},
			[]Container{db},
			[]want{{"db", KindContainer, []string{"db", "db"}}}},
		{"dockerd holding several containers' ports keeps its own row",
			[]Process{rp(12, "dockerd", KindServer, lst("tcp4", any4, 5432), lst("tcp4", any4, 6379))},
			[]Container{db, cache},
			[]want{{"", KindContainer, []string{"db", "cache"}}}},
		{"docker desktop: one backend holds two containers' ports and its own",
			[]Process{rp(20, "com.docker.backend", KindServer, lst("tcp6", any6, 5432), lst("tcp6", any6, 6379), lst("tcp4", lo, 6443))},
			[]Container{db, cache},
			[]want{{"", KindContainer, []string{"db", "cache", ""}}}},
		{"docker desktop with one container",
			[]Process{rp(20, "com.docker.backend", KindServer, lst("tcp6", any6, 5432))},
			[]Container{db},
			[]want{{"db", KindContainer, []string{"db"}}}},
		// Recorded with OrbStack 2.2.3 (DEV-73): `docker run -p 18090:80` is held by OrbStack
		// Helper on *:18090 for each family, next to its own 32222 and 59838. The forwarded
		// listeners link to the container; the process does not, since its own ports are not
		// the container's (PR #52 review).
		{"orbstack: the helper holds a container's port and its own",
			[]Process{orbHelper(14887, lst("tcp4", any4, 18090), lst("tcp6", any6, 18090),
				lst("tcp4", lo, 32222), lst("tcp6", netip.IPv6Loopback(), 32222), lst("tcp4", lo, 59838))},
			[]Container{orbProbe},
			[]want{{"", KindContainer, []string{"probe", "probe", "", "", ""}}}},
		{"docker desktop: one container's port next to the backend's own",
			[]Process{rp(20, "com.docker.backend", KindServer, lst("tcp6", any6, 5432), lst("tcp4", lo, 6443))},
			[]Container{db},
			[]want{{"", KindContainer, []string{"db", ""}}}},
		{"unknown owner with a container's port and an unpublished one",
			[]Process{rp(0, "unknown", KindOther, lst("tcp4", any4, 5432), lst("tcp4", any4, 9999))},
			[]Container{db},
			[]want{{"", KindContainer, []string{"db", ""}}}},
		{"orbstack: the helper's own ports alone are not a container's",
			[]Process{orbHelper(14887, lst("tcp4", lo, 32222), lst("tcp4", lo, 59838))},
			[]Container{orbProbe},
			[]want{{"", KindServer, []string{"", ""}}}},
		{"orbstack's app is not a forwarder",
			[]Process{func() Process {
				p := rp(14800, "OrbStack", KindServer, lst("tcp4", any4, 18090))
				p.Argv = []string{"/Applications/OrbStack.app/Contents/MacOS/OrbStack"}
				return p
			}()},
			[]Container{orbProbe},
			[]want{{"", KindServer, []string{""}}}},
		{"rootless docker",
			[]Process{rp(30, "rootlesskit", KindServer, lst("tcp4", any4, 6379))},
			[]Container{cache},
			[]want{{"cache", KindContainer, []string{"cache"}}}},
		{"rootless podman 5, empty host IP is every interface",
			[]Process{rp(31, "pasta", KindServer, lst("tcp4", any4, 8000))},
			[]Container{podman},
			[]want{{"pod", KindContainer, []string{"pod"}}}},
		{"name from argv[0] when the kernel name is something else",
			[]Process{func() Process {
				p := rp(32, "gvproxy-wrapper", KindServer, lst("tcp4", any4, 6379))
				p.Argv = []string{"/opt/podman/bin/gvproxy", "-listen"}
				return p
			}()},
			[]Container{cache},
			[]want{{"cache", KindContainer, []string{"cache"}}}},
		{"not a proxy: a server on a published port number is left alone",
			[]Process{rp(40, "node", KindServer, lst("tcp4", any4, 5432))},
			[]Container{db},
			[]want{{"", KindServer, []string{""}}}},
		{"exact address wins over unspecified",
			[]Process{rp(50, "docker-proxy", KindServer, lst("tcp4", lo, 8080)), rp(51, "docker-proxy", KindServer, lst("tcp4", lo2, 8080))},
			[]Container{local1, local2},
			[]want{{"l1", KindContainer, []string{"l1"}}, {"l2", KindContainer, []string{"l2"}}}},
		{"unspecified listener prefers the unspecified mapping",
			[]Process{rp(52, "docker-proxy", KindServer, lst("tcp4", any4, 8080))},
			[]Container{local2, open},
			[]want{{"open", KindContainer, []string{"open"}}}},
		{"each family's every-interface mapping is a different container's",
			[]Process{rp(0, "unknown", KindOther, lst("tcp4", any4, 8080)), rp(0, "unknown", KindOther, lst("tcp6", any6, 8080))},
			[]Container{v4, v6},
			[]want{{"v4", KindContainer, []string{"v4"}}, {"v6", KindContainer, []string{"v6"}}}},
		{"same, in the other API order",
			[]Process{rp(0, "unknown", KindOther, lst("tcp6", any6, 8080)), rp(0, "unknown", KindOther, lst("tcp4", any4, 8080))},
			[]Container{v6, v4},
			[]want{{"v6", KindContainer, []string{"v6"}}, {"v4", KindContainer, []string{"v4"}}}},
		{"podman's empty host IP is either family",
			[]Process{rp(31, "pasta", KindServer, lst("tcp6", any6, 8000))},
			[]Container{podman},
			[]want{{"pod", KindContainer, []string{"pod"}}}},
		{"unspecified listener takes one container's specific mapping",
			[]Process{rp(54, "docker-proxy", KindServer, lst("tcp4", any4, 8080))},
			[]Container{local2},
			[]want{{"l2", KindContainer, []string{"l2"}}}},
		{"unspecified listener does not guess between containers' specific mappings",
			[]Process{rp(54, "docker-proxy", KindServer, lst("tcp4", any4, 8080))},
			[]Container{local1, local2},
			[]want{{"", KindServer, []string{""}}}},
		{"a specific listener never takes an every-interface mapping",
			[]Process{rp(53, "docker-proxy", KindServer, lst("tcp4", lo, 5432))},
			[]Container{db},
			[]want{{"", KindServer, []string{""}}}},
		{"udp and unpublished ports are never matched",
			[]Process{rp(0, "unknown", KindOther, lst("tcp4", any4, 53)), rp(0, "unknown", KindOther, lst("tcp4", any4, 9000))},
			[]Container{dns},
			[]want{{"", KindOther, []string{""}}, {"", KindOther, []string{""}}}},
		{"no containers",
			[]Process{rp(10, "docker-proxy", KindServer, lst("tcp4", any4, 5432))},
			nil,
			[]want{{"", KindServer, []string{""}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Reconcile(slices.Clone(tt.procs), tt.containers)
			if len(got) != len(tt.want) {
				t.Fatalf("%d processes, want %d", len(got), len(tt.want))
			}
			for i, w := range tt.want {
				p := got[i]
				var ls []string
				for _, l := range p.Listeners {
					ls = append(ls, l.ContainerID)
				}
				if p.ContainerID != w.ctr || p.Kind != w.kind || !slices.Equal(ls, w.listeners) {
					t.Errorf("process %d (%s): container %q, kind %v, listeners %q; want %q, %v, %q",
						i, p.Name, p.ContainerID, p.Kind, ls, w.ctr, w.kind, w.listeners)
				}
			}
		})
	}
}

// TestBuildReconciles: a compose service's published port, held by Docker Desktop's backend,
// becomes a container row under its compose project, and the backend itself stays in `other`
// with kind container once it holds two containers' ports.
func TestBuildReconciles(t *testing.T) {
	raw := Raw{TakenAt: start, Processes: []Process{
		{PID: 20, PPID: 1, StartTime: start, Name: "com.docker.backe", Argv: []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend"}},
	}, Listeners: []RawListener{{"tcp6", any6, 5432, 20}, {"tcp6", any6, 6379, 20}}}
	containers := []Container{
		{ID: "db", Name: "shop-db-1", ComposeProject: "shop", Ports: []PortMapping{pm("0.0.0.0", 5432, 5432, "tcp")}},
		{ID: "cache", Name: "shop-cache-1", ComposeProject: "shop", Ports: []PortMapping{pm("0.0.0.0", 6379, 6379, "tcp")}},
	}
	s := Build(raw, Snapshot{}, containers, NewResolver("", nil))
	p := s.Processes[0]
	if p.Kind != KindContainer || p.ContainerID != "" || p.Listeners[0].ContainerID != "db" || p.Listeners[1].ContainerID != "cache" {
		t.Fatalf("backend: kind %v, container %q, listeners %+v", p.Kind, p.ContainerID, p.Listeners)
	}
	got := render(Flatten(s, ViewOptions{}))
	want := []string{"[compose shop]", "  ctr:shop-cache-1", "  ctr:shop-db-1", "[other]", "  com.docker.backend"}
	if !slices.Equal(got, want) {
		t.Errorf("rows\n%q\nwant\n%q", got, want)
	}

	// One container: the backend row is that container's.
	raw.Listeners = raw.Listeners[:1]
	s = Build(raw, Snapshot{}, containers[:1], NewResolver("", nil))
	if got, want := render(Flatten(s, ViewOptions{})), []string{"[compose shop]", "  com.docker.backend@shop-db-1"}; !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
}

// TestReconcileKeepsRowKey: a PID 0 row is the same row whether or not Docker answered.
func TestReconcileKeepsRowKey(t *testing.T) {
	raw := Raw{TakenAt: start, Listeners: []RawListener{{"tcp4", any4, 5432, 0}}}
	db := Container{ID: "db", Ports: []PortMapping{pm("0.0.0.0", 5432, 5432, "tcp")}}
	without := Build(raw, Snapshot{}, nil, NewResolver("", nil)).Processes[0]
	with := Build(raw, Snapshot{}, []Container{db}, NewResolver("", nil)).Processes[0]
	if with.Listeners[0].ContainerID != "db" {
		t.Fatalf("not reconciled: %+v", with.Listeners)
	}
	if with.Key() != without.Key() {
		t.Errorf("key %+v with Docker, %+v without", with.Key(), without.Key())
	}
}

// TestOrbStackRows: OrbStack Helper forwarding one container's port next to its own ports
// keeps its own row, kind container, with no container name, and the container gets a row of
// its own; hiding containers (`d`) drops only the container row, so the Helper's own ports
// stay visible (PR #52 review, DEV-73).
func TestOrbStackRows(t *testing.T) {
	helper := orbHelper(14887)
	helper.Kind, helper.StartTime = KindOther, time.Unix(1, 0)
	raw := Raw{Processes: []Process{helper}, Listeners: []RawListener{
		{Proto: "tcp4", Addr: any4, Port: 18090, PID: 14887}, {Proto: "tcp6", Addr: any6, Port: 18090, PID: 14887},
		{Proto: "tcp4", Addr: lo, Port: 32222, PID: 14887}, {Proto: "tcp4", Addr: lo, Port: 59838, PID: 14887},
	}}
	probe := Container{ID: "probe", Name: "devdash-orb-probe", Image: "nginx:alpine",
		Ports: []PortMapping{pm("0.0.0.0", 18090, 80, "tcp"), pm("::", 18090, 80, "tcp")}}
	s := Build(raw, Snapshot{}, []Container{probe}, NewResolver("", nil))
	p := s.Processes[0]
	if p.ContainerID != "" || p.Kind != KindContainer {
		t.Errorf("helper: container %q, kind %v; want none, container", p.ContainerID, p.Kind)
	}
	if got := render(Flatten(s, ViewOptions{})); !slices.Equal(got, []string{"[containers]", "  ctr:devdash-orb-probe", "[other]", "  OrbStack Helper"}) {
		t.Errorf("rows %q", got)
	}
	if got := render(Flatten(s, ViewOptions{HideContainers: true})); !slices.Equal(got, []string{"[other]", "  OrbStack Helper"}) {
		t.Errorf("HideContainers: rows %q", got)
	}
}
