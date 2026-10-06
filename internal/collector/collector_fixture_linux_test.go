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

	"golang.org/x/sys/unix"

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
	gone       bool     // cwd removed: fixtureStat reports no links for it (DEV-116)
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
	deleted.wantCwd, deleted.gone = "/home/dev/gone", true // DEV-44, DEV-116
	procs = append(procs,
		// The user's service manager: no project, no listener, a short name (DEV-123).
		user(990, 1, "systemd", "/", []string{"/usr/lib/systemd/systemd", "--user"}),
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

// fixtureStat stands in for the stat of /proc/<pid>/cwd, whose checked-in link dangles: no
// links for a gone process's cwd. Statting any other path fails the test, since the collector
// should stat only a cwd that carries the " (deleted)" mark (DEV-116).
func fixtureStat(t *testing.T, procs []fixProc) func(string, *unix.Stat_t) error {
	return func(path string, st *unix.Stat_t) error {
		pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		i := slices.IndexFunc(procs, func(f fixProc) bool { return f.pid == pid })
		if filepath.Base(path) != "cwd" || i < 0 || !procs[i].gone {
			t.Errorf("stat %s: want only the cwd of a process whose cwd is gone", path)
			return unix.ENOENT
		}
		*st = unix.Stat_t{Nlink: 0}
		return nil
	}
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
		p.CwdDeleted = f.gone
		if f.pid == fixDenied && denied {
			p.Argv, p.RSSBytes, p.Unknown = nil, 0, model.FieldArgv|model.FieldMem
		}
		want = append(want, p)
	}
	return want
}

// TestCollectFixture runs the collector over testdata/proc-500, as a user and as root with
// and without CAP_SYS_PTRACE. When the tests run as a user, fixDenied's fd/ and cmdline give
// EACCES, so the root cases are a root that the kernel still denies, as in a Docker container
// with default capabilities (DEV-49). When they run as root nothing is denied, and the root
// owner hint names the pid namespace instead (DEV-66).
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
		ptrace  bool   // CAP_SYS_PTRACE is effective
		mounts  string // written to <root>/mounts; %s is the proc root
		owner   func(fixListen) int
		hidepid string
		// Substrings of the process_fields_unreadable hint (checked when fixDenied is denied)
		// and of Result.OwnerHint ("" when Build's default applies; as root, checked when
		// fixDenied is denied).
		fieldsHint, ownerHint string
	}{
		{name: "user", euid: fixUser, owner: func(l fixListen) int { return l.userOwner },
			fieldsHint: "run with sudo"},
		{name: "root", euid: 0, owner: func(l fixListen) int { return l.rootOwner },
			mounts: "sysfs /sys sysfs rw 0 0\nproc %s proc rw,nosuid,relatime,hidepid=invisible 0 0\n", hidepid: "hidepid=invisible",
			fieldsHint: "root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE",
			ownerHint:  "root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE"},
		{name: "root with CAP_SYS_PTRACE", euid: 0, ptrace: true, owner: func(l fixListen) int { return l.rootOwner },
			fieldsHint: "even to root", ownerHint: "even to root"},
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
			c.euid, c.ptrace, c.stat = tt.euid, tt.ptrace, fixtureStat(t, procs)
			res, err := c.Collect(context.Background(), Options{})
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
			for _, w := range res.Warnings {
				if w.Code == "process_fields_unreadable" && !strings.Contains(w.Hint, tt.fieldsHint) {
					t.Errorf("process_fields_unreadable hint %q does not contain %q", w.Hint, tt.fieldsHint)
				}
			}
			wantOwner := tt.ownerHint
			// As root a permission hint needs a denied read, or hidepid hiding other users' pids
			// from root without CAP_SYS_PTRACE.
			if tt.euid == 0 && !denied && (tt.hidepid == "" || tt.ptrace) {
				wantOwner = "pid namespace"
			}
			if (wantOwner == "") != (res.OwnerHint == "") || !strings.Contains(res.OwnerHint, wantOwner) {
				t.Errorf("owner hint %q, want one containing %q", res.OwnerHint, wantOwner)
			}
			if tt.euid == 0 && strings.Contains(fmt.Sprint(res.OwnerHint, res.Warnings), "sudo") {
				t.Errorf("as root a hint says sudo: %q %+v", res.OwnerHint, res.Warnings)
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
	ownerOf := func(port uint16) int {
		res, err := c.Collect(context.Background(), Options{})
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

	addListen(t, dir, 8090, fixUser, 2010) // the socket fixDenied holds
	if got := ownerOf(8090); got != 0 {
		t.Fatalf("tick 1: owner %d, want 0 (fd/ denied)", got)
	}
	if err := os.Chmod(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(8090); got != 0 {
		t.Fatalf("tick 2: owner %d, want 0 (same unmatched set, denied pid skipped)", got)
	}
	addListen(t, dir, 8091, fixUser, 2999) // a new unmatched inode, owned by nobody
	if got := ownerOf(8090); got != fixDenied {
		t.Fatalf("tick 3: owner %d, want %d (unmatched set changed, pid retried)", got, fixDenied)
	}
}

// TestCollectFixtureLimitedArgv: with Options.InProject (over 5000 processes), cmdline is read
// only for listener owners, for the processes InProject marks, here those under src/api, for
// long or runtime names and for processes named systemd, so a systemd --user manager still
// shows its --user (DEV-123); every other process keeps its row with argv unknown, and that
// does not count as unreadable (DEV-92).
func TestCollectFixtureLimitedArgv(t *testing.T) {
	procs, listens := proc500()
	dir, denied := copyProc500(t)
	const cron = 330 // root, cwd /, no listener: its cmdline must not be read
	if err := os.Remove(filepath.Join(dir, strconv.Itoa(cron), "cmdline")); err != nil {
		t.Fatal(err) // were it read, ENOENT would drop the process as exited
	}
	project := "/home/dev/src/api"
	calls := 0
	o := Options{InProject: func(ps []Process) []bool {
		calls++
		in := make([]bool, len(ps))
		for i, p := range ps {
			if p.Argv != nil || p.Cwd == "" {
				t.Errorf("InProject got pid %d with argv %q, cwd %q; want argv not read, cwd read", p.PID, p.Argv, p.Cwd)
			}
			in[i] = p.Cwd == project
		}
		return in
	}}
	c := newLinux(dir)
	c.euid, c.stat = fixUser, fixtureStat(t, procs)
	res, err := c.Collect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("InProject called %d times, want 1", calls)
	}

	owners := map[int]bool{}
	for _, l := range listens {
		if l.state == "" && l.userOwner != 0 {
			owners[l.userOwner] = true
		}
	}
	want := wantProcs(procs, denied)
	read := 0
	for i := range want {
		if p := &want[i]; !owners[p.PID] && p.Cwd != project && !model.NeedsArgv(p.Name) {
			p.Argv, p.Unknown = nil, p.Unknown|model.FieldArgv
		} else {
			read++
		}
	}
	if read < 10 || read > len(want)/2 {
		t.Fatalf("fixture reads argv for %d of %d processes; want a few", read, len(want))
	}
	if !reflect.DeepEqual(res.Processes, want) {
		for i := range min(len(res.Processes), len(want)) {
			if !reflect.DeepEqual(res.Processes[i], want[i]) {
				t.Fatalf("process %d:\n got %+v\nwant %+v", i, res.Processes[i], want[i])
			}
		}
		t.Fatalf("got %d processes, want %d", len(res.Processes), len(want))
	}
	userManager := []string{"/usr/lib/systemd/systemd", "--user"}
	if i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == 990 }); i < 0 || !slices.Equal(res.Processes[i].Argv, userManager) {
		t.Errorf("systemd --user (pid 990) missing or without argv %q", userManager)
	}

	var codes []string
	for _, w := range res.Warnings {
		codes = append(codes, fmt.Sprintf("%s:%d", w.Code, w.Count))
	}
	var wantCodes []string
	if denied { // fixDenied's statm; its argv is not read, so not denied
		wantCodes = append(wantCodes, "process_fields_unreadable:1")
	}
	if !slices.Equal(codes, wantCodes) {
		t.Errorf("warnings %q, want %q", codes, wantCodes)
	}
}

// TestReadCwd: the kernel's " (deleted)" mark is stripped from Cwd, and CwdDeleted is set only
// when a stat of the cwd link, made only for a marked link, shows no links left; a live
// directory really named "x (deleted)" (DEV-44) and a failed stat leave it false (DEV-116).
func TestReadCwd(t *testing.T) {
	live := filepath.Join(t.TempDir(), "x (deleted)")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	noLinks := func(_ string, st *unix.Stat_t) error { *st = unix.Stat_t{Nlink: 0}; return nil }
	tests := []struct {
		name, link string
		stat       func(string, *unix.Stat_t) error
		cwd        string
		deleted    bool
		statted    bool
	}{
		{"removed", "/home/dev/gone (deleted)", noLinks, "/home/dev/gone", true, true},
		{"live directory named x (deleted)", live, unix.Stat, strings.TrimSuffix(live, " (deleted)"), false, true},
		{"stat fails", "/nonexistent/gone (deleted)", unix.Stat, "/nonexistent/gone", false, true},
		{"no mark", filepath.Dir(live), noLinks, filepath.Dir(live), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Symlink(tt.link, filepath.Join(dir, "cwd")); err != nil {
				t.Fatal(err)
			}
			statted := false
			c := newLinux(t.TempDir())
			c.stat = func(path string, st *unix.Stat_t) error {
				statted = true
				if path != dir+"/cwd" {
					t.Errorf("stat %q, want %q", path, dir+"/cwd")
				}
				return tt.stat(path, st)
			}
			var p Process
			if !c.readCwd(dir, &p, map[int]bool{}) {
				t.Fatal("readCwd: process gone")
			}
			if p.Cwd != tt.cwd || p.CwdDeleted != tt.deleted || statted != tt.statted {
				t.Errorf("cwd %q deleted %v statted %v; want %q, %v, %v", p.Cwd, p.CwdDeleted, statted, tt.cwd, tt.deleted, tt.statted)
			}
		})
	}
}

// TestHints: every hint names what is unreadable, so the fields and owner hints never read as
// the same text twice when the footer joins them (DEV-87).
func TestHints(t *testing.T) {
	tests := []struct {
		name          string
		euid          int
		ptrace        bool
		fdDenied      bool
		fields, owner string
	}{
		{"user", fixUser, false, true,
			"processes of other users have unreadable fields; run with sudo to see them", ""},
		{"root", 0, false, true,
			"running as root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE to see them",
			"running as root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE to see owners"},
		{"root with CAP_SYS_PTRACE", 0, true, true,
			"some processes have unreadable fields: denied even to root with CAP_SYS_PTRACE, by a security module or sandbox",
			"listener owners unreadable: denied even to root with CAP_SYS_PTRACE, by a security module or sandbox"},
		{"root with CAP_SYS_PTRACE, nothing denied", 0, true, false,
			"some processes have unreadable fields: denied even to root with CAP_SYS_PTRACE, by a security module or sandbox",
			"owner not visible from this pid namespace, or the socket is held by the kernel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &linuxCollector{euid: tt.euid, ptrace: tt.ptrace}
			fields, owner := c.hints(tt.fdDenied)
			if fields != tt.fields || owner != tt.owner {
				t.Errorf("hints(%v)\n got %q, %q\nwant %q, %q", tt.fdDenied, fields, owner, tt.fields, tt.owner)
			}
		})
	}
}

// TestCollectOwnerHint: as root, the owner hint blames permissions only when the fd walk was
// denied a read, of fd/ or of an fd link. Otherwise an unowned listener belongs to a process
// outside devdash's pid namespace or to the kernel, and the hint says so (DEV-66).
func TestCollectOwnerHint(t *testing.T) {
	const (
		capability = "root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE"
		module     = "listener owners unreadable: denied even to root with CAP_SYS_PTRACE, by a security module or sandbox"
		namespace  = "not visible from this pid namespace, or the socket is held by the kernel"
	)
	tests := []struct {
		name   string
		fdMode os.FileMode // of fixDenied's fd/: 0 denies fd/, 0o444 its links, 0o755 nothing
		ptrace bool
		want   string
	}{
		{"nothing denied", 0o755, false, namespace},
		{"nothing denied, CAP_SYS_PTRACE", 0o755, true, namespace},
		{"fd/ denied", 0, false, capability},
		{"fd/ denied, CAP_SYS_PTRACE", 0, true, module},
		{"fd link denied", 0o444, false, capability},
		{"fd link denied, CAP_SYS_PTRACE", 0o444, true, module},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, denied := copyProc500(t)
			if tt.fdMode != 0o755 && !denied {
				t.Skip("running as root: chmod does not deny")
			}
			if err := os.Chmod(filepath.Join(dir, strconv.Itoa(fixDenied), "fd"), tt.fdMode); err != nil {
				t.Fatal(err)
			}
			addListen(t, dir, 8091, 0, 2999) // no process holds inode 2999: the kernel, or another pid namespace
			c := newLinux(dir)
			c.euid, c.ptrace = 0, tt.ptrace
			// Tick 2 skips a pid whose fd/ was denied on tick 1; that still counts as denied.
			for tick := 1; tick <= 2; tick++ {
				res, err := c.Collect(context.Background(), Options{})
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range res.Listeners {
					if l.Port == 8091 && l.PID != 0 {
						t.Fatalf("tick %d: :8091 owned by %d", tick, l.PID)
					}
				}
				if !strings.Contains(res.OwnerHint, tt.want) || strings.Contains(res.OwnerHint, "sudo") {
					t.Errorf("tick %d: owner hint %q, want one containing %q", tick, res.OwnerHint, tt.want)
				}
			}
			c = newLinux(dir)
			c.euid, c.ptrace = fixUser, tt.ptrace
			if res, err := c.Collect(context.Background(), Options{}); err != nil || res.OwnerHint != "" {
				t.Errorf("as a user: owner hint %q, err %v; want \"\" (Build's sudo hint)", res.OwnerHint, err)
			}
		})
	}
}

// TestReadListens: net/tcp and net/tcp6 are read in that order, a missing file (tcp6 with IPv6
// disabled) has no listeners, and any other read error is returned.
func TestReadListens(t *testing.T) {
	_, listens := proc500()
	var want []Listener
	for _, l := range listens {
		if l.state == "" {
			want = append(want, l.want)
		}
	}
	dir, _ := copyProc500(t)
	rows, err := readListens(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []Listener
	for _, r := range rows {
		got = append(got, r.Listener)
	}
	if !slices.Equal(got, want) {
		t.Errorf("listeners:\n got %+v\nwant %+v", got, want)
	}

	if err := os.Remove(filepath.Join(dir, "net/tcp6")); err != nil {
		t.Fatal(err)
	}
	if rows, err = readListens(dir); err != nil || len(rows) == 0 || len(rows) == len(want) {
		t.Errorf("without tcp6: %d listeners, err %v; want only tcp's", len(rows), err)
	}
	if err := os.Mkdir(filepath.Join(dir, "net/tcp6"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readListens(dir); err == nil {
		t.Error("tcp6 a directory: no error")
	}
}

// addListen appends a 127.0.0.1:port listener with the given socket uid and inode to the
// net/tcp of the proc root dir.
func addListen(t *testing.T, dir string, port uint16, uid int, inode uint64) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "net/tcp"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, "  99: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  %d        0 %d 1 0 100 0 0 10 0\n", port, uid, inode)
	if err := errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
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
		res, err := c.Collect(context.Background(), Options{})
		if err != nil {
			b.Fatal(err)
		}
		prev = model.Build(res, prev, nil, r)
	}
}
