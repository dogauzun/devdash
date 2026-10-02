//go:build linux

package collector

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
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

	mappedPort := listenV4Mapped(t)

	// The child inherits a dup of a listening socket (fd 3) and runs in a directory
	// that is removed while it runs.
	shared, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shared.Close() }()
	sharedPort := uint16(shared.Addr().(*net.TCPAddr).Port)
	sharedFile, err := shared.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sharedFile.Close() }()
	gone, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone = filepath.Join(gone, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("cat")
	child.Dir, child.ExtraFiles = gone, []*os.File{sharedFile}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	// Start returns when exec closes the CLOEXEC status pipe, before the kernel publishes
	// the new argv (create_elf_tables), so /proc/<pid>/cmdline can still be empty (DEV-47).
	// An echoed line proves cat runs its own code, so its argv is visible.
	if _, err := stdin.Write([]byte("ready\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child echo: %q, %v", line, err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

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
	if self.PPID != os.Getppid() || self.UID != os.Geteuid() || self.Cwd != wd || !slices.Equal(self.Argv, os.Args) {
		t.Errorf("self = %+v; want ppid %d uid %d cwd %q argv %q", self, os.Getppid(), os.Geteuid(), wd, os.Args)
	}
	if self.RSSBytes == 0 || self.Unknown != 0 {
		t.Errorf("self RSS %d, unknown %v", self.RSSBytes, self.Unknown)
	}
	if d := time.Since(self.StartTime); d < 0 || d > time.Hour {
		t.Errorf("self start time %v is %v ago", self.StartTime, d)
	}

	j := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == child.Process.Pid })
	if j < 0 {
		t.Errorf("child %d not in %d processes", child.Process.Pid, len(res.Processes))
	} else if c := res.Processes[j]; c.PPID != pid || !slices.Equal(c.Argv, []string{"cat"}) || c.Cwd != gone {
		t.Errorf("child = %+v; want ppid %d argv [cat] cwd %q (deleted, DEV-44)", c, pid, gone)
	}

	lo := netip.MustParseAddr("127.0.0.1")
	for _, want := range []Listener{
		{Proto: "tcp4", Addr: lo, Port: port, PID: pid},
		{Proto: "tcp6", Addr: netip.IPv6Loopback(), Port: port6, PID: pid},
		{Proto: "tcp4", Addr: lo, Port: mappedPort, PID: pid}, // ::ffff:127.0.0.1 (DEV-43)
	} {
		if !slices.Contains(res.Listeners, want) {
			t.Errorf("listener %+v not in %+v", want, res.Listeners)
		}
	}
	// A socket shared across fork is one listener owned by the lowest pid (DEV-45).
	var sharedRows []Listener
	for _, l := range res.Listeners {
		if l.Port == sharedPort {
			sharedRows = append(sharedRows, l)
		}
	}
	want := Listener{Proto: "tcp4", Addr: lo, Port: sharedPort, PID: min(pid, child.Process.Pid)}
	if !slices.Equal(sharedRows, []Listener{want}) {
		t.Errorf("fork-shared listener: got %+v, want %+v", sharedRows, want)
	}
}

// listenV4Mapped opens an AF_INET6 dual-stack socket bound to ::ffff:127.0.0.1, as the JVM
// does for a loopback bind; net.Listen cannot, it turns that address into AF_INET.
func listenV4Mapped(t *testing.T) uint16 {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	sa := &syscall.SockaddrInet6{Addr: netip.MustParseAddr("::ffff:127.0.0.1").As16()}
	if err := errors.Join(
		syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0),
		syscall.Bind(fd, sa),
		syscall.Listen(fd, 1),
	); err != nil {
		t.Fatal(err)
	}
	got, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(got.(*syscall.SockaddrInet6).Port)
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
