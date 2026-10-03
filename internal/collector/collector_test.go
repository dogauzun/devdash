package collector

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dogauzun/devdash/internal/model"
)

// TestArgvWanted: with InProject, argv is read for a listener's owner, for the processes
// InProject marks, for those whose kernel name may have been cut or may be a runtime's, and
// for a process named systemd, and for nobody else; a short answer marks the rest false
// (DEV-92, DEV-123).
func TestArgvWanted(t *testing.T) {
	procs := []Process{{PID: 10}, {PID: 11}, {PID: 12}, {PID: 13},
		// Docker Desktop's backend as Linux comm and macOS p_comm cut it: argv[0] is what tells
		// model.IsContainerRuntime, so kill refuses it by name (PR #64 review).
		{PID: 14, Name: "com.docker.back"}, {PID: 15, Name: "com.docker.backe"},
		{PID: 16, Name: "fourteen-chars"}, // shorter than any cut: the whole name
		// passt's pasta re-execs as pasta.avx2 and keeps argv[0] pasta, the name that makes it a
		// runtime process (PR #64 re-review); a short cut prefix of a runtime name; a short
		// name that is neither.
		{PID: 17, Name: "pasta.avx2"}, {PID: 18, Name: "com.docker.vpn"}, {PID: 19, Name: "sleep"},
		// A systemd --user manager is known as a subreaper by its argv (model.Tag, DEV-117);
		// only the exact name counts.
		{PID: 20, Name: "systemd"}, {PID: 21, Name: "systemd-logind"},
	}
	ls := []Listener{{Port: 22}, {Port: 3000, PID: 11}} // PID 0: owner unknown
	var got []Process
	o := Options{InProject: func(ps []Process) []bool {
		got = ps
		return []bool{false, false, true} // 13 and on not answered
	}}
	want := []bool{false, true, true, false, true, true, false, true, true, false, true, false}
	if w := argvWanted(o, procs, ls); !slices.Equal(w, want) {
		t.Errorf("argvWanted = %v, want %v", w, want)
	}
	if len(got) != len(procs) {
		t.Errorf("InProject got %d processes, want %d", len(got), len(procs))
	}
}

// TestCollectLimitedArgvLive: with Options.InProject on the live OS, a child outside any
// project and without a listener gets no argv (unknown), a child InProject marks keeps its
// argv, and so does the test process, which holds a listener (DEV-92).
func TestCollectLimitedArgvLive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	project, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is a symlink, cwd is not
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in, out := startCat(t, project), startCat(t, outside)

	res, err := New().Collect(context.Background(), Options{InProject: func(ps []Process) []bool {
		marks := make([]bool, len(ps))
		for i, p := range ps {
			marks[i] = p.Cwd == project
		}
		return marks
	}})
	if err != nil {
		t.Fatal(err)
	}
	find := func(pid int) Process {
		t.Helper()
		i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == pid })
		if i < 0 {
			t.Fatalf("pid %d not in %d processes", pid, len(res.Processes))
		}
		return res.Processes[i]
	}
	if self := find(os.Getpid()); !slices.Equal(self.Argv, os.Args) || self.Unknown != 0 {
		t.Errorf("self (listener) argv %q unknown %v; want %q, none", self.Argv, self.Unknown, os.Args)
	}
	if p := find(in); !slices.Equal(p.Argv, []string{"cat"}) || p.Unknown != 0 {
		t.Errorf("child in a project: argv %q unknown %v; want [cat], none", p.Argv, p.Unknown)
	}
	if p := find(out); p.Argv != nil || p.Unknown != model.FieldArgv || p.Cwd != outside {
		t.Errorf("child outside: argv %q unknown %v cwd %q; want nil, argv only, %q", p.Argv, p.Unknown, p.Cwd, outside)
	}
}

// TestCollectCwdDeleted: a child whose working directory is removed while it runs keeps its
// old path as Cwd and has CwdDeleted; a child in a live directory and the test process do not
// (DEV-116).
func TestCollectCwdDeleted(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is a symlink, cwd is not
	if err != nil {
		t.Fatal(err)
	}
	gone, live := filepath.Join(base, "gone"), filepath.Join(base, "live")
	for _, d := range []string{gone, live} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	orphan, kept := startCat(t, gone), startCat(t, live)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	res, err := New().Collect(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		pid     int
		cwd     string
		deleted bool
	}{{orphan, gone, true}, {kept, live, false}, {os.Getpid(), kernelWd(t), false}} {
		i := slices.IndexFunc(res.Processes, func(p Process) bool { return p.PID == want.pid })
		if i < 0 {
			t.Errorf("pid %d not in %d processes", want.pid, len(res.Processes))
		} else if p := res.Processes[i]; p.Cwd != want.cwd || p.CwdDeleted != want.deleted {
			t.Errorf("pid %d: cwd %q deleted %v; want %q, %v", p.PID, p.Cwd, p.CwdDeleted, want.cwd, want.deleted)
		}
	}
}

// startCat starts cat in dir, waits until it runs its own code (so its argv is published,
// DEV-47) and kills it when the test ends. It returns the pid.
func startCat(t *testing.T, dir string) int {
	t.Helper()
	child := exec.Command("cat")
	child.Dir = dir
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
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if _, err := stdin.Write([]byte("ready\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child echo: %q, %v", line, err)
	}
	return child.Process.Pid
}
