package engine

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/model"
)

// TestTagsLiveRemovedWorktree is the phase 6 gate (spec "Milestones"): a server started in a
// linked worktree through a parent that exits, whose worktree is then removed, carries both
// orphaned and cwd_deleted in a snapshot taken with the real collector.
func TestTagsLiveRemovedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// The collectors report resolved paths (/private/var/folders on macOS, not /var/folders).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, wt := filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	git(t, dir, "init", "-q", repo)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	pid, port := spawnOrphan(t, wt)

	// Before the removal: the helper belongs to the worktree's project and is already orphaned.
	s := await(t, "the helper listening", func(s model.Snapshot) bool { return holds(s, pid, port) })
	p, _ := find(s, pid)
	parent, _ := find(s, p.PPID)
	t.Logf("helper %d reparented to %d (%s %q)", pid, p.PPID, parent.Name, parent.Argv)

	// The spec tags orphaned under init (launchd on macOS) or the user's `systemd --user`; any
	// other subreaper that adopts the helper (a CI runner's own, a container's init shim that is
	// not pid 1) is no exited parent devdash can know of, so there only cwd_deleted is promised.
	// On CI that would leave the gate checking one tag, so there it fails instead.
	adopted := p.PPID == 1 || runtime.GOOS == "linux" && parent.Name == "systemd" && parent.UID == p.UID &&
		len(parent.Argv) > 1 && slices.Contains(parent.Argv[1:], "--user")
	orphaned := model.TagOrphaned
	if !adopted {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatalf("helper %d adopted by %d (%s %q), neither init nor systemd --user: the gate cannot check orphaned",
				pid, p.PPID, parent.Name, parent.Argv)
		}
		t.Logf("pid %d is a subreaper other than init or systemd --user: orphaned is not expected", p.PPID)
		orphaned = 0
	}
	if p.ProjectID != wt || p.Tags != orphaned {
		t.Fatalf("before the removal: project %q, tags %v; want %q, %v", p.ProjectID, p.Tags.Names(), wt, orphaned.Names())
	}

	git(t, repo, "worktree", "remove", "--force", wt)
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}

	// The removal is done when git returns, so one snapshot is enough.
	s = liveSnapshot(t)
	p, ok := find(s, pid)
	if !ok || !holds(s, pid, port) {
		t.Fatalf("helper %d gone from the snapshot, or no longer on port %d", pid, port)
	}
	if want := orphaned | model.TagCwdDeleted; p.Tags != want {
		t.Errorf("after the removal: tags %v, want %v (ppid %d, cwd %q, cwd deleted %v, project %q)",
			p.Tags.Names(), want.Names(), p.PPID, p.Cwd, p.CwdDeleted, p.ProjectID)
	}
	if p.Cwd != wt {
		t.Errorf("cwd %q, want the removed %q", p.Cwd, wt)
	}
	t.Logf("after the removal: project %q, tags %v", p.ProjectID, p.Tags.Names())
}

// git runs git in dir with no user or system configuration, so a runner's signing or hook
// settings cannot get in the way.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=devdash", "-c", "user.email=devdash@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// spawnOrphan starts TestKillLiveListenerHelper in dir from a shell that exits right after, so
// the helper is reparented, and returns the helper's pid and port once the OS reports its new
// parent. The shell leads a new process group that the helper joins; Cleanup reaps the shell
// only after signalling, so until then its pid, and with it the group id, cannot be reused: the
// group signal reaches only the helper. The helper is also signalled by pid, and only while its
// start time is the one read when it started.
func spawnOrphan(t *testing.T, dir string) (int, uint16) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `"$0" -test.run '^TestKillLiveListenerHelper$' -test.count=1 & echo "helper-pid: $!"`,
		os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), listenHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leader := cmd.Process.Pid
	if leader <= 1 {
		t.Fatalf("started pid %d", leader)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Both lines come through the shell's stdout, which the helper inherits.
	lines := make(chan string, 2)
	go func() {
		for sc := bufio.NewScanner(out); sc.Scan(); {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var pid, port int
	for pid == 0 || port == 0 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the shell or the helper exited before saying its pid and port")
			}
			if v, ok := strings.CutPrefix(l, "helper-pid: "); ok {
				pid, _ = strconv.Atoi(v)
			} else if v, ok := strings.CutPrefix(l, "listen-helper-port: "); ok {
				port, _ = strconv.Atoi(v)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no pid and port from the helper")
		}
	}
	if g, err := syscall.Getpgid(pid); err != nil || g != leader || port > 65535 {
		t.Fatalf("helper %d: group %d (err %v), port %d", pid, g, err, port)
	}
	start, _, err := collector.ProcStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if st, _, err := collector.ProcStat(pid); err == nil && st.Equal(start) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, ppid, err := collector.ProcStat(pid)
		if err != nil {
			t.Fatal(err)
		}
		if ppid != leader {
			return pid, uint16(port)
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper %d still a child of the shell %d", pid, leader)
		}
	}
}
