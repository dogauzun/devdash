//go:build linux

package collector

import (
	"context"
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

// TestCollectFixture runs the collector on a hand-built proc root.
func TestCollectFixture(t *testing.T) {
	root := t.TempDir()
	me, other := os.Getuid(), os.Getuid()+1
	stat := func(pid int, state byte, flags uint64) string {
		// ppid 1, utime 7 + stime 3 ticks, starttime 500 ticks after boot
		return fmt.Sprintf("%d (my (proc)) %c 1 1 1 0 -1 %d 0 0 0 0 7 3 0 0 20 0 1 0 500 0 0\n", pid, state, flags)
	}
	status := func(uid int) string { return fmt.Sprintf("Name:\tx\nUid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid) }
	proc := func(pid, uid int, state byte, flags uint64, cmdline string, fds ...string) map[string]string {
		f := map[string]string{
			"stat": stat(pid, state, flags), "status": status(uid), "cmdline": cmdline,
			"statm": "300 25 0 0 0 0 0\n", "cwd": "/work/" + strconv.Itoa(pid),
			"fd/0": "/dev/null", "fd/1": "pipe:[7]",
		}
		for i, target := range fds {
			f["fd/"+strconv.Itoa(i+3)] = target
		}
		return f
	}
	files := map[string]map[string]string{
		"10": proc(10, me, 'S', 0, "node\x00server.js\x00", "socket:[100]"), // tcp4 owner
		"11": proc(11, me, 'R', 0, "vite\x00", "socket:[200]"),              // tcp6 owner
		"12": proc(12, me, 'S', 0, "secret\x00", "socket:[400]"),            // EACCES on cmdline, statm, fd/
		"13": proc(13, other, 'S', 0, "postgres\x00", "socket:[500]"),       // other uid: fds not scanned
		"16": proc(16, me, 'S', 0, "", "socket:[600]"),                      // blanked argv, still owns a port
		"2":  proc(2, 0, 'S', pfKthread, ""),                                // kernel thread
		"3":  proc(3, me, 'Z', 0, ""),                                       // zombie
		"14": {"stat": stat(14, 'S', 0), "status": status(me)},              // exited after status
		"15": {},                                                            // exited before stat
		".": {
			"stat":   "cpu  1 2 3\nbtime 1700000000\n",
			"mounts": "proc " + root + " proc rw,relatime,hidepid=invisible 0 0\n",
			"net/tcp": "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
				"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 100 1 0 100 0 0 10 0\n" +
				"   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 300 1 0 100 0 0 10 0\n" +
				"   2: 0100007F:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 400 1 0 100 0 0 10 0\n" +
				"   3: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 500 1 0 100 0 0 10 0\n" +
				"   4: 0100007F:1F92 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 600 1 0 100 0 0 10 0\n",
			"net/tcp6": "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
				"   0: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 200 1 0 100 0 0 10 0\n",
		},
	}
	for dir, entries := range files {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range entries {
			path := filepath.Join(root, dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			if name == "cwd" || strings.HasPrefix(name, "fd/") {
				err = os.Symlink(content, path)
			} else {
				err = os.WriteFile(path, []byte(content), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// A symlink's mode is ignored by readlink, so an unreadable cwd cannot be
	// faked with plain files; cmdline, statm and fd/ cover the EACCES paths.
	for _, path := range []string{"12/cmdline", "12/statm", "12/fd"} {
		if err := os.Chmod(filepath.Join(root, path), 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "12/fd"), 0o755) })
	asRoot := me == 0 // root ignores the 000 modes and may read every fd/

	res, err := newLinux(root).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Unix(1700000000, 0).Add(5 * time.Second)
	p := func(pid, uid int, argv ...string) Process {
		return Process{PID: pid, PPID: 1, UID: uid, StartTime: start, Name: "my (proc)", Argv: argv,
			Cwd: "/work/" + strconv.Itoa(pid), CPUTime: 100 * time.Millisecond, RSSBytes: 25 * uint64(os.Getpagesize())}
	}
	secret := p(12, me, "secret")
	if !asRoot {
		secret.Argv, secret.RSSBytes, secret.Unknown = nil, 0, model.FieldMem|model.FieldArgv
	}
	wantProcs := []Process{p(10, me, "node", "server.js"), p(11, me, "vite"), secret, p(13, other, "postgres"), p(16, me)}
	slices.SortFunc(res.Processes, func(a, b Process) int { return a.PID - b.PID })
	if !reflect.DeepEqual(res.Processes, wantProcs) {
		t.Errorf("processes:\n got %+v\nwant %+v", res.Processes, wantProcs)
	}

	ownerOf12, ownerOf13 := 0, 0
	if asRoot {
		ownerOf12, ownerOf13 = 12, 13
	}
	lo := netip.MustParseAddr("127.0.0.1")
	wantListeners := []Listener{
		{Proto: "tcp4", Addr: lo, Port: 8080, PID: 10},
		{Proto: "tcp4", Addr: netip.IPv4Unspecified(), Port: 22, PID: 0}, // no owner anywhere
		{Proto: "tcp4", Addr: lo, Port: 8081, PID: ownerOf12},
		{Proto: "tcp4", Addr: lo, Port: 5432, PID: ownerOf13},
		{Proto: "tcp4", Addr: lo, Port: 8082, PID: 16},
		{Proto: "tcp6", Addr: netip.IPv6Loopback(), Port: 3000, PID: 11},
	}
	if !slices.Equal(res.Listeners, wantListeners) {
		t.Errorf("listeners:\n got %+v\nwant %+v", res.Listeners, wantListeners)
	}

	if len(res.Warnings) == 0 || res.Warnings[0].Code != "proc_hidepid" || !strings.Contains(res.Warnings[0].Hint, "hidepid=invisible") {
		t.Errorf("warnings %+v: want the hidepid option named first", res.Warnings)
	}
}
