package main

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

func TestWritePort(t *testing.T) {
	lo4, lo6 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")
	any4, any6 := netip.IPv4Unspecified(), netip.IPv6Unspecified()
	l := func(proto string, a netip.Addr, port uint16, pid int) model.RawListener {
		return model.RawListener{Proto: proto, Addr: a, Port: port, PID: pid}
	}
	proc := func(pid int, name string) model.Process {
		return model.Process{PID: pid, PPID: 1, Name: name, Argv: []string{name}, StartTime: time.Unix(1, 0)}
	}
	tests := []struct {
		name      string
		listeners []model.RawListener
		port      uint16
		want      string
		found     bool
	}{
		{"found", []model.RawListener{l("tcp4", lo4, 3000, 10)}, 3000, "10  node  -  127.0.0.1:3000\n", true},
		{"free", []model.RawListener{l("tcp4", lo4, 3000, 10)}, 3001, "free\n", false},
		{"nothing listens at all", nil, 80, "free\n", false},
		{"unknown owner", []model.RawListener{l("tcp4", any4, 631, 0)}, 631,
			"0  unknown  -  0.0.0.0:631  owner unknown: run with sudo to see owners\n", true},
		{"dual-stack socket is one line", []model.RawListener{l("tcp6", any6, 8080, 10)}, 8080, "10  node  -  [::]:8080\n", true},
		{"IPv4 and IPv6 sockets are two lines", []model.RawListener{l("tcp4", lo4, 5173, 10), l("tcp6", lo6, 5173, 10)}, 5173,
			"10  node  -  127.0.0.1:5173\n10  node  -  [::1]:5173\n", true},
		{"scoped IPv6 address keeps its zone", []model.RawListener{l("tcp6", netip.MustParseAddr("fe80::1%lo0"), 8081, 10)}, 8081,
			"10  node  -  [fe80::1%lo0]:8081\n", true},
		{"two owners, aligned", []model.RawListener{l("tcp4", lo4, 9000, 11), l("tcp6", any6, 9000, 0), l("tcp4", lo4, 9001, 10)}, 9000,
			"11  postgres  -  127.0.0.1:9000\n" +
				"0   unknown   -  [::]:9000  owner unknown: run with sudo to see owners\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := model.Raw{Processes: []model.Process{proc(10, "node"), proc(11, "postgres")}, Listeners: tt.listeners}
			s := model.Build(raw, model.Snapshot{}, nil, model.NewResolver("", nil))
			var b bytes.Buffer
			if found, err := writePort(&b, s, tt.port); err != nil || found != tt.found || b.String() != tt.want {
				t.Errorf("found %v, output\n%s\nwant %v\n%s", found, b.String(), tt.found, tt.want)
			}
		})
	}
}

func TestWritePortProject(t *testing.T) {
	s := model.Snapshot{
		Projects:  []model.Project{{ID: "/code/shop", Root: "/code/shop", Name: "shop"}},
		Processes: []model.Process{{PID: 7, Name: "vite", ProjectID: "/code/shop", Listeners: []model.Listener{{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: 5173}}}},
	}
	var b bytes.Buffer
	if _, _ = writePort(&b, s, 5173); b.String() != "7  vite  shop  [::]:5173\n" {
		t.Errorf("got %q", b.String())
	}
}

// TestPortLive runs `devdash port` against the real collector for a port this test holds and
// for one it just released.
func TestPortLive(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	var stdout, stderr bytes.Buffer
	if code := run([]string{"port", fmt.Sprint(port)}, &stdout, &stderr, collector.New()); code != 0 {
		t.Fatalf("held port %d: exit %d, stdout %q, stderr %q", port, code, stdout.String(), stderr.String())
	}
	if want := fmt.Sprintf("%d ", os.Getpid()); !strings.HasPrefix(stdout.String(), want) || !strings.Contains(stdout.String(), fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Errorf("held port %d: %q, want our pid and address", port, stdout.String())
	}

	_ = ln.Close()
	stdout.Reset()
	if code := run([]string{"port", fmt.Sprint(port)}, &stdout, &stderr, collector.New()); code != 1 || stdout.String() != "free\n" {
		t.Errorf("released port %d: exit %d, stdout %q, stderr %q", port, code, stdout.String(), stderr.String())
	}
}

// TestWritePortContainers: a listener reconciled to a container names the container, its
// image and compose project instead of the proxy or the unknown owner; a published port with
// no listener behind it (iptables only) is reported too (DEV-52).
func TestWritePortContainers(t *testing.T) {
	any4, any6, lo4 := netip.IPv4Unspecified(), netip.IPv6Unspecified(), netip.MustParseAddr("127.0.0.1")
	pm := func(ip netip.Addr, port uint16) model.PortMapping {
		return model.PortMapping{HostIP: ip, HostPort: port, ContainerPort: port, Proto: "tcp"}
	}
	db := model.Container{ID: "db0123456789", Name: "shop-db-1", Image: "postgres:16", State: "running", ComposeProject: "shop", ComposeService: "db",
		Ports: []model.PortMapping{pm(any4, 5432), pm(any6, 5432)}}
	cache := model.Container{ID: "cache0123456", Name: "cache", Image: "redis:7", State: "running", Ports: []model.PortMapping{pm(any4, 6379)}}
	unnamed := model.Container{ID: "77aa", State: "running", Ports: []model.PortMapping{{HostPort: 8000, ContainerPort: 80, Proto: "tcp"}}}
	proc := func(pid int, name string) model.Process {
		return model.Process{PID: pid, PPID: 1, Name: name, Argv: []string{"/usr/bin/" + name}, StartTime: time.Unix(1, 0)}
	}
	l := func(proto string, a netip.Addr, port uint16, pid int) model.RawListener {
		return model.RawListener{Proto: proto, Addr: a, Port: port, PID: pid}
	}
	tests := []struct {
		name       string
		listeners  []model.RawListener
		containers []model.Container
		port       uint16
		want       string
	}{
		{"docker-proxy", []model.RawListener{l("tcp4", any4, 5432, 20), l("tcp6", any6, 5432, 21)}, []model.Container{db}, 5432,
			"20  shop-db-1  shop  0.0.0.0:5432  container (postgres:16) via docker-proxy\n" +
				"21  shop-db-1  shop  [::]:5432     container (postgres:16) via docker-proxy\n"},
		{"unknown owner (root's docker-proxy, seen as a user)", []model.RawListener{l("tcp4", any4, 5432, 0)}, []model.Container{db}, 5432,
			"-  shop-db-1  shop  0.0.0.0:5432  container (postgres:16)\n"},
		{"shared com.docker.backend", []model.RawListener{l("tcp6", any6, 5432, 30), l("tcp6", any6, 6379, 30)}, []model.Container{db, cache}, 6379,
			"30  cache  -  [::]:6379  container (redis:7) via com.docker.backend\n"},
		{"iptables only", nil, []model.Container{db}, 5432,
			"-  shop-db-1  shop  0.0.0.0:5432  container (postgres:16), no listening socket\n" +
				"-  shop-db-1  shop  [::]:5432     container (postgres:16), no listening socket\n"},
		{"iptables only, no host IP and no name", nil, []model.Container{unnamed}, 8000,
			"-  77aa  -  0.0.0.0:8000  container, no listening socket\n"},
		{"a process of the user next to an iptables-only container", []model.RawListener{l("tcp4", lo4, 5432, 10)}, []model.Container{db}, 5432,
			"10  node       -     127.0.0.1:5432\n" +
				"-   shop-db-1  shop  0.0.0.0:5432  container (postgres:16), no listening socket\n" +
				"-   shop-db-1  shop  [::]:5432     container (postgres:16), no listening socket\n"},
		{"another port of the container is not reported", nil, []model.Container{db}, 5433, "free\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := model.Raw{Processes: []model.Process{proc(10, "node"), proc(20, "docker-proxy"), proc(21, "docker-proxy"), proc(30, "com.docker.backend")},
				Listeners: tt.listeners}
			s := model.Build(raw, model.Snapshot{}, tt.containers, model.NewResolver("", nil))
			var b bytes.Buffer
			found, err := writePort(&b, s, tt.port)
			if err != nil || found != (tt.want != "free\n") || b.String() != tt.want {
				t.Errorf("found %v, err %v, output\n%s\nwant\n%s", found, err, b.String(), tt.want)
			}
		})
	}
}

// TestWritePortDockerHint: with no container list (a socket the user may not open, an engine
// that does not answer, an endpoint devdash cannot use), a port held by an unknown owner or by
// a runtime process is most likely a container's, so its line carries the Docker warning's
// hint after the sudo one; a line of the user's own process does not (DEV-76).
func TestWritePortDockerHint(t *testing.T) {
	any4, lo4 := netip.IPv4Unspecified(), netip.MustParseAddr("127.0.0.1")
	denied := model.Warning{Code: "docker_unreachable", Count: 1, Hint: "docker: permission denied on /var/run/docker.sock (add yourself to the docker group)"}
	invalid := model.Warning{Code: "docker_endpoint_invalid", Count: 1, Hint: `docker: DOCKER_HOST "ssh://box": scheme ssh is not supported`}
	proc := func(pid int, name string) model.Process {
		return model.Process{PID: pid, PPID: 1, Name: name, Argv: []string{"/usr/bin/" + name}, StartTime: time.Unix(1, 0)}
	}
	l := func(port uint16, pid int) model.RawListener {
		return model.RawListener{Proto: "tcp4", Addr: any4, Port: port, PID: pid}
	}
	tests := []struct {
		name     string
		listener model.RawListener
		warnings []model.Warning
		want     string
	}{
		{"unknown owner, permission denied", l(18081, 0), []model.Warning{denied},
			"0  unknown  -  0.0.0.0:18081  owner unknown: run with sudo to see owners; " + denied.Hint + "\n"},
		{"unknown owner, endpoint invalid", l(18081, 0), []model.Warning{invalid},
			"0  unknown  -  0.0.0.0:18081  owner unknown: run with sudo to see owners; " + invalid.Hint + "\n"},
		{"docker-proxy as root, engine not answering", l(18081, 20), []model.Warning{{Code: "docker_unreachable", Count: 1, Hint: "docker: not reachable at unix:///var/run/docker.sock"}},
			"20  docker-proxy  -  0.0.0.0:18081  docker: not reachable at unix:///var/run/docker.sock\n"},
		{"docker-proxy without a Docker warning", l(18081, 20), nil, "20  docker-proxy  -  0.0.0.0:18081\n"},
		{"the user's own process", model.RawListener{Proto: "tcp4", Addr: lo4, Port: 3000, PID: 10}, []model.Warning{denied},
			"10  node  -  127.0.0.1:3000\n"},
		{"another warning is not Docker's", l(18081, 0), []model.Warning{{Code: "proc_hidepid", Count: 1, Hint: "/proc is mounted with hidepid=2"}},
			"0  unknown  -  0.0.0.0:18081  owner unknown: run with sudo to see owners\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := model.Raw{Processes: []model.Process{proc(10, "node"), proc(20, "docker-proxy")}, Listeners: []model.RawListener{tt.listener}}
			s := model.Build(raw, model.Snapshot{}, nil, model.NewResolver("", nil))
			s.Warnings = append(s.Warnings, tt.warnings...)
			var b bytes.Buffer
			if _, err := writePort(&b, s, tt.listener.Port); err != nil || b.String() != tt.want {
				t.Errorf("err %v, output\n%q\nwant\n%q", err, b.String(), tt.want)
			}
		})
	}
}
