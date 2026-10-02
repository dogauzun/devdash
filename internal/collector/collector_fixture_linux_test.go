//go:build linux

package collector

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// The fixture proc root testdata/proc-500 is written by proc500 below; regenerate it with
//
//	go test ./internal/collector -run TestCollectFixture -update
//
// Git keeps no file modes but the exec bit, so EACCES cannot be checked in: the tests
// copy the tree to a temp dir and chmod the files of the fixDenied process to 000 there.
// Root ignores those modes, so the expectations for that process depend on the real euid.
// Symlinks (cwd, fd/*) are checked in as dangling links. Addresses in net/tcp{,6} are
// written little-endian, as amd64 and arm64 print them.
var update = flag.Bool("update", false, "rewrite testdata/proc-500")

const (
	fixtureDir = "testdata/proc-500"
	fixBtime   = 1700000000
	fixUser    = 1000 // the euid the "user" cases collect as
	fixOther   = 1001 // another user (postgres)
	fixDenied  = 1142 // own-uid process whose cmdline, statm and fd/ are chmodded to 000
)

// fixProc is one /proc/[pid] directory and what the collector should make of it.
type fixProc struct {
	pid, ppid  int
	ruid, euid int
	state      byte // 0 means 'S'
	flags      uint64
	comm       string
	argv       []string
	cmdline    string   // written instead of argv when set (rewritten argv, no NULs)
	cwd        string   // link target
	wantCwd    string   // expected Cwd when it differs from cwd
	fds        []string // link targets of fd/0, fd/1, ...
	only       []string // write only these files: the process exits mid-read
	drop       bool     // not expected in the result
}

type fixListen struct {
	file      string // "tcp" or "tcp6"
	addr      string
	port      uint16
	uid       int
	inode     uint64
	state     string // 0 means "0A"
	want      Listener
	userOwner int // expected owner collecting as fixUser
	rootOwner int // expected owner collecting as root
}

func proc500() ([]fixProc, []fixListen) {
	sock := func(inode int) string { return "socket:[" + strconv.Itoa(inode) + "]" }
	std := func(pid int, more ...string) []string {
		return append([]string{"/dev/null", "pipe:[" + strconv.Itoa(70000+pid) + "]"}, more...)
	}
	root := func(pid int, comm string, argv []string, fds ...string) fixProc {
		return fixProc{pid: pid, ppid: 1, comm: comm, argv: argv, cwd: "/", fds: std(pid, fds...)}
	}
	user := func(pid, ppid int, comm, cwd string, argv []string, fds ...string) fixProc {
		return fixProc{pid: pid, ppid: ppid, ruid: fixUser, euid: fixUser, comm: comm, argv: argv, cwd: cwd, fds: std(pid, fds...)}
	}
	src := "/home/dev/src/"

	procs := []fixProc{
		{pid: 1, comm: "systemd", argv: []string{"/sbin/init", "splash"}, cwd: "/", fds: std(1)},
		{pid: 2, comm: "kthreadd", flags: pfKthread | 0x8040, cwd: "/", drop: true},
	}
	for pid := 3; pid <= 80; pid++ { // kernel threads
		procs = append(procs, fixProc{pid: pid, ppid: 2, comm: fmt.Sprintf("kworker/%d:0", pid%8),
			flags: pfKthread | 0x4208060, state: "SI"[pid%2], cwd: "/", drop: true})
	}
	// Daemons that rewrite their argv into one string without NULs.
	sshd := root(300, "sshd", []string{"sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups"}, sock(1001), sock(1002))
	nginx := root(320, "nginx", []string{"nginx: master process /usr/sbin/nginx"}, sock(1005))
	sshd.cmdline, nginx.cmdline = sshd.argv[0], nginx.argv[0]
	procs = append(procs, sshd, nginx,
		fixProc{pid: 310, ppid: 1, ruid: fixOther, euid: fixOther, comm: "postgres",
			argv: []string{"/usr/lib/postgresql/16/bin/postgres", "-D", "/var/lib/postgresql/16/main"},
			cwd:  "/var/lib/postgresql/16/main", fds: std(310, sock(1003), sock(1004))},
		root(330, "cron", []string{"/usr/sbin/cron", "-f"}),
		root(335, "docker-proxy", []string{"/usr/bin/docker-proxy", "-proto", "tcp", "-host-port", "6379"}, sock(1006), sock(1007)),
		// sudo: real uid fixUser, effective uid 0; UID is the effective one.
		fixProc{pid: 400, ppid: 1001, ruid: fixUser, euid: 0, comm: "sudo", argv: []string{"sudo", "-E", "htop"}, cwd: src + "api", fds: std(400)},
	)
	for pid := 321; pid <= 324; pid++ { // nginx workers share the master's socket across fork
		procs = append(procs, fixProc{pid: pid, ppid: 320, ruid: 33, euid: 33, comm: "nginx",
			argv: []string{"nginx: worker process"}, cmdline: "nginx: worker process", cwd: "/", fds: std(pid, sock(1005))})
	}
	for pid := 500; pid < 600; pid++ {
		procs = append(procs, root(pid, fmt.Sprintf("svc-%d", pid), []string{fmt.Sprintf("/usr/libexec/svc-%d", pid), "--daemon"}))
	}

	deleted := user(1141, 1001, "perl", "/home/dev/gone (deleted)", []string{"perl", "-e", "sleep 600"})
	deleted.wantCwd = "/home/dev/gone" // DEV-44
	procs = append(procs,
		user(1000, 1, "tmux: server", "/home/dev", []string{"tmux", "new", "-s", "dev"}),
		user(1001, 1000, "zsh", src+"api", []string{"-zsh"}),
		user(1100, 1001, "node", src+"api", []string{"node", "server.js"}, sock(2001), sock(2100)),
		user(1101, 1001, "node", src+"web", []string{"node", src + "web/node_modules/.bin/vite"}, sock(2002)),
		user(1102, 1001, "java", src+"shop", []string{"java", "-jar", "app.jar"}, sock(2003)), // v4-mapped
		user(1103, 1001, "python3", src+"ml", []string{"python3", "-m", "http.server", "8001"}, sock(2004)),
		user(1110, 1001, "gunicorn", src+"blog", []string{"gunicorn", "-w", "4", "app:app"}, sock(2005)),
		user(1130, 1001, "envoy", src+"mesh", []string{"envoy", "-c", "a.yaml"}, sock(2007)), // SO_REUSEPORT pair
		user(1131, 1001, "envoy", src+"mesh", []string{"envoy", "-c", "b.yaml"}, sock(2008)),
		user(1140, 1001, "blank", src+"api", nil, sock(2009)), // blanked argv, still owns its port
		deleted,
		user(fixDenied, 1000, "gpg-agent", "/home/dev", []string{"gpg-agent", "--daemon"}, sock(2010)),
		fixProc{pid: 1143, ppid: 1100, ruid: fixUser, euid: fixUser, state: 'Z', comm: "node", cwd: "/", drop: true},
		fixProc{pid: 1144, ppid: 1100, ruid: fixUser, euid: fixUser, state: 'X', comm: "node", cwd: "/", drop: true},
		fixProc{pid: 1145, ppid: 1001, ruid: fixUser, euid: fixUser, comm: "make", only: []string{"stat", "status"}, drop: true},
		fixProc{pid: 1146, ppid: 1001, ruid: fixUser, euid: fixUser, comm: "cc", only: []string{"cmdline"}, drop: true},
		// Mid-exec (DEV-47): comm is already the new program's, but the kernel has not yet set
		// the argv range (create_elf_tables), so cmdline reads zero bytes. Kept, Argv nil.
		fixProc{pid: 1148, ppid: 1001, ruid: fixUser, euid: fixUser, state: 'R', comm: "sleep", cwd: src + "api", fds: std(1148)},
		// uvicorn 1200 forked 1150 (after the pid space wrapped): the lower pid owns the shared socket.
		user(1150, 1200, "uvicorn", src+"chat", []string{"uvicorn", "main:app", "--reload"}, sock(2006)),
		user(1200, 1001, "uvicorn", src+"chat", []string{"uvicorn", "main:app", "--reload"}, sock(2006)),
	)
	for pid := 1111; pid <= 1114; pid++ { // gunicorn workers share the master's socket
		procs = append(procs, user(pid, 1110, "gunicorn", src+"blog", []string{"gunicorn", "-w", "4", "app:app"}, sock(2005)))
	}
	editors := [][]string{{"-zsh"}, {"sleep", "300"}, {"nvim", "main.go"}, {"go", "test", "./..."}}
	for pid := 1300; pid < 1600; pid++ {
		argv := editors[pid%len(editors)]
		procs = append(procs, user(pid, 1000, filepath.Base(argv[0]), fmt.Sprintf("%sproj-%d", src, pid%20), argv, sock(90000+pid)))
	}
	// caddy was started as root, bound :443 and dropped to the user: its socket's uid is 0.
	procs = append(procs, user(1999, 1, "caddy", "/etc/caddy", []string{"caddy", "run"}, sock(1008)))
	slices.SortFunc(procs, func(a, b fixProc) int { return a.pid - b.pid })

	l := func(file, addr string, port uint16, uid int, inode uint64, userOwner, rootOwner int) fixListen {
		a := netip.MustParseAddr(addr)
		want := Listener{Proto: map[string]string{"tcp": "tcp4", "tcp6": "tcp6"}[file], Addr: a, Port: port}
		return fixListen{file: file, addr: addr, port: port, uid: uid, inode: inode, want: want, userOwner: userOwner, rootOwner: rootOwner}
	}
	mapped := l("tcp6", "::ffff:127.0.0.1", 8081, fixUser, 2003, 1102, 1102) // DEV-43
	mapped.want = Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: 8081}
	established := l("tcp", "127.0.0.1", 8080, fixUser, 2100, 0, 0)
	established.state = "01"
	listens := []fixListen{
		l("tcp", "0.0.0.0", 22, 0, 1001, 0, 300),
		l("tcp", "127.0.0.1", 5432, fixOther, 1003, 0, 310),
		l("tcp", "0.0.0.0", 80, 0, 1005, 0, 320), // fork-shared with the workers 321-324
		l("tcp", "0.0.0.0", 6379, 0, 1006, 0, 335),
		l("tcp", "127.0.0.1", 8080, fixUser, 2001, 1100, 1100),
		established, // not a listener
		l("tcp", "0.0.0.0", 9000, fixUser, 2005, 1110, 1110), // fork-shared with the workers 1111-1114
		l("tcp", "0.0.0.0", 7000, fixUser, 2007, 1130, 1130), // SO_REUSEPORT: two sockets, two listeners
		l("tcp", "0.0.0.0", 7000, fixUser, 2008, 1131, 1131),
		l("tcp", "127.0.0.1", 8082, fixUser, 2009, 1140, 1140),
		l("tcp", "127.0.0.1", 8000, fixUser, 2006, 1150, 1150), // lowest pid, not the parent
		// Root's socket held by an own-uid process: the walk stops before pid 1999 once every
		// own-uid listener is matched, so it stays unowned unless collecting as root.
		l("tcp", "0.0.0.0", 443, 0, 1008, 0, 1999),
		l("tcp6", "::", 22, 0, 1002, 0, 300),
		l("tcp6", "::1", 5432, fixOther, 1004, 0, 310),
		l("tcp6", "::1", 5173, fixUser, 2002, 1101, 1101),
		mapped,
		l("tcp6", "::", 8001, fixUser, 2004, 1103, 1103), // dual-stack: one tcp6 :: row
		l("tcp6", "::", 6379, 0, 1007, 0, 335),
	}
	return procs, listens
}

// procHex encodes an address as /proc/net/tcp{,6} prints it on a little-endian host.
func procHex(a netip.Addr) string {
	b := a.AsSlice()
	var s strings.Builder
	for i := 0; i < len(b); i += 4 {
		fmt.Fprintf(&s, "%08X", binary.LittleEndian.Uint32(b[i:]))
	}
	return s.String()
}

func writeProc500(t testing.TB, dir string) {
	procs, listens := proc500()
	files := map[string]string{"stat": fmt.Sprintf("cpu  10 0 20 300 0 0 0 0 0 0\nbtime %d\nprocesses 9000\n", fixBtime)}
	links := map[string]string{}
	for _, p := range procs {
		d := strconv.Itoa(p.pid) + "/"
		state := p.state
		if state == 0 {
			state = 'S'
		}
		cmdline := p.cmdline
		if cmdline == "" && p.argv != nil {
			cmdline = strings.Join(p.argv, "\x00") + "\x00"
		}
		f := map[string]string{
			"stat": fmt.Sprintf("%d (%s) %c %d %d %d 0 -1 %d 120 0 0 0 %d 3 0 0 20 0 1 0 %d 4321792 %d 18446744073709551615\n",
				p.pid, p.comm, state, p.ppid, p.pid, p.pid, p.flags, p.pid%50, 1000+7*p.pid, 100+p.pid%400),
			"status": fmt.Sprintf("Name:\t%s\nUmask:\t0022\nState:\t%c\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nUid:\t%d\t%d\t%d\t%d\nGid:\t%d\t%d\t%d\t%d\n",
				p.comm, state, p.pid, p.pid, p.ppid, p.ruid, p.euid, p.euid, p.euid, p.ruid, p.ruid, p.ruid, p.ruid),
			"cmdline": cmdline,
			"statm":   fmt.Sprintf("1055 %d 380 29 0 109 0\n", 100+p.pid%400),
		}
		if p.flags&pfKthread != 0 {
			f["statm"] = "0 0 0 0 0 0 0\n"
		}
		for name, content := range f {
			if p.only == nil || slices.Contains(p.only, name) {
				files[d+name] = content
			}
		}
		if p.only == nil {
			links[d+"cwd"] = p.cwd
			for i, target := range p.fds {
				links[d+"fd/"+strconv.Itoa(i)] = target
			}
		}
	}
	var tcp [2]strings.Builder
	for i, h := range []string{"local_address rem_address  ", "local_address                         remote_address                       "} {
		fmt.Fprintf(&tcp[i], "  sl  %s st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n", h)
	}
	for _, ln := range listens {
		i, zero := 0, netip.IPv4Unspecified()
		if ln.file == "tcp6" {
			i, zero = 1, netip.IPv6Unspecified()
		}
		state, remote, remotePort := ln.state, zero, uint16(0)
		if state == "" {
			state = "0A"
		} else {
			remote, remotePort = netip.MustParseAddr(ln.addr), 50000
		}
		fmt.Fprintf(&tcp[i], "%4d: %s:%04X %s:%04X %s 00000000:00000000 00:00000000 00000000 %5d        0 %d 1 0000000000000000 100 0 0 10 0\n",
			strings.Count(tcp[i].String(), "\n")-1, procHex(netip.MustParseAddr(ln.addr)), ln.port, procHex(remote), remotePort, state, ln.uid, ln.inode)
	}
	files["net/tcp"], files["net/tcp6"] = tcp[0].String(), tcp[1].String()

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
}

// copyProc500 copies the fixture to a temp dir and makes fixDenied's cmdline, statm and fd/
// unreadable. It reports whether that took effect (it does not for root).
func copyProc500(t testing.TB) (dir string, denied bool) {
	dir = t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(fixtureDir)); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, strconv.Itoa(fixDenied))
	for _, name := range []string{"cmdline", "statm", "fd"} {
		if err := os.Chmod(filepath.Join(d, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(d, "fd"), 0o755) }) // so TempDir can remove it
	_, err := os.ReadFile(filepath.Join(d, "cmdline"))
	return dir, err != nil
}

// wantProcs is what the collector should make of the fixture's processes.
func wantProcs(procs []fixProc, denied bool) []Process {
	var want []Process
	for _, f := range procs {
		if f.drop {
			continue
		}
		p := Process{
			PID: f.pid, PPID: f.ppid, UID: f.euid, Name: f.comm, Argv: f.argv, Cwd: f.cwd,
			StartTime: time.Unix(fixBtime, 0).Add(time.Duration(1000+7*f.pid) * 10 * time.Millisecond),
			CPUTime:   time.Duration(f.pid%50+3) * 10 * time.Millisecond,
			RSSBytes:  uint64(100+f.pid%400) * uint64(os.Getpagesize()),
		}
		if f.wantCwd != "" {
			p.Cwd = f.wantCwd
		}
		if f.pid == fixDenied && denied {
			p.Argv, p.RSSBytes, p.Unknown = nil, 0, model.FieldArgv|model.FieldMem
		}
		want = append(want, p)
	}
	return want
}

// TestCollectFixture runs the collector over testdata/proc-500, as a user and as root.
func TestCollectFixture(t *testing.T) {
	if *update {
		if err := os.RemoveAll(fixtureDir); err != nil {
			t.Fatal(err)
		}
		writeProc500(t, fixtureDir)
	}
	procs, listens := proc500()
	tests := []struct {
		name    string
		euid    int
		mounts  string // written to <root>/mounts; %s is the proc root
		owner   func(fixListen) int
		hidepid string
	}{
		{name: "user", euid: fixUser, owner: func(l fixListen) int { return l.userOwner }},
		{name: "root", euid: 0, owner: func(l fixListen) int { return l.rootOwner },
			mounts: "sysfs /sys sysfs rw 0 0\nproc %s proc rw,nosuid,relatime,hidepid=invisible 0 0\n", hidepid: "hidepid=invisible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, denied := copyProc500(t)
			if tt.mounts != "" {
				if err := os.WriteFile(filepath.Join(dir, "mounts"), fmt.Appendf(nil, tt.mounts, dir), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := newLinux(dir)
			c.euid = tt.euid
			res, err := c.Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			want := wantProcs(procs, denied)
			if len(want) < 400 {
				t.Fatalf("fixture has only %d processes", len(want))
			}
			if !reflect.DeepEqual(res.Processes, want) {
				for i := range min(len(res.Processes), len(want)) {
					if !reflect.DeepEqual(res.Processes[i], want[i]) {
						t.Fatalf("process %d:\n got %+v\nwant %+v", i, res.Processes[i], want[i])
					}
				}
				t.Fatalf("got %d processes, want %d", len(res.Processes), len(want))
			}

			var wantL []Listener
			for _, l := range listens {
				if l.state == "" {
					l.want.PID = tt.owner(l)
					wantL = append(wantL, l.want)
				}
			}
			if !slices.Equal(res.Listeners, wantL) {
				t.Errorf("listeners:\n got %+v\nwant %+v", res.Listeners, wantL)
			}

			var codes []string
			for _, w := range res.Warnings {
				codes = append(codes, fmt.Sprintf("%s:%d", w.Code, w.Count))
			}
			var wantCodes []string
			if tt.hidepid != "" {
				wantCodes = append(wantCodes, "proc_hidepid:1")
				if !strings.Contains(res.Warnings[0].Hint, tt.hidepid) {
					t.Errorf("hidepid hint %q does not name %s", res.Warnings[0].Hint, tt.hidepid)
				}
			}
			if denied {
				wantCodes = append(wantCodes, "process_fields_unreadable:1")
			}
			if !slices.Equal(codes, wantCodes) {
				t.Errorf("warnings %q, want %q", codes, wantCodes)
			}
		})
	}
}

// TestCollectSkipsDeniedFDs: a pid whose fd/ was EACCES is not walked again until the set
// of unmatched own-uid listener inodes changes.
func TestCollectSkipsDeniedFDs(t *testing.T) {
	dir, denied := copyProc500(t)
	if !denied {
		t.Skip("running as root: chmod 000 does not deny")
	}
	c := newLinux(dir)
	c.euid = fixUser
	addListen := func(port uint16, inode uint64) {
		f, err := os.OpenFile(filepath.Join(dir, "net/tcp"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(f, "  99: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  %d        0 %d 1 0 100 0 0 10 0\n", port, fixUser, inode)
		if err := errors.Join(err, f.Close()); err != nil {
			t.Fatal(err)
		}
	}
	ownerOf := func(port uint16) int {
		res, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range res.Listeners {
			if l.Port == port {
				return l.PID
			}
		}
		t.Fatalf("no listener on %d", port)
		return 0
	}
	fdDir := filepath.Join(dir, strconv.Itoa(fixDenied), "fd")

	addListen(8090, 2010) // the socket fixDenied holds
	if got := ownerOf(8090); got != 0 {
		t.Fatalf("tick 1: owner %d, want 0 (fd/ denied)", got)
	}
	if err := os.Chmod(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(8090); got != 0 {
		t.Fatalf("tick 2: owner %d, want 0 (same unmatched set, denied pid skipped)", got)
	}
	addListen(8091, 2999) // a new unmatched inode, owned by nobody
	if got := ownerOf(8090); got != fixDenied {
		t.Fatalf("tick 3: owner %d, want %d (unmatched set changed, pid retried)", got, fixDenied)
	}
}

// BenchmarkSnapshot measures collector plus model.Build over the 500-process fixture. It
// reads a temp copy, so a checkout on a slow mount (a Docker bind mount) does not count.
func BenchmarkSnapshot(b *testing.B) {
	dir, _ := copyProc500(b)
	c := newLinux(dir)
	c.euid = fixUser
	r := model.NewResolver(b.TempDir(), nil)
	var prev model.Snapshot
	for b.Loop() {
		res, err := c.Collect(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		prev = model.Build(res, prev, nil, r)
	}
}
