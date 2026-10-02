package engine

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// Live tests signal only processes in a process group the test created: every child runs
// under a shell started with Setpgid, and liveOS refuses any kill(2) outside that group.

type child struct {
	pid   int         // the group leader the test started
	lines chan string // its stdout, line by line; closed at EOF
}

// spawn starts `sh -c script` as the leader of a new process group. Cleanup sends SIGKILL to
// that group and then reaps the leader: until it is reaped its pid cannot be reused, so the
// group signal can only reach the test's own processes.
func spawn(t *testing.T, script string) child {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := child{pid: cmd.Process.Pid, lines: make(chan string, 100)}
	if c.pid <= 1 {
		t.Fatalf("started pid %d", c.pid)
	}
	go func() {
		for sc := bufio.NewScanner(out); sc.Scan(); {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-c.pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return c
}

// liveOS is the real OS seam with kill(2) restricted to the group led by leader.
func liveOS(t *testing.T, leader int) osys {
	sy := realOS
	sy.kill = func(pid int, sig syscall.Signal) error {
		ok := leader > 1 && pid == -leader
		if pid > 1 {
			g, err := syscall.Getpgid(pid)
			ok = leader > 1 && err == nil && g == leader
		}
		if !ok {
			t.Errorf("refusing to send %v to %d: not in the test's group %d", sig, pid, leader)
			return syscall.EINVAL
		}
		return syscall.Kill(pid, sig)
	}
	return sy
}

func liveSnapshot(t *testing.T) model.Snapshot {
	t.Helper()
	s, err := Snapshot(context.Background(), Options{Collector: collector.New(), Resolver: model.NewResolver("", nil)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// await takes snapshots until ok accepts one, for up to 10 s.
func await(t *testing.T, what string, ok func(model.Snapshot) bool) model.Snapshot {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s := liveSnapshot(t); ok(s) {
			return s
		}
	}
	t.Fatalf("timed out waiting for %s", what)
	return model.Snapshot{}
}

func find(s model.Snapshot, pid int) (model.Process, bool) {
	i := slices.IndexFunc(s.Processes, func(p model.Process) bool { return p.PID == pid })
	if i < 0 {
		return model.Process{}, false
	}
	return s.Processes[i], true
}

// sleeps returns the pids of the `sleep` children of ppid that have finished exec.
func sleeps(s model.Snapshot, ppid int) []int {
	var ps []int
	for _, p := range s.Processes {
		if p.PPID == ppid && p.Name == "sleep" {
			ps = append(ps, p.PID)
		}
	}
	return ps
}

func alive(t *testing.T, pid int) bool {
	t.Helper()
	_, err := procStart(pid)
	if err != nil && !errors.Is(err, errGone) {
		t.Fatal(err)
	}
	return err == nil
}

func TestKillLive(t *testing.T) {
	tests := []struct {
		name  string
		opt   KillOptions
		group bool // the whole tree, through the group signal
	}{
		{"process", KillOptions{}, false},
		{"process force", KillOptions{Force: true}, false},
		{"tree", KillOptions{Tree: true}, true},
		{"tree force", KillOptions{Tree: true, Force: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := spawn(t, "sleep 300 & sleep 300 & wait")
			s := await(t, "two sleeps", func(s model.Snapshot) bool { return len(sleeps(s, c.pid)) == 2 })
			kids := sleeps(s, c.pid)
			sh, _ := find(s, c.pid)
			sy := liveOS(t, c.pid)

			p, err := newPlan(s, sh.Key(), tt.opt, sy)
			if err != nil {
				t.Fatal(err)
			}
			want := []int{c.pid}
			if tt.opt.Tree {
				want = append(want, kids...)
			}
			if got := pids(p.Procs); !slices.Equal(got, want) {
				t.Fatalf("plan %v, want %v", got, want)
			}
			if tt.group != (p.Group == c.pid) {
				t.Fatalf("plan group %d", p.Group)
			}

			r, err := kill(p, 0, sy)
			if err != nil {
				t.Fatal(err)
			}
			if r.ExitCode() != 0 || len(r.Survivors()) != 0 {
				t.Errorf("exit %d, survivors %v", r.ExitCode(), pids(r.Survivors()))
			}
			for _, o := range r.Outcomes {
				if !o.Signalled || !o.Exited || o.Err != nil {
					t.Errorf("pid %d: signalled %v, exited %v, err %v", o.Process.PID, o.Signalled, o.Exited, o.Err)
				}
			}
			if alive(t, c.pid) {
				t.Errorf("shell %d still running", c.pid)
			}
			for _, k := range kids {
				if alive(t, k) != !tt.opt.Tree {
					t.Errorf("sleep %d alive %v after %s", k, alive(t, k), tt.name)
				}
			}
		})
	}
}

func TestKillLiveSurvivorsThenForce(t *testing.T) {
	c := spawn(t, `trap "" TERM; sleep 300`) // sleep inherits the ignored SIGTERM
	s := await(t, "sleep", func(s model.Snapshot) bool { return len(sleeps(s, c.pid)) == 1 })
	sh, _ := find(s, c.pid)
	sy := liveOS(t, c.pid)

	p, err := newPlan(s, sh.Key(), KillOptions{Tree: true}, sy)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, err := kill(p, 300*time.Millisecond, sy)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pids(r.Survivors()), []int{c.pid, sleeps(s, c.pid)[0]}; !slices.Equal(got, want) {
		t.Errorf("survivors %v, want %v", got, want)
	}
	if r.ExitCode() != 4 || time.Since(start) < 300*time.Millisecond {
		t.Errorf("exit %d after %v", r.ExitCode(), time.Since(start))
	}

	p, err = newPlan(liveSnapshot(t), sh.Key(), KillOptions{Tree: true, Force: true}, sy)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = kill(p, 0, sy); err != nil || r.ExitCode() != 0 {
		t.Errorf("force: exit %d, err %v, survivors %v", r.ExitCode(), err, pids(r.Survivors()))
	}
}

// A supervisor (inside the test's group, but not its leader, so the tree is signalled pid by
// pid) restarts its child whenever it exits and prints each child's pid.
func TestKillLiveSupervisorParentFirst(t *testing.T) {
	c := spawn(t, `sh -c 'while :; do sleep 300 & echo child $!; wait $!; done' & echo super $!; wait`)
	var super, first int
	for super == 0 || first == 0 {
		select {
		case l := <-c.lines:
			f := strings.Fields(l)
			n, _ := strconv.Atoi(f[1])
			if f[0] == "super" {
				super = n
			} else {
				first = n
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no pids from the supervisor")
		}
	}
	s := await(t, "first child", func(s model.Snapshot) bool { return slices.Contains(sleeps(s, super), first) })
	sy := liveOS(t, c.pid)

	// Killing only the child shows the supervisor really respawns it.
	kid, _ := find(s, first)
	p, err := newPlan(s, kid.Key(), KillOptions{}, sy)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := kill(p, 0, sy); err != nil || r.ExitCode() != 0 {
		t.Fatalf("child: exit %d, err %v", r.ExitCode(), err)
	}
	var second int
	select {
	case l := <-c.lines:
		second, _ = strconv.Atoi(strings.TrimPrefix(l, "child "))
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not respawn")
	}

	// The tree, parent first: nothing is respawned.
	s = await(t, "second child", func(s model.Snapshot) bool { return slices.Contains(sleeps(s, super), second) })
	sv, _ := find(s, super)
	p, err = newPlan(s, sv.Key(), KillOptions{Tree: true}, sy)
	if err != nil {
		t.Fatal(err)
	}
	if got := pids(p.Procs); p.Group != 0 || !slices.Equal(got, []int{super, second}) {
		t.Fatalf("plan %v group %d, want [%d %d] and no group", got, p.Group, super, second)
	}
	if r, err := kill(p, 0, sy); err != nil || r.ExitCode() != 0 {
		t.Fatalf("tree: exit %d, err %v, survivors %v", r.ExitCode(), err, pids(r.Survivors()))
	}
	// stdout closes once the outer shell, the supervisor and every child have exited.
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				return
			}
			t.Errorf("respawned after the tree kill: %q", l)
		case <-time.After(10 * time.Second):
			t.Fatal("group still holds stdout: a child survived")
		}
	}
}

func TestKillLiveRefusesDevdash(t *testing.T) {
	// Kill itself refuses every ancestor read fresh from the OS; liveOS(t, 0) could not
	// signal anything even if it did not.
	chain := realOS.chain()
	want := []int{os.Getpid()}
	if os.Getppid() > 1 {
		want = append(want, os.Getppid())
	}
	if len(chain) < len(want) || !slices.Equal(chain[:len(want)], want) {
		t.Fatalf("chain %v, want it to start with %v", chain, want)
	}
	t.Logf("devdash's ancestors from the OS: %v", chain[1:])
	for _, pid := range chain[1:] {
		start, err := procStart(pid)
		if err != nil {
			continue
		}
		p := Plan{Procs: []model.Process{{PID: pid, StartTime: start}}, Signal: syscall.SIGTERM}
		if _, err := kill(p, time.Millisecond, liveOS(t, 0)); err == nil {
			t.Errorf("Kill accepted ancestor %d", pid)
		}
	}

	s := liveSnapshot(t)
	for _, pid := range []int{os.Getpid(), os.Getppid()} {
		p, ok := find(s, pid)
		if !ok {
			continue // the parent may be another user's and unreadable
		}
		for _, o := range []KillOptions{{}, {Tree: true}} {
			var r *Refusal
			if _, err := NewPlan(s, p.Key(), o); !errors.As(err, &r) {
				t.Errorf("pid %d %+v: err %v, want a refusal", pid, o, err)
			}
		}
	}
}
