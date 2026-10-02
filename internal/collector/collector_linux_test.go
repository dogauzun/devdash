//go:build linux

package collector

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
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
	ln6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln6.Close() }()
	port6 := uint16(ln6.Addr().(*net.TCPAddr).Port)

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()

	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"proctable", "argv_cwd", "listeners"} {
		if _, ok := res.Timings[name]; !ok {
			t.Errorf("Timings[%q] missing", name)
		}
	}

	pid := os.Getpid()
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == pid })
	if i < 0 {
		t.Fatalf("own pid %d not in %d processes", pid, len(res.Processes))
	}
	self := res.Processes[i]
	wd, _ := os.Getwd()
	if self.PPID != os.Getppid() || self.UID != os.Getuid() || self.Cwd != wd || !slices.Equal(self.Argv, os.Args) {
		t.Errorf("self = %+v; want ppid %d uid %d cwd %q argv %q", self, os.Getppid(), os.Getuid(), wd, os.Args)
	}
	if self.RSSBytes == 0 || self.Unknown != 0 {
		t.Errorf("self RSS %d, unknown %v", self.RSSBytes, self.Unknown)
	}
	if d := time.Since(self.StartTime); d < 0 || d > time.Hour {
		t.Errorf("self start time %v is %v ago", self.StartTime, d)
	}

	if !slices.ContainsFunc(res.Processes, func(p Process) bool {
		return p.PID == child.Process.Pid && p.PPID == pid && slices.Equal(p.Argv, []string{"sleep", "30"})
	}) {
		t.Errorf("child %d not found under pid %d", child.Process.Pid, pid)
	}

	for _, want := range []Listener{
		{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: port, PID: pid},
		{Proto: "tcp6", Addr: netip.IPv6Loopback(), Port: port6, PID: pid},
	} {
		if !slices.Contains(res.Listeners, want) {
			t.Errorf("listener %+v not in %+v", want, res.Listeners)
		}
	}
}

// BenchmarkCollect runs against the live /proc and reports the p50 of each source.
func BenchmarkCollect(b *testing.B) {
	c := New()
	samples := map[string][]time.Duration{}
	for b.Loop() {
		start := time.Now()
		res, err := c.Collect(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		samples["total"] = append(samples["total"], time.Since(start))
		for k, v := range res.Timings {
			samples[k] = append(samples[k], v)
		}
	}
	for k, s := range samples {
		slices.Sort(s)
		b.ReportMetric(float64(s[len(s)/2])/float64(time.Millisecond), k+"-p50-ms")
	}
}
