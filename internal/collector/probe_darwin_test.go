//go:build darwin

package collector

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestProbeOtherUID logs what an unprivileged, ad-hoc-signed binary can read about processes
// of other users (DEV-10). It asserts nothing about the answers; they are recorded in DECISIONS.md.
func TestProbeOtherUID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as a normal user")
	}
	lib, err := loadLibSystem()
	if err != nil {
		t.Fatal(err)
	}
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		t.Fatal(err)
	}
	argBuf := make([]byte, 1<<20)
	buf := make([]byte, 64*1024)
	counts := map[string]int{}
	others := 0
	for _, kp := range kps {
		pid, uid := int(kp.Proc.P_pid), int(kp.Eproc.Ucred.Uid)
		if uid == os.Geteuid() || pid == 0 {
			continue
		}
		others++
		_, e1 := lib.procArgs2(pid, argBuf)
		_, e2 := lib.pidinfo(pid, procPidTaskInfo, buf[:sizeofProcTaskInfo])
		_, e3 := lib.pidinfo(pid, procPidVnodePathInfo, buf[:sizeofVnodePathInfo])
		_, e4 := lib.pidinfo(pid, procPidListFDs, buf)
		for name, e := range map[string]error{"procargs2": e1, "taskinfo": e2, "vnodepathinfo": e3, "listfds": e4} {
			counts[fmt.Sprintf("%s=%v", name, e)]++
		}
		if pid == 1 || len(counts) > 0 && others <= 3 {
			t.Logf("pid %d uid %d %s: procargs2=%v taskinfo=%v vnodepathinfo=%v listfds=%v",
				pid, uid, cstring(kp.Proc.P_comm[:]), e1, e2, e3, e4)
		}
	}
	t.Logf("%d other-uid processes: %v", others, counts)
}

func TestProbePCB(t *testing.T) {
	for _, name := range []string{"net.inet.tcp.pcblist_n", "net.inet.tcp.pcblist", "net.inet.tcp.pcblist64"} {
		b, err := unix.SysctlRaw(name)
		t.Logf("%s: len=%d err=%v", name, len(b), err)
		if len(b) >= sizeofXgen {
			t.Logf("  xig_count=%d", le.Uint32(b[4:]))
		}
	}
}
