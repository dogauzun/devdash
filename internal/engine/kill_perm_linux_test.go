package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/dogauzun/devdash/internal/model"
)

// The permission test needs three uids: root runs the test and owns cleanup, the target runs
// as targetUID, and the helper that calls Kill runs as helperUID, so its kill(2) gets EPERM.
// Neither unprivileged uid needs to exist in /etc/passwd.
const (
	targetUID = 65534 // nobody
	helperUID = 65533

	permTargetEnv = "DEVDASH_KILL_PERM_TARGET" // "pid:startnanos" of the target, helper mode only
	permResult    = "kill-perm-result: "       // prefix of the helper's JSON line on stdout
)

// permReport is what the helper prints for the parent to check.
type permReport struct {
	UID      int
	Plan     []int
	ExitCode int
	Outcomes []permOutcome
}

type permOutcome struct {
	PID                      int
	Signalled, Exited, EPERM bool
	Err                      string
}

// TestKillLivePermission runs the real NewPlan and Kill, as an unprivileged helper process,
// against a `sleep` the test started as another uid: kill(2) fails with EPERM, the outcome is
// ErrPermission and the exit code is 3. Nothing is signalled but by root's cleanup of its own
// child, and the helper's uid cannot signal any process but itself.
func TestKillLivePermission(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a target and a helper as two other uids (CI runs it with sudo)")
	}

	target := exec.Command("sleep", "300")
	target.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: targetUID, Gid: targetUID}}
	if err := target.Start(); err != nil {
		t.Fatal(err)
	}
	pid := target.Process.Pid
	t.Cleanup(func() {
		// Not yet reaped, so pid still names the test's own child.
		_ = target.Process.Kill()
		_ = target.Wait()
	})
	s := await(t, "the target's exec of sleep", func(s model.Snapshot) bool {
		p, ok := find(s, pid)
		return ok && p.Name == "sleep" && p.UID == targetUID
	})
	tp, _ := find(s, pid)

	// The `go test` binary lives in a root-only build directory: copy it where the helper's
	// uid can exec it. Running it directly needs no Go cache.
	dir, err := os.MkdirTemp("", "devdash-kill-perm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "engine.test")
	if err := copySelf(bin); err != nil {
		t.Fatal(err)
	}
	// Explicit modes: MkdirTemp makes 0700, and the umask may strip more.
	if err := errors.Join(os.Chmod(dir, 0o755), os.Chmod(bin, 0o755)); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(bin, "-test.run=^TestKillLivePermissionHelper$", "-test.count=1")
	helper.Dir = dir
	helper.Env = append(os.Environ(), fmt.Sprintf("%s=%d:%d", permTargetEnv, pid, tp.StartTime.UnixNano()))
	helper.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: helperUID, Gid: helperUID}}
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("helper (uid %d, %s; every directory above it must be searchable by that uid): %v\n%s", helperUID, bin, err, out)
	}
	var r permReport
	for line := range strings.Lines(string(out)) {
		if js, ok := strings.CutPrefix(line, permResult); ok {
			if err := json.Unmarshal([]byte(js), &r); err != nil {
				t.Fatal(err)
			}
		}
	}

	if r.UID != helperUID {
		t.Fatalf("helper ran as uid %d, want %d; output:\n%s", r.UID, helperUID, out)
	}
	if len(r.Plan) != 1 || r.Plan[0] != pid {
		t.Fatalf("helper planned %v, want [%d]; output:\n%s", r.Plan, pid, out)
	}
	if len(r.Outcomes) != 1 {
		t.Fatalf("outcomes %+v, want one", r.Outcomes)
	}
	if o := r.Outcomes[0]; o.PID != pid || !o.EPERM || o.Signalled || o.Exited || o.Err != ErrPermission.Error() {
		t.Errorf("outcome %+v, want pid %d not signalled, not exited, err %q", o, pid, ErrPermission)
	}
	if r.ExitCode != 3 {
		t.Errorf("exit code %d, want 3", r.ExitCode)
	}
	if !alive(t, pid) {
		t.Errorf("target %d is gone", pid)
	}
}

// TestKillLivePermissionHelper is TestKillLivePermission's helper process; without the
// environment variable it does nothing.
func TestKillLivePermissionHelper(t *testing.T) {
	v := os.Getenv(permTargetEnv)
	if v == "" {
		return
	}
	// Belt and braces: Kill below uses the real kill(2), so only ever as the unprivileged uid.
	if os.Getuid() != helperUID || os.Geteuid() != helperUID {
		t.Fatalf("helper runs as uid %d euid %d, want %d", os.Getuid(), os.Geteuid(), helperUID)
	}
	ps, ns, _ := strings.Cut(v, ":")
	pid, err1 := strconv.Atoi(ps)
	start, err2 := strconv.ParseInt(ns, 10, 64)
	if err := errors.Join(err1, err2); err != nil || pid <= 1 {
		t.Fatalf("%s=%q: %v", permTargetEnv, v, err)
	}

	p, err := NewPlan(liveSnapshot(t), model.RowKey{PID: pid, StartTime: start}, KillOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pids(p.Procs); len(got) != 1 || got[0] != pid || p.Group != 0 {
		t.Fatalf("plan %v group %d, want [%d] and no group", got, p.Group, pid)
	}
	res, err := Kill(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := permReport{UID: os.Getuid(), Plan: pids(p.Procs), ExitCode: res.ExitCode()}
	for _, o := range res.Outcomes {
		po := permOutcome{PID: o.Process.PID, Signalled: o.Signalled, Exited: o.Exited, EPERM: errors.Is(o.Err, ErrPermission)}
		if o.Err != nil {
			po.Err = o.Err.Error()
		}
		r.Outcomes = append(r.Outcomes, po)
	}
	js, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s%s\n", permResult, js)
}

// copySelf copies the running test binary to dst.
func copySelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
