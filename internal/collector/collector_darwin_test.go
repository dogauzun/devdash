//go:build darwin

package collector

import (
	"context"
	"net"
	"net/netip"
	"os"
	"slices"
	"sort"
	"testing"
	"time"
)

func TestCollectFindsSelf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == os.Getpid() })
	if i < 0 {
		t.Fatalf("own pid %d not in %d processes", os.Getpid(), len(res.Processes))
	}
	p := res.Processes[i]
	wd, _ := os.Getwd()
	if p.PPID != os.Getppid() || p.UID != os.Geteuid() || p.Cwd != wd || !slices.Equal(p.Argv, os.Args) {
		t.Errorf("self = %+v, want ppid %d uid %d cwd %q argv %q", p, os.Getppid(), os.Geteuid(), wd, os.Args)
	}
	if p.RSSBytes < 1<<20 || p.CPUTime <= 0 || len(p.Unknown) > 0 || time.Since(p.StartTime) > time.Hour || time.Since(p.StartTime) < 0 {
		t.Errorf("self rss %d cpu %v unknown %v start %v", p.RSSBytes, p.CPUTime, p.Unknown, p.StartTime)
	}
	want := Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: port, PID: os.Getpid()}
	if !slices.Contains(res.Listeners, want) {
		t.Errorf("listener %+v not in %+v", want, res.Listeners)
	}
	for _, k := range []string{"proctable", "argv_cwd", "listeners"} {
		if res.Timings[k] <= 0 {
			t.Errorf("timing %q missing: %v", k, res.Timings)
		}
	}
}

// TestDecodePCBList decodes a net.inet.tcp.pcblist_n blob recorded on macOS 27.0.1 arm64
// (the LISTEN groups only); pids are so_last_pid and matched lsof at recording time.
func TestDecodePCBList(t *testing.T) {
	b, err := os.ReadFile("testdata/pcblist_n_listen.bin")
	if err != nil {
		t.Fatal(err)
	}
	ls, pcbs := decodePCBList(b)
	l := func(proto, addr string, port uint16, pid int) pcbListener {
		return pcbListener{Listener{proto, netip.MustParseAddr(addr), port, pid}, 501}
	}
	want := []pcbListener{
		l("tcp6", "::", 8080, 56883), l("tcp6", "::1", 5173, 56864),
		l("tcp6", "::", 63369, 934), l("tcp4", "0.0.0.0", 63369, 934),
		l("tcp4", "127.0.0.1", 39127, 1894), l("tcp4", "127.0.0.1", 9277, 1893),
		l("tcp6", "::", 5000, 1261), l("tcp4", "0.0.0.0", 5000, 1261),
		l("tcp6", "::", 7000, 1261), l("tcp4", "0.0.0.0", 7000, 1261),
	}
	if pcbs != 10 || !slices.Equal(ls, want) {
		t.Errorf("got %d PCBs %+v, want %+v", pcbs, ls, want)
	}
	if ls, pcbs := decodePCBList(append(b[:24:24], b[len(b)-24:]...)); pcbs != 0 || ls != nil { // header-only list as served to non-entitled callers
		t.Errorf("header-only list: %d %v", pcbs, ls)
	}
}

func TestDecodeProcArgs2(t *testing.T) {
	blob := append([]byte{2, 0, 0, 0}, "/bin/x\x00\x00\x00\x00x\x00-v\x00HOME=/\x00"...)
	if got, ok := decodeProcArgs2(blob); !ok || !slices.Equal(got, []string{"x", "-v"}) {
		t.Errorf("got %q %v", got, ok)
	}
	for _, bad := range [][]byte{nil, {1, 0, 0, 0}, {1, 0, 0, 0, 'x', 0}} {
		if got, ok := decodeProcArgs2(bad); ok {
			t.Errorf("decodeProcArgs2(%q) = %q, want failure", bad, got)
		}
	}
}

// BenchmarkCollect reports the p50 of each source and of the whole snapshot in ms.
func BenchmarkCollect(b *testing.B) {
	c := New()
	samples := map[string][]time.Duration{}
	procs := 0
	for b.Loop() {
		t := time.Now()
		res, err := c.Collect(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		samples["total"] = append(samples["total"], time.Since(t))
		for k, v := range res.Timings {
			samples[k] = append(samples[k], v)
		}
		procs = len(res.Processes)
	}
	for k, v := range samples {
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		b.ReportMetric(float64(v[len(v)/2])/1e6, k+"-p50-ms")
	}
	b.ReportMetric(float64(procs), "procs")
}
