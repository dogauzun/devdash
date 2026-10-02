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
