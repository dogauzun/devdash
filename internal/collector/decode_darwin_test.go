//go:build darwin

package collector

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Recorded blobs live in testdata as <source>.<case>.<macos-VERSION.ARCH>.bin and are written by
// TestRecordBlobs from processes whose argv, environment and cwd the test fixes, so no blob holds
// secrets and every OS version's blobs decode to the same expected values below.

// argvCases are /bin/bash children (bash -c 'read x' waits on stdin) and their expected argv.
var argvCases = []struct {
	name       string
	argv, want []string
}{
	{"plain", []string{"bash", "-c", "read x"}, []string{"bash", "-c", "read x"}},
	{"argv0-empty", []string{"", "-c", "read x"}, []string{"", "-c", "read x"}},
	{"empty-middle", []string{"bash", "-c", "read x", "", "z"}, []string{"bash", "-c", "read x", "", "z"}},
	{"trailing-empty", []string{"bash", "-c", "read x", ""}, []string{"bash", "-c", "read x"}},
}

const (
	blobEnv = "DEVDASH_BLOB=1" // the children's whole environment
	blobCwd = "/usr/bin"       // the children's cwd
)

// sockCases are sockets of the test process itself; want is zero for a non-listener.
var sockCases = []struct {
	name string
	open func(t *testing.T) int // returns the fd
	want Listener               // Proto and Addr; the port is whatever the kernel picked
}{
	{"listen-127.0.0.1", func(t *testing.T) int { return listenFD(t, "tcp4", "127.0.0.1:0") }, Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1")}},
	{"listen-v6-loopback", func(t *testing.T) int { return listenFD(t, "tcp6", "[::1]:0") }, Listener{Proto: "tcp6", Addr: netip.MustParseAddr("::1")}},
	{"listen-dual", func(t *testing.T) int { return listenFD(t, "tcp", ":0") }, Listener{Proto: "tcp6", Addr: netip.IPv6Unspecified()}},
	{"listen-v4-mapped", func(t *testing.T) int { fd, _ := listenV4Mapped(t); return fd }, Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1")}},
	{"connected", connectedFD, Listener{}},
}

func TestDecodeRecordedBlobs(t *testing.T) {
	tags, _ := filepath.Glob("testdata/procargs2.plain.*.bin")
	if len(tags) == 0 {
		t.Fatal("no recorded blobs in testdata")
	}
	for _, tag := range tags {
		tag = strings.TrimSuffix(strings.TrimPrefix(tag, "testdata/procargs2.plain."), ".bin")
		t.Run(tag, func(t *testing.T) {
			read := func(name string) []byte {
				b, err := os.ReadFile("testdata/" + name + "." + tag + ".bin")
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			for _, c := range argvCases {
				if got, ok := decodeProcArgs2(read("procargs2." + c.name)); !ok || !slices.Equal(got, c.want) {
					t.Errorf("procargs2 %s: got %q %v, want %q", c.name, got, ok, c.want)
				}
			}
			if rss, ticks, ok := decodeTaskInfo(read("proc_taskinfo")); !ok || rss < 16<<10 || rss > 1<<30 || ticks == 0 {
				t.Errorf("proc_taskinfo: rss %d ticks %d ok %v", rss, ticks, ok)
			}
			if cwd, ok := decodeVnodePathInfo(read("proc_vnodepathinfo")); cwd != blobCwd || !ok {
				t.Errorf("proc_vnodepathinfo: cwd %q %v, want %q", cwd, ok, blobCwd)
			}
			var listeners []sock
			for _, c := range sockCases {
				s, ok := decodeSocketFDInfo(read("socket_fdinfo." + c.name))
				if c.want == (Listener{}) {
					if ok {
						t.Errorf("socket_fdinfo %s: got listener %+v", c.name, s)
					}
					continue
				}
				if !ok || s.Proto != c.want.Proto || s.Addr != c.want.Addr || s.Port == 0 || s.so == 0 {
					t.Errorf("socket_fdinfo %s: got %+v %v, want %+v with a port and handle", c.name, s, ok, c.want)
				}
				listeners = append(listeners, s)
			}

			// The PCB list is recorded only when it was served. It holds the test process's
			// listeners: the ones above (same handles) and the "connected" case's listener.
			b, err := os.ReadFile("testdata/pcblist_n.own." + tag + ".bin")
			if err != nil {
				t.Logf("no PCB list blob for %s: %v", tag, err)
				return
			}
			pcb, n := decodePCBList(b, -1)
			if _, others := decodePCBList(b, pcb[0].PID); others != 0 {
				t.Errorf("PCB list of one pid: %d PCBs of others, want 0 (reads as withheld)", others)
			}
			if n != len(listeners)+1 || len(pcb) != n {
				t.Fatalf("PCB list: %d PCBs, %d listeners, want %d of each", n, len(pcb), len(listeners)+1)
			}
			for _, s := range listeners {
				i := slices.IndexFunc(pcb, func(p sock) bool { return p.so == s.so })
				if i < 0 || pcb[i].Proto != s.Proto || pcb[i].Addr != s.Addr || pcb[i].Port != s.Port || pcb[i].PID != pcb[0].PID || pcb[i].PID <= 0 {
					t.Errorf("PCB list: socket_fdinfo %+v (handle %#x) not matched in %+v", s.Listener, s.so, pcb)
				}
			}
		})
	}
}

// TestRecordBlobs re-records the blobs above for this macOS version and arch:
//
//	DEVDASH_RECORD=1 go test -run TestRecordBlobs ./internal/collector
//
// The PCB list blob holds only the test's own listeners, which the kernel returns even when it
// withholds everyone else's (ad-hoc-signed ancestor such as go test).
func TestRecordBlobs(t *testing.T) {
	if os.Getenv("DEVDASH_RECORD") == "" {
		t.Skip("set DEVDASH_RECORD=1")
	}
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatal(err)
	}
	ver, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		t.Fatal(err)
	}
	tag := "macos-" + ver + "." + runtime.GOARCH
	write := func(name string, b []byte) {
		path := "testdata/" + name + "." + tag + ".bin"
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(b))
	}

	buf := make([]byte, 1<<20)
	for _, c := range argvCases {
		pid := startBash(t, c.argv)
		n, err := lib.procArgs2(pid, buf)
		if err != nil {
			t.Fatalf("procargs2 %s: %v", c.name, err)
		}
		write("procargs2."+c.name, buf[:n])
		if c.name != "plain" {
			continue
		}
		for name, flavor := range map[string]int{"proc_taskinfo": procPidTaskInfo, "proc_vnodepathinfo": procPidVnodePathInfo} {
			n, err := lib.pidinfo(pid, flavor, buf)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			write(name, buf[:n])
		}
	}
	for _, c := range sockCases {
		n, err := lib.pidfdinfo(os.Getpid(), c.open(t), procPidFDSocketInfo, buf[:sizeofSocketFDInfo])
		if err != nil {
			t.Fatalf("socket_fdinfo %s: %v", c.name, err)
		}
		write("socket_fdinfo."+c.name, buf[:n])
	}

	// Only this process's listener groups, between the list's opening and closing xinpgen.
	b, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		t.Fatal(err)
	}
	out := slices.Clone(b[:sizeofXgen])
	end := walkPCBList(b, func(g []byte, s sock, listen bool) {
		if listen && s.PID == os.Getpid() {
			out = append(out, g...)
		}
	})
	if len(out) == sizeofXgen {
		t.Fatal("PCB list holds none of this process's listeners")
	}
	write("pcblist_n.own", append(out, b[end:]...))
}

// startBash starts /bin/bash with argv in blobCwd with only blobEnv as its environment, waiting on
// its stdin, and returns its pid; cmd.Start returns after the exec.
func startBash(t *testing.T, argv []string) int {
	t.Helper()
	cmd := &exec.Cmd{Path: "/bin/bash", Args: argv, Env: []string{blobEnv}, Dir: blobCwd}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func listenFD(t *testing.T, network, addr string) int {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return sysFD(t, ln.(*net.TCPListener))
}

// connectedFD returns the client end of an established loopback connection.
func connectedFD(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return sysFD(t, c.(*net.TCPConn))
}

func sysFD(t *testing.T, c syscall.Conn) int {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fd := -1
	if err := rc.Control(func(f uintptr) { fd = int(f) }); err != nil {
		t.Fatal(err)
	}
	return fd
}

// procArgs2Blob builds a kern.procargs2 buffer as XNU lays it out: argc, the exec path NUL-padded
// to a multiple of 8 from its start, the argv strings, then the environment.
func procArgs2Blob(path string, argv []string, env ...string) []byte {
	b := le.AppendUint32(nil, uint32(len(argv)))
	b = append(b, path...)
	b = append(b, make([]byte, roundup8(uint32(len(path)+1))-len(path))...)
	for _, s := range slices.Concat(argv, env) {
		b = append(append(b, s...), 0)
	}
	return b
}

// TestDecodeProcArgs2 covers every exec path length mod 8 (the padding the old decoder misread,
// DEV-40), empty argv strings, a blanked argv and malformed buffers. The environment must never
// appear in argv.
func TestDecodeProcArgs2(t *testing.T) {
	env := []string{"SECRET=leaked", "HOME=/"}
	for n := 1; n <= 16; n++ {
		path := "/" + strings.Repeat("p", n-1)
		for _, c := range []struct{ argv, want []string }{
			{[]string{"x", "-v"}, []string{"x", "-v"}},
			{[]string{"", "600"}, []string{"", "600"}},
			{[]string{"a", "", "", "b"}, []string{"a", "", "", "b"}},
			{[]string{"", "", "b"}, []string{"", "", "b"}},
			{[]string{"a", "b", ""}, []string{"a", "b"}},
			{[]string{""}, nil},
			{[]string{"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", "\x00", "\x00"}, nil}, // blanked in place: all NULs, then env
			{[]string{}, nil},
		} {
			got, ok := decodeProcArgs2(procArgs2Blob(path, c.argv, env...))
			if !ok || !slices.Equal(got, c.want) {
				t.Errorf("path %q argv %q: got %q %v, want %q", path, c.argv, got, ok, c.want)
			}
		}
	}

	good := procArgs2Blob("/bin/x", []string{"x", "y"})
	for name, bad := range map[string][]byte{
		"empty":           nil,
		"argc only":       {1, 0, 0, 0},
		"path unpadded":   {1, 0, 0, 0, 'x', 0},
		"no NUL":          append([]byte{1, 0, 0, 0}, "/bin/x"...),
		"truncated argv":  good[:len(good)-1],
		"argc beyond end": append(le.AppendUint32(nil, 3), good[4:]...),
		"negative argc":   append(le.AppendUint32(nil, 0xffffffff), good[4:]...),
		"huge argc":       append(le.AppendUint32(nil, 1<<30), good[4:]...),
	} {
		if got, ok := decodeProcArgs2(bad); ok {
			t.Errorf("%s: got %q, want failure", name, got)
		}
	}
}

// TestDecodePCBList decodes a net.inet.tcp.pcblist_n blob recorded with sysctl(8) on macOS 27.0.1
// arm64 (the LISTEN groups only); pids are so_last_pid and matched lsof at recording time.
func TestDecodePCBList(t *testing.T) {
	b, err := os.ReadFile("testdata/pcblist_n_listen.macos-27.0.1.arm64.bin")
	if err != nil {
		t.Fatal(err)
	}
	ls, pcbs := decodePCBList(b, -1)
	if _, others := decodePCBList(b, 1261); others != 6 {
		t.Errorf("PCBs of pids other than 1261: %d, want 6", others)
	}
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
	got := make([]Listener, len(ls))
	handles := map[uint64]bool{}
	for i, s := range ls {
		got[i] = s.Listener
		handles[s.so] = true
	}
	if pcbs != 10 || !slices.Equal(got, want) {
		t.Errorf("got %d PCBs %+v, want %+v", pcbs, got, want)
	}
	if len(handles) != len(ls) || handles[0] {
		t.Errorf("socket handles not distinct and non-zero: %v", handles)
	}
	if ls, pcbs := decodePCBList(append(b[:24:24], b[len(b)-24:]...), -1); pcbs != 0 || ls != nil { // header-only list as served to non-entitled callers
		t.Errorf("header-only list: %d %v", pcbs, ls)
	}
}
