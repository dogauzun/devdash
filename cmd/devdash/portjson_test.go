package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/freeport"
	"github.com/dogauzun/devdash/internal/model"
)

// portJSON is the port answer as a script reads it.
type portJSON struct {
	SchemaVersion int    `json:"schema_version"`
	TakenAt       string `json:"taken_at"`
	Port          int
	Free          bool
	Holders       []map[string]any
	Containers    []map[string]any
	NextFree      *int `json:"next_free"`
}

func decodePortJSON(t *testing.T, out []byte) portJSON {
	t.Helper()
	var a portJSON
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return a
}

// TestPortAnswer: the holders are the processes with a listener on N, the unknown owner
// included, each as the full process object of --json; the containers are those publishing N
// over tcp, an exposed-only port not counted; free only when there is neither. goldenSnapshot
// listens on 5173 (node), 631 (unknown owner), 8081 (go) and 5432 (root's docker-proxy, as the
// unknown owner, reconciled to shop-db-1), and the paused scratch container exposes 80.
func TestPortAnswer(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := goldenSnapshot(t, dir, nil)
	var full bytes.Buffer
	if err := writeJSON(&full, s, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var snap struct {
		TakenAt    string `json:"taken_at"`
		Processes  []map[string]any
		Containers []map[string]any
	}
	if err := json.Unmarshal(full.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	pid := func(m map[string]any) int { return int(m["pid"].(float64)) }
	tests := []struct {
		port       uint16
		holders    []int    // pids, in snapshot order
		containers []string // ids
	}{
		{5173, []int{100}, nil},
		{631, []int{0}, nil},
		{8081, []int{101}, nil},
		{5432, []int{0}, []string{"9f1c2a7b0d3e"}},
		{80, nil, nil},
		{3000, nil, nil},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.port), func(t *testing.T) {
			var b bytes.Buffer
			if err := writePortJSON(&b, portAnswer(s, tt.port)); err != nil {
				t.Fatal(err)
			}
			validateAs(t, b.Bytes(), "port_answer")
			a := decodePortJSON(t, b.Bytes())
			if a.SchemaVersion != 1 || a.TakenAt != snap.TakenAt || a.Port != int(tt.port) || a.NextFree != nil {
				t.Errorf("schema_version %d, taken_at %q, port %d, next_free %v; want 1, %q, %d, null",
					a.SchemaVersion, a.TakenAt, a.Port, a.NextFree, snap.TakenAt, tt.port)
			}
			if want := len(tt.holders) == 0 && len(tt.containers) == 0; a.Free != want {
				t.Errorf("free %v, want %v", a.Free, want)
			}
			var got []int
			for _, h := range a.Holders {
				got = append(got, pid(h))
				// Exactly the --json process object: the same entry of processes (for pid 0, the
				// one whose listener is on this port).
				i := slices.IndexFunc(snap.Processes, func(p map[string]any) bool {
					ls := p["listeners"].([]any)
					return pid(p) == pid(h) && slices.ContainsFunc(ls, func(l any) bool { return l.(map[string]any)["port"] == float64(tt.port) })
				})
				if i < 0 || !reflect.DeepEqual(h, snap.Processes[i]) {
					t.Errorf("holder %v is not a process object of --json", h)
				}
			}
			if !slices.Equal(got, tt.holders) {
				t.Errorf("holders %v, want %v", got, tt.holders)
			}
			var ids []string
			for _, c := range a.Containers {
				ids = append(ids, c["id"].(string))
				if i := slices.IndexFunc(snap.Containers, func(sc map[string]any) bool { return sc["id"] == c["id"] }); i < 0 || !reflect.DeepEqual(c, snap.Containers[i]) {
					t.Errorf("container %v is not a container object of --json", c)
				}
			}
			if !slices.Equal(ids, tt.containers) {
				t.Errorf("containers %v, want %v", ids, tt.containers)
			}
		})
	}
}

// TestPortAnswerContainers: a container publishing N with no socket on it (iptables only)
// makes N taken, on any host address or none; one publishing N over udp, or another port, does
// not.
func TestPortAnswerContainers(t *testing.T) {
	pm := func(ip netip.Addr, port uint16, proto string) model.PortMapping {
		return model.PortMapping{HostIP: ip, HostPort: port, ContainerPort: port, Proto: proto}
	}
	db := model.Container{ID: "db", Name: "shop-db-1", State: "running", Ports: []model.PortMapping{pm(netip.IPv4Unspecified(), 5432, "tcp"), pm(netip.IPv6Unspecified(), 5432, "tcp")}}
	podman := model.Container{ID: "pod", Name: "web", State: "running", Ports: []model.PortMapping{pm(netip.Addr{}, 8000, "tcp")}}
	dns := model.Container{ID: "dns", Name: "dns", State: "running", Ports: []model.PortMapping{pm(netip.IPv4Unspecified(), 5353, "udp")}}
	s := model.Build(model.Raw{}, model.Snapshot{}, []model.Container{db, podman, dns}, model.NewResolver("", nil))
	for port, want := range map[uint16][]string{5432: {"db"}, 8000: {"pod"}, 5353: nil, 5433: nil} {
		a := portAnswer(s, port)
		var ids []string
		for _, c := range a.Containers {
			ids = append(ids, c.ID)
		}
		if !slices.Equal(ids, want) || len(a.Holders) != 0 || a.Free != (want == nil) {
			t.Errorf("port %d: containers %v, holders %d, free %v; want %v, none, %v", port, ids, len(a.Holders), a.Free, want, want == nil)
		}
	}
}

// TestRunPortJSON: `port N --json` prints the answer in both outcomes, with the exit code of
// `port N`; next_free is the search of `free N+1` when N is taken, and null when N is free, is
// 65535, nothing in range is free, or the probe fails, which also prints the error on stderr
// and keeps the exit code. fake() listens on 127.0.0.1:3000.
func TestRunPortJSON(t *testing.T) {
	boom := errors.New("probing port 3002: bind: operation not permitted")
	at65535 := func() *collector.Fake {
		f := fake()
		f.Steps[0].Result.Listeners[0].Port = 65535
		return f
	}
	tests := []struct {
		name   string
		args   []string
		c      *collector.Fake
		taken  []uint16 // ports the probe cannot bind
		fail   uint16   // a port whose probe fails with boom
		want   int
		free   bool
		next   *int
		stderr string
		asked  []uint16
	}{
		{"held", []string{"port", "3000", "--json"}, fake(), nil, 0, 0, false, ptr(3001), "", []uint16{3001}},
		{"--json first", []string{"--json", "--no-docker", "port", "3000"}, fake(), nil, 0, 0, false, ptr(3001), "", []uint16{3001}},
		{"probe says taken", []string{"port", "3000", "--json"}, fake(), []uint16{3001, 3002}, 0, 0, false, ptr(3003), "", []uint16{3001, 3002, 3003}},
		{"free", []string{"port", "3001", "--json"}, fake(), nil, 0, 1, true, nil, "", nil},
		{"65535 held", []string{"port", "65535", "--json"}, at65535(), nil, 0, 0, false, nil, "", nil},
		{"65535 free", []string{"port", "65535", "--json"}, fake(), nil, 0, 1, true, nil, "", nil},
		{"nothing free in range", []string{"port", "3000", "--json"}, fake(), rangeOf(3001, 3100), 0, 0, false, nil, "", rangeOf(3001, 3100)},
		{"probe fails", []string{"port", "3000", "--json"}, fake(), []uint16{3001}, 3002, 0, false, nil, "devdash: next free: " + boom.Error() + "\n", []uint16{3001, 3002}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := stubProbe(t, func(p uint16) (bool, error) {
				if p == tt.fail {
					return false, boom
				}
				return !slices.Contains(tt.taken, p), nil
			})
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr, tt.c); code != tt.want || stderr.String() != tt.stderr {
				t.Errorf("exit %d, stderr %q; want %d, %q", code, stderr.String(), tt.want, tt.stderr)
			}
			validateAs(t, stdout.Bytes(), "port_answer")
			a := decodePortJSON(t, stdout.Bytes())
			if a.Free != tt.free || !reflect.DeepEqual(a.NextFree, tt.next) {
				t.Errorf("free %v, next_free %v; want %v, %v", a.Free, deref(a.NextFree), tt.free, deref(tt.next))
			}
			if !slices.Equal(*asked, tt.asked) {
				t.Errorf("probed %v, want %v", *asked, tt.asked)
			}
			if n := len(tt.c.Calls()); n != 2 {
				t.Errorf("%d samples, want 2", n)
			}
		})
	}
}

// TestRunPortJSONSamplesTwice: like --json, the holder's cpu_percent is the rate between two
// samples 200 ms apart, and taken_at is the second sample's.
func TestRunPortJSONSamplesTwice(t *testing.T) {
	f := fake()
	second := f.Steps[0]
	second.Result.TakenAt = second.Result.TakenAt.Add(cpuGap)
	second.Result.Processes = slices.Clone(second.Result.Processes)
	second.Result.Processes[0].CPUTime = cpuGap / 2
	f.Steps = append(f.Steps, second)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"port", "3000", "--json"}, &stdout, &stderr, f); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	a := decodePortJSON(t, stdout.Bytes())
	if len(a.Holders) != 1 || a.Holders[0]["cpu_percent"] != 50.0 || a.TakenAt != "2026-10-02T12:00:00.2Z" {
		t.Errorf("holders %v, taken_at %q; want one at 50%% CPU, the second sample's time", a.Holders, a.TakenAt)
	}
}

// TestRunPortJSONFailed: no snapshot is exit 5 with nothing on stdout and nothing probed,
// never "free" (1); an unwritable stdout is exit 5.
func TestRunPortJSONFailed(t *testing.T) {
	asked := stubProbe(t, func(uint16) (bool, error) { return true, nil })
	for _, steps := range [][]collector.Step{{{Err: errors.New("collector broke")}}, {fake().Steps[0], {Err: errors.New("collector broke")}}} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"port", "3001", "--json"}, &stdout, &stderr, &collector.Fake{Steps: steps}); code != 5 || stdout.Len() != 0 || stderr.String() != "devdash: collector broke\n" {
			t.Errorf("snapshot fails: exit %d, stdout %q, stderr %q; want 5, nothing, the error", code, stdout.String(), stderr.String())
		}
	}
	if len(*asked) != 0 {
		t.Errorf("probed %v without a snapshot", *asked)
	}
	for _, port := range []string{"3000", "3001"} {
		var stderr bytes.Buffer
		if code := run([]string{"port", port, "--json"}, failWriter{}, &stderr, fake()); code != 5 || !strings.Contains(stderr.String(), "devdash: disk full") {
			t.Errorf("port %s, stdout fails: exit %d, stderr %q; want 5 and the error", port, code, stderr.String())
		}
	}
}

// TestPortJSONLive runs `devdash port N --json` with the real collector for a port this test
// holds and checks it against docs/json-schema.md.
func TestPortJSONLive(t *testing.T) {
	stubProbe(t, freeport.Probe)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	var stdout, stderr bytes.Buffer
	if code := run([]string{"port", fmt.Sprint(port), "--json"}, &stdout, &stderr, collector.New()); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	validateAs(t, stdout.Bytes(), "port_answer")
	a := decodePortJSON(t, stdout.Bytes())
	if len(a.Holders) != 1 || int(a.Holders[0]["pid"].(float64)) != os.Getpid() || a.Holders[0]["cpu_percent"] == nil {
		t.Errorf("holders %v, want this test's process with a cpu_percent", a.Holders)
	}
	if a.Free || a.NextFree == nil || *a.NextFree <= port || *a.NextFree > int(freeport.Last(uint16(port)+1)) {
		t.Errorf("free %v, next_free %v; want false and a port of %d-%d", a.Free, deref(a.NextFree), port+1, freeport.Last(uint16(port)+1))
	}
}

func ptr(n int) *int { return &n }

// deref prints a *int as its value or null.
func deref(p *int) any {
	if p == nil {
		return "null"
	}
	return *p
}
