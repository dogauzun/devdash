//go:build linux

package engine

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

const listenHelperEnv = "DEVDASH_KILL_LISTEN_HELPER" // set: run TestKillLiveListenerHelper's listener

// stat builds a /proc/[pid]/stat line with the given state, ppid, num_threads (field 20) and
// starttime (field 22); the comm holds a space and a ')', as the parser must allow.
func stat(state string, ppid, threads int, start uint64) []byte {
	return fmt.Appendf(nil, "4242 (a b) c) %s %d 4242 4242 0 -1 4194560 100 0 0 0 7 3 0 0 20 0 %d 0 %d 1000 10\n",
		state, ppid, threads, start)
}

// TestParseStat: a zombie counts as gone only once its thread group is empty. The leader of a
// multi-threaded process shows state Z as soon as its own thread has exited, while another
// thread may still be closing the process's files, sockets included (DEV-83).
func TestParseStat(t *testing.T) {
	tests := []struct {
		name  string
		b     []byte
		ticks uint64
		ppid  int
		err   error
	}{
		{"running", stat("S", 1, 1, 5000), 5000, 1, nil},
		{"running, several threads", stat("R", 7, 6, 5000), 5000, 7, nil},
		{"zombie, thread group empty", stat("Z", 7, 1, 5000), 0, 0, errGone},
		{"zombie leader, other threads still exiting", stat("Z", 7, 3, 5000), 5000, 7, nil},
		{"dead", stat("X", 7, 3, 5000), 0, 0, errGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticks, ppid, err := parseStat(tt.b)
			if ticks != tt.ticks || ppid != tt.ppid || !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil) {
				t.Errorf("parseStat = %d, %d, %v; want %d, %d, %v", ticks, ppid, err, tt.ticks, tt.ppid, tt.err)
			}
		})
	}
	if _, _, err := parseStat([]byte("4242 (x) S 1 2")); err == nil || errors.Is(err, errGone) {
		t.Errorf("short stat: err %v, want malformed", err)
	}
}

// TestKillLiveListenerSocketClosed: when Kill reports a SIGTERMed Go listener (multi-threaded,
// as every Go program is) exited, its listening socket is gone from a fresh snapshot, so
// `devdash kill`'s re-check never reports a just-killed holder's port as held by an unknown
// owner. Polling every 100 µs instead of 100 ms lands in the exit window on most runs: a
// zombie leader whose other threads have not yet closed the fd table (DEV-83).
func TestKillLiveListenerSocketClosed(t *testing.T) {
	for i := range 20 {
		pid, port := spawnListener(t)
		s := await(t, "the listener", func(s model.Snapshot) bool { return holds(s, pid, port) })
		p, _ := find(s, pid)
		sy := liveOS(t, pid)
		sy.sleep = func(time.Duration) { time.Sleep(100 * time.Microsecond) }

		plan, err := newPlan(s, p.Key(), KillOptions{}, sy)
		if err != nil {
			t.Fatal(err)
		}
		r, err := kill(plan, 5*time.Second, sy)
		if err != nil {
			t.Fatal(err)
		}
		if o := r.Outcomes[0]; !o.Signalled || !o.Exited || o.Err != nil {
			t.Fatalf("run %d: signalled %v, exited %v, err %v", i, o.Signalled, o.Exited, o.Err)
		}
		// /proc/net/tcp first, read at once: the window lasts a few ms at most, less than a
		// snapshot's process scan, which comes before its own read of /proc/net/tcp.
		if listening(t, port) {
			t.Fatalf("run %d: pid %d reported exited, but 127.0.0.1:%d still listens", i, pid, port)
		}
		if s := liveSnapshot(t); holds(s, -1, port) {
			t.Fatalf("run %d: pid %d reported exited, but a snapshot still shows port %d", i, pid, port)
		}
	}
}

// holds reports whether pid (any pid, PID 0 included, when pid is -1) listens on port.
func holds(s model.Snapshot, pid int, port uint16) bool {
	return slices.ContainsFunc(s.Processes, func(p model.Process) bool {
		return (pid < 0 || p.PID == pid) && slices.ContainsFunc(p.Listeners, func(l model.Listener) bool { return l.Port == port })
	})
}

// listening reports whether /proc/net/tcp has a listener (state 0A) on 127.0.0.1:port.
func listening(t *testing.T, port uint16) bool {
	t.Helper()
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), fmt.Sprintf(" 0100007F:%04X 00000000:0000 0A ", port))
}

// spawnListener starts TestKillLiveListenerHelper as the leader of a new process group and
// returns its pid and port. Cleanup sends SIGKILL to that group and then reaps the leader, so
// until then a zombie keeps the pid, and the group id, from being reused.
func spawnListener(t *testing.T) (int, uint16) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestKillLiveListenerHelper$")
	cmd.Env = append(os.Environ(), listenHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if p, ok := strings.CutPrefix(sc.Text(), "listen-helper-port: "); ok {
				line <- p
				return
			}
		}
		close(line)
	}()
	select {
	case l := <-line:
		port, err := strconv.ParseUint(l, 10, 16)
		if err != nil {
			t.Fatalf("helper said %q: %v", l, err)
		}
		return pid, uint16(port)
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not start")
	}
	return 0, 0
}

// TestKillLiveListenerHelper is TestKillLiveListenerSocketClosed's listener: on 127.0.0.1:0, it
// prints its port and sleeps until signalled. Without the environment variable it does nothing.
func TestKillLiveListenerHelper(t *testing.T) {
	if os.Getenv(listenHelperEnv) == "" {
		return
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("listen-helper-port: %d\n", ln.Addr().(*net.TCPAddr).Port)
	time.Sleep(time.Hour)
}
