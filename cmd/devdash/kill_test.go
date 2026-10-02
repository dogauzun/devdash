package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// TestMain doubles as the live test's helper process: with DEVDASH_KILL_HELPER set, the test
// binary listens on 127.0.0.1:0 and never runs a test.
func TestMain(m *testing.M) {
	if mode := os.Getenv("DEVDASH_KILL_HELPER"); mode != "" {
		helper(mode)
	}
	os.Exit(m.Run())
}

// Unit tests use pids above Linux's pid_max (2^22) and macOS's 99999, so no fake pid is ever a
// real process, and killFn is always a recorder.
const fakePID = 5_000_000

var t0 = time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)

func fproc(pid, ppid int, cwd string) collector.Process {
	return collector.Process{PID: pid, PPID: ppid, Name: "node" + strconv.Itoa(pid-fakePID), Argv: []string{"node"}, Cwd: cwd, StartTime: t0}
}

func flisten(pid int, port uint16) collector.Listener {
	return collector.Listener{Proto: "tcp4", Addr: netip.MustParseAddr("127.0.0.1"), Port: port, PID: pid}
}

func fstep(ps []collector.Process, ls ...collector.Listener) collector.Step {
	return collector.Step{Result: collector.Result{TakenAt: t0, Processes: ps, Listeners: ls}}
}

// stubKill replaces killFn with a recorder that gives each planned process the outcome res
// returns, and stdin with a terminal (or not) holding input. stdout is not a terminal; tests
// replace stdoutTerminal after this call to change that.
func stubKill(t *testing.T, tty bool, input string, res func(model.Process) engine.Outcome) *[]engine.Plan {
	t.Helper()
	var plans []engine.Plan
	oldKill, oldIn, oldTTY, oldOut := killFn, stdin, stdinTerminal, stdoutTerminal
	t.Cleanup(func() { killFn, stdin, stdinTerminal, stdoutTerminal = oldKill, oldIn, oldTTY, oldOut })
	stdoutTerminal = func(io.Writer) bool { return false }
	killFn = func(p engine.Plan, _ time.Duration) (engine.Result, error) {
		plans = append(plans, p)
		var r engine.Result
		for _, q := range p.Procs {
			o := res(q)
			o.Process = q
			r.Outcomes = append(r.Outcomes, o)
		}
		return r, nil
	}
	stdin, stdinTerminal = strings.NewReader(input), func() bool { return tty }
	return &plans
}

var (
	exited   = func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true, Exited: true} }
	survives = func(model.Process) engine.Outcome { return engine.Outcome{Signalled: true} }
	denied   = func(model.Process) engine.Outcome { return engine.Outcome{Err: engine.ErrPermission} }
)

func TestKill(t *testing.T) {
	home := testHome(t)
	repo := filepath.Join(home, "code")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, b := fakePID+1, fakePID+2
	inRepo := []collector.Process{fproc(a, 1, repo), fproc(b, a, repo)}
	outside := []collector.Process{fproc(a, 1, "/"), fproc(b, a, "/")}
	held := fstep(inRepo, flisten(a, 3000))
	free := fstep(inRepo)

	tests := []struct {
		name      string
		args      []string
		steps     []collector.Step // before the kill, and the re-check after it
		tty       bool             // stdin is a terminal
		stdoutTTY bool
		input     string
		res       func(model.Process) engine.Outcome
		want      int
		wantPlans [][]int // pids of each Plan passed to killFn
		stdout    []string
		stderr    []string
	}{
		{name: "nothing listens", args: []string{"--yes"}, steps: []collector.Step{free}, res: exited, want: 0,
			stdout: []string{"nothing listens on port 3000\n"}},
		{name: "all exited", args: []string{"--yes"}, steps: []collector.Step{held, free}, res: exited, want: 0, wantPlans: [][]int{{a}},
			stdout: []string{"kill port 3000: process mode, SIGTERM to 1 process:\n5000001  node1  code  3000\n",
				"5000001  node1  signalled, exited\n", "every signalled process exited\n", "port 3000 is free\n"}},
		{name: "tree force", args: []string{"--yes", "--tree", "--force"}, steps: []collector.Step{held, free}, res: exited, want: 0, wantPlans: [][]int{{a, b}},
			stdout: []string{"tree, force mode, SIGKILL to 2 processes:\n5000001  node1  code  3000\n5000002  node2  code  -\n"}},
		{name: "permission denied", args: []string{"--yes"}, steps: []collector.Step{held}, res: denied, want: 3, wantPlans: [][]int{{a}},
			stdout: []string{"not signalled: permission denied, run with sudo"}},
		{name: "survivors", args: []string{"--yes", "--timeout", "1s"}, steps: []collector.Step{held}, res: survives, want: 4, wantPlans: [][]int{{a}},
			stdout: []string{"signalled, still running", "survivors: 5000001 node1 (try --force)\n", "port 3000 is still held by 5000001 node1\n"}},
		{name: "forked child still holds the port", args: []string{"--yes"}, steps: []collector.Step{held, fstep(inRepo[1:], flisten(b, 3000))},
			res: exited, want: 0, wantPlans: [][]int{{a}},
			stdout: []string{"port 3000 is still held by 5000002 node2; a forked child can hold it after its parent exits: try --tree\n"}},
		{name: "two owners, one confirmation", args: nil, tty: true, input: "y\n", steps: []collector.Step{fstep(inRepo, flisten(a, 3000), flisten(b, 3000))},
			res: exited, want: 0, wantPlans: [][]int{{a}, {b}}, stdout: []string{"SIGTERM to 2 processes:\n"}, stderr: []string{"Send SIGTERM to 2 processes? [y/N] "}},
		{name: "owner inside another owner's tree", args: []string{"--yes", "--tree"}, steps: []collector.Step{fstep(inRepo, flisten(a, 3000), flisten(b, 3000))},
			res: exited, want: 0, wantPlans: [][]int{{a, b}}},
		{name: "unknown owner", args: []string{"--yes"}, steps: []collector.Step{fstep(inRepo, flisten(0, 3000))}, res: exited, want: 3,
			stderr: []string{"owner of this port is unknown", "sudo", "nothing was signalled"}},
		{name: "one unknown owner refuses all", args: []string{"--yes"}, steps: []collector.Step{fstep(inRepo, flisten(a, 3000), flisten(0, 3000))}, res: exited, want: 3},
		{name: "init refused", args: []string{"--yes"}, steps: []collector.Step{fstep([]collector.Process{fproc(1, 0, "/")}, flisten(1, 3000))}, res: exited, want: 6,
			stderr: []string{"refused: pid 1 (node-4999999) is init"}},
		{name: "devdash refused", args: []string{"--yes"}, steps: []collector.Step{fstep([]collector.Process{fproc(os.Getpid(), 1, "/")}, flisten(os.Getpid(), 3000))}, res: exited, want: 6,
			stderr: []string{"is devdash itself"}},
		{name: "no terminal", args: nil, steps: []collector.Step{held}, res: exited, want: 2,
			stdout: []string{"5000001  node1"}, stderr: []string{"confirmation needs a terminal; pass --yes"}},
		{name: "confirmed", tty: true, input: "YES\n", steps: []collector.Step{held, free}, res: exited, want: 0, wantPlans: [][]int{{a}}},
		{name: "confirmed, stdout a terminal", tty: true, stdoutTTY: true, input: "y\n", steps: []collector.Step{held, free}, res: exited, want: 0, wantPlans: [][]int{{a}}},
		{name: "declined", tty: true, input: "n\n", steps: []collector.Step{held}, res: exited, want: 6, stderr: []string{"nothing was signalled"}},
		{name: "eof declines", tty: true, input: "", steps: []collector.Step{held}, res: exited, want: 6},
		{name: "outside asks twice", tty: true, input: "y\ny\n", steps: []collector.Step{fstep(outside, flisten(a, 3000)), fstep(outside)}, res: exited, want: 0, wantPlans: [][]int{{a}},
			stdout: []string{"5000001  node1  -  3000\n"}, stderr: []string{"may be a system service. Kill it anyway? [y/N] "}},
		{name: "outside declined the second time", tty: true, input: "y\nn\n", steps: []collector.Step{fstep(outside, flisten(a, 3000))}, res: exited, want: 6},
		{name: "yes skips both questions", args: []string{"--yes"}, steps: []collector.Step{fstep(outside, flisten(a, 3000)), fstep(outside)}, res: exited, want: 0, wantPlans: [][]int{{a}}},
		{name: "snapshot fails", args: []string{"--yes"}, steps: []collector.Step{{Err: errors.New("collector broke")}}, res: exited, want: 5, stderr: []string{"collector broke"}},
		{name: "re-check fails", args: []string{"--yes"}, steps: []collector.Step{held, {Err: errors.New("collector broke")}}, res: exited, want: 0, wantPlans: [][]int{{a}},
			stderr: []string{"cannot check the port again: collector broke"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := stubKill(t, tt.tty, tt.input, tt.res)
			stdoutTerminal = func(io.Writer) bool { return tt.stdoutTTY }
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"kill", "3000"}, tt.args...), &stdout, &stderr, &collector.Fake{Steps: tt.steps})
			if code != tt.want {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.want, stdout.String(), stderr.String())
			}
			var got [][]int
			for _, p := range *plans {
				var ps []int
				for _, q := range p.Procs {
					ps = append(ps, q.PID)
				}
				got = append(got, ps)
			}
			if !slices.EqualFunc(got, tt.wantPlans, slices.Equal) {
				t.Errorf("plans %v, want %v", got, tt.wantPlans)
			}
			for _, s := range tt.stdout {
				if !strings.Contains(stdout.String(), s) {
					t.Errorf("stdout lacks %q:\n%s", s, stdout.String())
				}
			}
			for _, s := range tt.stderr {
				if !strings.Contains(stderr.String(), s) {
					t.Errorf("stderr lacks %q:\n%s", s, stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "[y/N]") && !tt.tty {
				t.Error("asked without a terminal")
			}
			// The plan is on stdout once; when the user is asked and stdout is redirected, it
			// is also on stderr, before the question.
			const header = "kill port 3000:"
			if n := strings.Count(stdout.String(), header); tt.wantPlans != nil && n != 1 {
				t.Errorf("plan on stdout %d times", n)
			}
			if tt.tty {
				onStderr, ask := strings.Index(stderr.String(), header), strings.Index(stderr.String(), "[y/N]")
				if tt.stdoutTTY && onStderr >= 0 || !tt.stdoutTTY && (onStderr < 0 || onStderr > ask) {
					t.Errorf("stdout terminal %v: plan at %d of stderr, question at %d:\n%s", tt.stdoutTTY, onStderr, ask, stderr.String())
				}
			}
		})
	}
}

// writeOnce accepts its first Write and fails every later one.
type writeOnce struct{ n int }

func (w *writeOnce) Write(b []byte) (int, error) {
	if w.n++; w.n > 1 {
		return 0, errors.New("disk full")
	}
	return len(b), nil
}

// TestKillReportFails: once signals were sent, the exit code is their result even when the
// report cannot be written; the write error goes to stderr.
func TestKillReportFails(t *testing.T) {
	for _, tt := range []struct {
		res  func(model.Process) engine.Outcome
		want int
	}{{exited, 0}, {denied, 3}, {survives, 4}} {
		plans := stubKill(t, false, "", tt.res)
		var stdout writeOnce
		var stderr bytes.Buffer
		f := &collector.Fake{Steps: []collector.Step{fstep([]collector.Process{fproc(fakePID+1, 1, "/")}, flisten(fakePID+1, 3000))}}
		if code := run([]string{"kill", "3000", "--yes"}, &stdout, &stderr, f); code != tt.want || len(*plans) != 1 || !strings.Contains(stderr.String(), "disk full") {
			t.Errorf("exit %d, %d plans, stderr %q; want %d, 1 plan and the error", code, len(*plans), stderr.String(), tt.want)
		}
	}
}

// TestKillPlanErrors: engine.Kill refusing one plan (with nothing in it signalled) is reported
// on stderr; the code is the result of the plans that ran, and 6 only when none ran.
func TestKillPlanErrors(t *testing.T) {
	a, b := fakePID+1, fakePID+2
	for _, tt := range []struct {
		name string
		fail []int // 1-based killFn calls that return an error
		res  func(model.Process) engine.Outcome
		want int
	}{
		{"second plan refused after the first exited", []int{2}, exited, 4},
		{"second plan refused after the first was denied", []int{2}, denied, 3},
		{"first plan refused, second survives", []int{1}, survives, 4},
		{"every plan refused", []int{1, 2}, exited, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubKill(t, false, "", tt.res)
			rec, calls := killFn, 0
			killFn = func(p engine.Plan, d time.Duration) (engine.Result, error) {
				calls++
				if slices.Contains(tt.fail, calls) {
					return engine.Result{}, fmt.Errorf("kill: pid %d not allowed", p.Procs[0].PID)
				}
				return rec(p, d)
			}
			procs := []collector.Process{fproc(a, 1, "/"), fproc(b, 1, "/")}
			f := &collector.Fake{Steps: []collector.Step{fstep(procs, flisten(a, 3000), flisten(b, 3000))}}
			var stdout, stderr bytes.Buffer
			code := run([]string{"kill", "3000", "--yes"}, &stdout, &stderr, f)
			if code != tt.want || calls != 2 || !strings.Contains(stderr.String(), "not signalled: kill: pid") {
				t.Errorf("exit %d after %d calls, stderr %q; want %d after 2 and the refusal", code, calls, stderr.String(), tt.want)
			}
		})
	}
}

// TestKillStdoutFails: the plan cannot be shown, so nothing is signalled and devdash fails.
func TestKillStdoutFails(t *testing.T) {
	plans := stubKill(t, false, "", exited)
	var stderr bytes.Buffer
	f := &collector.Fake{Steps: []collector.Step{fstep([]collector.Process{fproc(fakePID+1, 1, "/")}, flisten(fakePID+1, 3000))}}
	if code := run([]string{"kill", "3000", "--yes"}, failWriter{}, &stderr, f); code != 5 || len(*plans) != 0 {
		t.Errorf("exit %d, %d plans; want 5 and none", code, len(*plans))
	}
}

// TestKillRefusesRuntime: Docker Desktop's backend holds a published port and Docker gave no
// container list (unreachable, or --no-docker), so nothing marks the port as a container's. The
// kill is still refused, nothing is signalled, and the exit code is 6 (DEV-51).
func TestKillRefusesRuntime(t *testing.T) {
	for _, args := range [][]string{{"kill", "5432", "--yes"}, {"kill", "5432", "--yes", "--tree", "--force"}, {"kill", "5432", "--yes", "--no-docker"}} {
		plans := stubKill(t, false, "", exited)
		backend := fproc(fakePID+1, 1, "/")
		backend.Name, backend.Argv = "com.docker.backend", []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend"}
		f := &collector.Fake{Steps: []collector.Step{fstep([]collector.Process{backend}, flisten(fakePID+1, 5432))}}
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr, f)
		if code != 6 || len(*plans) != 0 {
			t.Errorf("%v: exit %d, %d plans; want 6 and none", args, code, len(*plans))
		}
		if !strings.Contains(stderr.String(), "com.docker.backend) is part of the container runtime") || !strings.Contains(stderr.String(), "docker ps --filter publish=5432") {
			t.Errorf("%v: stderr %q", args, stderr.String())
		}
	}
}

func TestTargetsContainer(t *testing.T) {
	s := model.Snapshot{Containers: []model.Container{
		{ID: "abc", Name: "shop-db-1", Ports: []model.PortMapping{{HostPort: 5432, ContainerPort: 5432, Proto: "tcp"}}},
	}}
	ts := targets(s, 5432)
	if len(ts) != 1 || ts[0].key != (model.RowKey{ContainerID: "abc"}) {
		t.Fatalf("targets %+v", ts)
	}
	var r *engine.Refusal
	if _, err := engine.NewPlan(s, ts[0].key, engine.KillOptions{}); !errors.As(err, &r) || !strings.Contains(r.Reason, "docker stop shop-db-1") {
		t.Errorf("err %v, want a docker stop refusal", err)
	}
}

// helper is the live test's listener, started as a process-group leader. Modes: "stubborn"
// ignores SIGTERM; "tree" starts a child; "fork" starts a child that inherits the listening
// socket. It prints "port child-pid" and sleeps.
func helper(mode string) {
	if mode == "hold" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if mode == "stubborn" {
		signal.Ignore(syscall.SIGTERM)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	child := 0
	if mode == "tree" || mode == "fork" {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "DEVDASH_KILL_HELPER=hold")
		if mode == "fork" {
			f, err := ln.(*net.TCPListener).File()
			if err != nil {
				panic(err)
			}
			cmd.ExtraFiles = []*os.File{f}
		}
		if err := cmd.Start(); err != nil {
			panic(err)
		}
		child = cmd.Process.Pid
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port, child)
	time.Sleep(time.Hour)
	os.Exit(0)
}

// spawnHelper starts helper(mode) as the leader of a new process group and returns its pid,
// its child's pid (0 if none) and its port. Cleanup sends SIGKILL to that group only, then
// reaps the leader; until then its pid, and so the group id, cannot be reused.
func spawnHelper(t *testing.T, mode string) (leader, child int, port string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "DEVDASH_KILL_HELPER="+mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leader = cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Scan()
		line <- sc.Text()
	}()
	select {
	case l := <-line:
		if _, err := fmt.Sscan(l, &port, &child); err != nil {
			t.Fatalf("helper said %q: %v", l, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not start")
	}
	return leader, child, port
}

// guardKill lets killFn signal only processes in the group led by leader.
func guardKill(t *testing.T, leader int) {
	t.Helper()
	old := killFn
	t.Cleanup(func() { killFn = old })
	killFn = func(p engine.Plan, d time.Duration) (engine.Result, error) {
		for _, q := range p.Procs {
			if g, err := syscall.Getpgid(q.PID); err != nil || g != leader || p.Group != 0 && p.Group != leader {
				t.Errorf("refusing plan with pid %d (group %d, err %v) outside the test's group %d", q.PID, g, err, leader)
				return engine.Result{}, errors.New("not the test's group")
			}
		}
		return engine.Kill(p, d)
	}
}

// running reports whether pid is in a fresh snapshot (zombies are not).
func running(t *testing.T, pid int) bool {
	t.Helper()
	s, err := engine.Snapshot(context.Background(), engine.Options{Collector: collector.New(), Resolver: model.NewResolver("", nil)})
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(s.Processes, func(p model.Process) bool { return p.PID == pid })
}

func killLive(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"kill"}, args...), &stdout, &stderr, collector.New())
	t.Logf("devdash kill %v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
	return code, stdout.String()
}

// TestKillLive kills processes this test started, found by a port they opened on 127.0.0.1:0.
func TestKillLive(t *testing.T) {
	if os.Geteuid() == 0 && runtime.GOOS == "darwin" {
		t.Skip("no kill tests as root on macOS")
	}
	t.Run("tree", func(t *testing.T) {
		leader, child, port := spawnHelper(t, "tree")
		guardKill(t, leader)
		if code, out := killLive(t, port, "--tree", "--yes"); code != 0 || !strings.Contains(out, "port "+port+" is free") {
			t.Errorf("exit %d, want 0 and a free port", code)
		}
		for _, pid := range []int{leader, child} {
			if running(t, pid) {
				t.Errorf("pid %d still running", pid)
			}
		}
	})
	t.Run("survivor, then force", func(t *testing.T) {
		leader, _, port := spawnHelper(t, "stubborn")
		guardKill(t, leader)
		if code, out := killLive(t, port, "--yes", "--timeout", "300ms"); code != 4 || !strings.Contains(out, fmt.Sprintf("survivors: %d ", leader)) {
			t.Errorf("exit %d, want 4 and the survivor", code)
		}
		if code, _ := killLive(t, port, "--yes", "--force"); code != 0 || running(t, leader) {
			t.Errorf("force: exit %d, running %v; want 0 and gone", code, running(t, leader))
		}
	})
	t.Run("forked child keeps the port", func(t *testing.T) {
		leader, child, port := spawnHelper(t, "fork")
		guardKill(t, leader)
		// The shared socket is credited to the lower pid (DEV-45): usually the leader, the
		// child when pids wrapped between them. Process mode kills that one only.
		first, second := min(leader, child), max(leader, child)
		want := fmt.Sprintf("port %s is still held by %d ", port, second)
		if code, out := killLive(t, port, "--yes"); code != 0 || !strings.Contains(out, fmt.Sprintf("\n%d ", first)) ||
			!strings.Contains(out, want) || !strings.Contains(out, "try --tree") {
			t.Errorf("exit %d, want 0, pid %d signalled and %q with the --tree hint", code, first, want)
		}
		if code, out := killLive(t, port, "--yes"); code != 0 || !strings.Contains(out, "port "+port+" is free") || running(t, first) || running(t, second) {
			t.Errorf("second kill: exit %d, want 0, a free port and both gone", code)
		}
	})
}
