//go:build linux

package engine

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

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
