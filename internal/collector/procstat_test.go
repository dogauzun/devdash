package collector

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestProcStatSelf: ProcStat reads the test's own parent and a start time in the last day, and
// a child that has exited and been reaped is ErrGone.
func TestProcStatSelf(t *testing.T) {
	start, ppid, err := ProcStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ppid != os.Getppid() {
		t.Errorf("ppid %d, want %d", ppid, os.Getppid())
	}
	if now := time.Now(); !start.Before(now) || start.Before(now.Add(-24*time.Hour)) {
		t.Errorf("start %v, want in the past day", start)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ProcStat(cmd.Process.Pid); !errors.Is(err, ErrGone) {
		t.Errorf("reaped child: err %v, want ErrGone", err)
	}
}
