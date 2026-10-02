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
	"strings"
	"sync"
	"sync/atomic"
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
	_, mappedPort := listenV4Mapped(t)

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
	// Even when withheld (ad-hoc-signed ancestor, e.g. go test) the list holds our own sockets.
	pcb, others := decodePCBList(b, os.Getpid())
	t.Logf("PCB list: %d listeners, %d PCBs of other processes", len(pcb), others)
	for _, w := range want {
		if !slices.ContainsFunc(pcb, func(s sock) bool { return s.Listener == w }) {
			t.Errorf("PCB list: %+v missing", w)
		}
	}
}

// listenV4Mapped opens an AF_INET6 listener bound to ::ffff:127.0.0.1, as JVMs do by default.
func listenV4Mapped(t *testing.T) (fd int, port uint16) {
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
	return fd, uint16(got.(*unix.SockaddrInet6).Port)
}

func TestMergeListeners(t *testing.T) {
	a := Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 8080}
	at := func(pid int) Listener { l := a; l.PID = pid; return l }
	s := func(so uint64, pid int) sock { return sock{at(pid), so} }
	other := sock{Listener{Proto: "tcp4", Addr: netip.MustParseAddr("0.0.0.0"), Port: 22}, 9}
	for name, c := range map[string]struct {
		fd, pcb []sock
		want    []Listener
	}{
		"fork-shared socket goes to the lowest pid": {fd: []sock{s(1, 20), s(1, 10), s(1, 30)}, want: []Listener{at(10)}},
		"one pid, two fds of one socket":            {fd: []sock{s(1, 10), s(1, 10)}, want: []Listener{at(10)}},
		"SO_REUSEPORT siblings stay distinct":       {fd: []sock{s(1, 10), s(2, 11), s(3, 10)}, want: []Listener{at(10), at(11), at(10)}},
		"PCB list agrees on the handle":             {fd: []sock{s(1, 10), s(1, 20)}, pcb: []sock{s(1, 20), other}, want: []Listener{at(10), other.Listener}},
		"PCB list adds a distinct socket":           {fd: []sock{s(1, 10)}, pcb: []sock{s(2, 0)}, want: []Listener{at(10), at(0)}},
		"PCB list only":                             {pcb: []sock{other}, want: []Listener{other.Listener}},
	} {
		if got := mergeListeners(c.fd, c.pcb); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
}

// TestCollectForkSharedListener: a listening socket inherited by a child is one listener, owned
// by the lower pid (DEV-45).
func TestCollectForkSharedListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	f, err := ln.(*net.TCPListener).File() // a second fd for the same socket
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	got := slices.DeleteFunc(slices.Clone(res.Listeners), func(l Listener) bool { return l.Port != port })
	want := Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: port, PID: min(os.Getpid(), cmd.Process.Pid)}
	if !slices.Equal(got, []Listener{want}) {
		t.Errorf("got %+v, want exactly %+v", got, want)
	}
}

// TestCollectFindsChild: a child started with exec.Command is found under the test's pid.
func TestCollectFindsChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == cmd.Process.Pid })
	if i < 0 {
		t.Fatalf("child %d not found", cmd.Process.Pid)
	}
	wd, _ := os.Getwd()
	if p := res.Processes[i]; p.PPID != os.Getpid() || p.Name != "sleep" || !slices.Equal(p.Argv, cmd.Args) || p.Cwd != wd || p.Unknown != 0 {
		t.Errorf("child = %+v, want ppid %d argv %q cwd %q", p, os.Getpid(), cmd.Args, wd)
	}
}

// TestCollectArgvExact: empty argv strings are kept in place and the environment (here
// DEVDASH_BLOB=1) never shows up in argv (DEV-40).
func TestCollectArgvExact(t *testing.T) {
	argv := []string{"", "-c", "read x", "", "z"}
	pid := startBash(t, argv)
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == pid })
	if i < 0 {
		t.Fatalf("child %d not found", pid)
	}
	if got := res.Processes[i].Argv; !slices.Equal(got, argv) {
		t.Errorf("argv %q, want %q", got, argv)
	}
}

// TestCollectArgvTooLarge: when exec path, argv, env and apple strings exceed kern.argmax,
// kern.procargs2 fills the buffer with the tail of the strings area while argc stays the real
// one. The row stays, with argv unknown and no environment in it (DEV-40 review).
func TestCollectArgvTooLarge(t *testing.T) {
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatal(err)
	}
	argmax, err := unix.SysctlUint32("kern.argmax")
	if err != nil {
		t.Fatal(err)
	}
	// A long exec path counts twice (the path and executable_path=), pushing the area past argmax.
	dir := t.TempDir()
	for range 6 {
		dir += "/" + strings.Repeat("d", 100)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := dir + "/bash"
	if err := os.Symlink("/bin/bash", bin); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, argmax)
	pid := 0
	for size := int(argmax) - 200; size > int(argmax)-4000 && pid == 0; size -= 100 {
		cmd := &exec.Cmd{Path: bin, Args: []string{"bash", "-c", "read x", strings.Repeat("a", size)}, Env: []string{"SENTINEL_ENV=leaked"}}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Start() != nil { // E2BIG: try a smaller argument
			continue
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
		if n, err := lib.procArgs2(cmd.Process.Pid, buf); err == nil && n == len(buf) {
			pid = cmd.Process.Pid
		}
	}
	if pid == 0 {
		t.Fatal("no child filled the kern.procargs2 buffer")
	}
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == pid })
	if i < 0 {
		t.Fatalf("child %d dropped", pid)
	}
	if p := res.Processes[i]; p.Unknown != model.FieldArgv || p.Argv != nil {
		t.Errorf("child unknown %v argv %.80q, want argv unknown and nil", p.Unknown.Names(), p.Argv)
	}
}

// TestCollectChurn: while children are spawned and reaped, own-uid rows are complete or absent,
// never half-filled (DEV-41).
func TestCollectChurn(t *testing.T) {
	var stop atomic.Bool
	var spawned atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for !stop.Load() {
				if exec.Command("/usr/bin/true").Run() == nil {
					spawned.Add(1)
				}
			}
		})
	}
	defer wg.Wait()
	defer stop.Store(true)

	c := New()
	for runs := 0; runs < 100 || spawned.Load() < 300; runs++ {
		res, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range res.Processes {
			if p.UID == os.Geteuid() && p.Unknown != 0 {
				t.Errorf("run %d: own-uid row half-filled: %+v", runs, p)
			}
		}
	}
	t.Logf("%d children spawned", spawned.Load())
}

// TestCollectNoPID0: kernel_task is not a row; PID 0 is the "unknown owner" pseudo-process (DEV-42).
func TestCollectNoPID0(t *testing.T) {
	res, err := New().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == 0 }); i >= 0 {
		t.Errorf("pid 0 row: %+v", res.Processes[i])
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
