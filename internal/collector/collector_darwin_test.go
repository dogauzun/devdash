//go:build darwin

package collector

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"sort"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/model"
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
	if p.RSSBytes < 1<<20 || p.CPUTime <= 0 || p.Unknown != 0 || time.Since(p.StartTime) > time.Hour || time.Since(p.StartTime) < 0 {
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

// TestListenerFamilies checks that the fd walk and, when the kernel serves it, the PCB list name
// a tcp6, a v4-mapped and a dual-stack listener identically and that Collect lists each once.
func TestListenerFamilies(t *testing.T) {
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v6.Close() }()
	dual, err := net.Listen("tcp", ":0") // AF_INET6 with IPV6_V6ONLY off
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dual.Close() }()
	mappedPort := listenV4Mapped(t)

	pid := os.Getpid()
	want := []Listener{
		{Proto: "tcp6", Addr: netip.MustParseAddr("::1"), Port: uint16(v6.Addr().(*net.TCPAddr).Port), PID: pid},
		{Proto: "tcp6", Addr: netip.IPv6Unspecified(), Port: uint16(dual.Addr().(*net.TCPAddr).Port), PID: pid},
		{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: mappedPort, PID: pid}, // lsof shows 127.0.0.1 too
	}
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		got := slices.DeleteFunc(slices.Clone(res.Listeners), func(l Listener) bool { return l.Port != w.Port })
		if !slices.Equal(got, []Listener{w}) {
			t.Errorf("port %d: got %+v, want exactly %+v", w.Port, got, w)
		}
	}

	b, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		t.Fatal(err)
	}
	pcb, n := decodePCBList(b)
	if n == 0 {
		t.Log("PCB list not served (ad-hoc-signed ancestor, e.g. go test); run the test binary from a shell to check it")
		return
	}
	for _, w := range want {
		if !slices.Contains(pcb, w) {
			t.Errorf("PCB list: %+v missing", w)
		}
	}
}

// listenV4Mapped opens an AF_INET6 listener bound to ::ffff:127.0.0.1, as JVMs do by default.
func listenV4Mapped(t *testing.T) uint16 {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	sa := &unix.SockaddrInet6{Addr: netip.MustParseAddr("::ffff:127.0.0.1").As16()}
	if err := unix.Bind(fd, sa); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	got, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(got.(*unix.SockaddrInet6).Port)
}

func TestMergeListenersOneRowPerSocket(t *testing.T) {
	a := Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 8080, PID: 10}
	other := Listener{Proto: "tcp4", Addr: netip.MustParseAddr("0.0.0.0"), Port: 22}
	got := mergeListeners([]Listener{a, a}, []Listener{{Proto: "tcp4", Addr: a.Addr, Port: 8080, PID: 11}, other})
	if !slices.Equal(got, []Listener{a, other}) {
		t.Errorf("got %+v", got)
	}
}

// TestCollectDropsZombie: a process that is gone by the time libproc is asked (here a zombie,
// ESRCH) is dropped rather than shown half-filled.
func TestCollectDropsZombie(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err == nil && kp.Proc.P_stat == 5 { // SZOMB
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a zombie")
		}
	}
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == pid }); i >= 0 {
		t.Errorf("zombie kept: %+v", res.Processes[i])
	}
}

func TestExited(t *testing.T) {
	for err, want := range map[error]bool{syscall.ESRCH: true, syscall.EPERM: false, syscall.EINVAL: false, nil: false} {
		if exited(err) != want {
			t.Errorf("exited(%v) = %v", err, !want)
		}
	}
}

// TestCollectOtherUsers: other users' processes stay as rows with argv, cwd, cpu and mem
// unknown, counted in one warning.
func TestCollectOtherUsers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a normal user")
	}
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == 1 })
	if i < 0 {
		t.Fatal("launchd (pid 1) missing")
	}
	if p := res.Processes[i]; p.UID != 0 || p.Name != "launchd" || p.Unknown != model.FieldArgv|model.FieldCwd|model.FieldCPU|model.FieldMem {
		t.Errorf("pid 1 = %+v", p)
	}
	others := 0
	for _, p := range res.Processes {
		if p.UID != os.Geteuid() {
			others++
		}
	}
	if n := len(slices.DeleteFunc(slices.Clone(res.Warnings), func(w model.Warning) bool {
		return w.Code != "process_fields_unreadable" || w.Count != others
	})); n != 1 {
		t.Errorf("warnings %+v, want one process_fields_unreadable with count %d", res.Warnings, others)
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
	l := func(proto, addr string, port uint16, pid int) Listener {
		return Listener{Proto: proto, Addr: netip.MustParseAddr(addr), Port: port, PID: pid}
	}
	want := []Listener{
		l("tcp6", "::", 8080, 56883), // dual-stack (netstat tcp46): one tcp6 row
		l("tcp6", "::1", 5173, 56864),
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
