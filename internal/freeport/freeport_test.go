package freeport

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/model"
)

func TestLast(t *testing.T) {
	for from, want := range map[uint16]uint16{1: 100, 3000: 3099, 65436: 65535, 65437: 65535, 65535: 65535} {
		if got := Last(from); got != want {
			t.Errorf("Last(%d) = %d, want %d", from, got, want)
		}
	}
}

// probeSet is a fake Prober that reports every port bindable except those in taken, and records
// the ports it was asked about.
type probeSet struct {
	taken []uint16
	fail  map[uint16]error
	asked []uint16
}

func (p *probeSet) probe(port uint16) (bool, error) {
	p.asked = append(p.asked, port)
	if err := p.fail[port]; err != nil {
		return false, err
	}
	return !slices.Contains(p.taken, port), nil
}

func TestFind(t *testing.T) {
	any4, any6, lo4 := netip.IPv4Unspecified(), netip.IPv6Unspecified(), netip.MustParseAddr("127.0.0.1")
	hostIP := netip.MustParseAddr("192.168.1.20")
	listen := func(proto string, a netip.Addr, ports ...uint16) model.Process {
		p := model.Process{PID: 10, Name: "node"}
		for _, port := range ports {
			p.Listeners = append(p.Listeners, model.Listener{Proto: proto, Addr: a, Port: port})
		}
		return p
	}
	publish := func(proto string, ip netip.Addr, ports ...uint16) model.Container {
		c := model.Container{ID: "c0ffee", Name: "web", State: "running"}
		for _, port := range ports {
			c.Ports = append(c.Ports, model.PortMapping{HostIP: ip, HostPort: port, ContainerPort: 80, Proto: proto})
		}
		return c
	}
	tests := []struct {
		name       string
		procs      []model.Process
		containers []model.Container
		taken      []uint16 // ports the fake probe cannot bind
		from       uint16
		want       uint16
		ok         bool
		asked      []uint16 // ports the probe is asked about
	}{
		{"from is free", nil, nil, nil, 3000, 3000, true, []uint16{3000}},
		{"listener on 0.0.0.0", []model.Process{listen("tcp4", any4, 3000)}, nil, nil, 3000, 3001, true, []uint16{3001}},
		{"listener on 127.0.0.1", []model.Process{listen("tcp4", lo4, 3000, 3001)}, nil, nil, 3000, 3002, true, []uint16{3002}},
		{"listener on ::", []model.Process{listen("tcp6", any6, 5173)}, nil, nil, 5173, 5174, true, []uint16{5174}},
		{"unknown owner (pid 0) still holds the port",
			[]model.Process{{PID: 0, Name: "unknown", Listeners: []model.Listener{{Proto: "tcp4", Addr: any4, Port: 8080}}}}, nil, nil, 8080, 8081, true, []uint16{8081}},
		{"container publishing on a host IP", nil, []model.Container{publish("tcp", hostIP, 8000)}, nil, 8000, 8001, true, []uint16{8001}},
		{"container publishing with no host IP (Podman)", nil, []model.Container{publish("tcp", netip.Addr{}, 8000, 8001)}, nil, 8000, 8002, true, []uint16{8002}},
		{"udp mapping does not count", nil, []model.Container{publish("udp", any4, 5353)}, nil, 5353, 5353, true, []uint16{5353}},
		{"udp listener does not count", []model.Process{listen("udp4", any4, 5353)}, nil, nil, 5353, 5353, true, []uint16{5353}},
		{"probe says taken (another user's listener)", nil, nil, []uint16{4000, 4001}, 4000, 4002, true, []uint16{4000, 4001, 4002}},
		{"snapshot and probe together", []model.Process{listen("tcp4", lo4, 4000)}, []model.Container{publish("tcp", any4, 4002)}, []uint16{4001}, 4000, 4003, true,
			[]uint16{4001, 4003}},
		{"last port of the range", []model.Process{listen("tcp4", any4, rangeOf(3000, 3098)...)}, nil, nil, 3000, 3099, true, []uint16{3099}},
		{"nothing free in range", []model.Process{listen("tcp4", any4, rangeOf(3000, 3099)...)}, nil, []uint16{3100}, 3000, 0, false, nil},
		{"range ends at 65535", nil, nil, rangeOf(65500, 65535), 65500, 0, false, rangeOf(65500, 65535)},
		{"from 65535", nil, nil, nil, 65535, 65535, true, []uint16{65535}},
		{"from 65535, taken", []model.Process{listen("tcp6", any6, 65535)}, nil, nil, 65535, 0, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := model.Snapshot{Processes: tt.procs, Containers: tt.containers}
			p := &probeSet{taken: tt.taken}
			got, ok, err := Find(s, tt.from, p.probe)
			if err != nil || got != tt.want || ok != tt.ok {
				t.Errorf("Find(%d) = %d, %v, %v; want %d, %v, nil", tt.from, got, ok, err, tt.want, tt.ok)
			}
			if !slices.Equal(p.asked, tt.asked) {
				t.Errorf("probed %v, want %v", p.asked, tt.asked)
			}
		})
	}
}

// TestNext: the search `free n+1` makes, with no search at all for n = 65535.
func TestNext(t *testing.T) {
	held := model.Snapshot{Processes: []model.Process{{PID: 10, Name: "node", Listeners: []model.Listener{
		{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 3001},
	}}}}
	tests := []struct {
		n      uint16
		taken  []uint16
		want   uint16
		wantOK bool
		asked  []uint16
	}{
		{n: 3000, taken: []uint16{3002}, want: 3003, wantOK: true, asked: []uint16{3002, 3003}}, // from n+1, as `free n+1`
		{n: 65534, taken: []uint16{65535}, asked: []uint16{65535}},                              // none in range
		{n: 65535}, // no search at all
	}
	for _, tt := range tests {
		p := &probeSet{taken: tt.taken}
		got, ok, err := Next(held, tt.n, p.probe)
		if err != nil || got != tt.want || ok != tt.wantOK || !slices.Equal(p.asked, tt.asked) {
			t.Errorf("Next(%d) = %d, %v, %v asking %v; want %d, %v, nil asking %v", tt.n, got, ok, err, p.asked, tt.want, tt.wantOK, tt.asked)
		}
	}
}

// TestFindProbeError: the search stops at the first probe error, so a failed probe is never
// mistaken for "nothing free".
func TestFindProbeError(t *testing.T) {
	boom := errors.New("bind: operation not permitted")
	p := &probeSet{taken: []uint16{3000}, fail: map[uint16]error{3001: boom}}
	got, ok, err := Find(model.Snapshot{}, 3000, p.probe)
	if !errors.Is(err, boom) || ok || got != 0 {
		t.Errorf("Find = %d, %v, %v; want 0, false, %v", got, ok, err, boom)
	}
	if !slices.Equal(p.asked, []uint16{3000, 3001}) {
		t.Errorf("probed %v, want [3000 3001]", p.asked)
	}
}

func rangeOf(from, to uint16) []uint16 {
	var ps []uint16
	for p := int(from); p <= int(to); p++ {
		ps = append(ps, uint16(p))
	}
	return ps
}

// TestProbeLive binds real sockets: a port this test listens on, on 127.0.0.1, on 0.0.0.0 and
// on ::1, is taken; the same port once closed is free.
func TestProbeLive(t *testing.T) {
	for _, tt := range []struct{ network, addr string }{
		{"tcp4", "127.0.0.1:0"},
		{"tcp4", "0.0.0.0:0"},
		{"tcp6", "[::1]:0"},
	} {
		t.Run(tt.addr, func(t *testing.T) {
			ln, err := net.Listen(tt.network, tt.addr)
			if err != nil {
				if tt.network == "tcp6" {
					t.Skipf("no IPv6 loopback here: %v", err)
				}
				t.Fatal(err)
			}
			port := uint16(ln.Addr().(*net.TCPAddr).Port)
			if free, err := Probe(port); err != nil || free {
				t.Errorf("held port %d: Probe = %v, %v; want false, nil", port, free, err)
			}
			_ = ln.Close()
			if free, err := Probe(port); err != nil || !free {
				t.Errorf("closed port %d: Probe = %v, %v; want true, nil", port, free, err)
			}
		})
	}
}

// TestProbeLiveFind: Find with the real Probe skips a port this test holds even when the
// snapshot does not show it, as for another user's listener.
func TestProbeLiveFind(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	got, ok, err := Find(model.Snapshot{}, port, Probe)
	if err != nil || !ok || got == port || got < port || got > Last(port) {
		t.Errorf("Find(%d) = %d, %v, %v; want a port in %d-%d other than %d", port, got, ok, err, port+1, Last(port), port)
	}
}

// TestErrorClasses: only a failed bind is "taken", only a failed socket or bind for want of
// IPv6 skips the IPv6 check, and everything else (a setsockopt, EMFILE, EPERM) is an error.
func TestErrorClasses(t *testing.T) {
	sys := func(call string, errno unix.Errno) error { return os.NewSyscallError(call, errno) }
	tests := []struct {
		err           error
		taken, noIPv6 bool
	}{
		{nil, false, false},
		{sys("bind", unix.EADDRINUSE), true, false},
		{sys("bind", unix.EACCES), true, false},
		{sys("bind", unix.EPERM), false, false},
		{sys("bind", unix.EADDRNOTAVAIL), false, true},
		{sys("socket", unix.EAFNOSUPPORT), false, true},
		{sys("socket", unix.EMFILE), false, false},
		{sys("socket", unix.EACCES), false, false},
		{sys("setsockopt IPV6_V6ONLY", unix.EAFNOSUPPORT), false, false},
		{unix.EADDRINUSE, false, false},
	}
	for _, tt := range tests {
		if taken(tt.err) != tt.taken || noIPv6(tt.err) != tt.noIPv6 {
			t.Errorf("%v: taken %v, noIPv6 %v; want %v, %v", tt.err, taken(tt.err), noIPv6(tt.err), tt.taken, tt.noIPv6)
		}
	}
}
