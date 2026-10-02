//go:build darwin

package collector

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestProbeOtherUID logs the raw errno of each source for other users' processes, to re-check
// the DEV-10 findings on a new macOS version: DEVDASH_PROBE=1 go test -run Probe -v.
// TestCollectOtherUsers holds the assertions.
func TestProbeOtherUID(t *testing.T) {
	if os.Getenv("DEVDASH_PROBE") == "" {
		t.Skip("set DEVDASH_PROBE=1")
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
	for _, kp := range kps {
		pid, uid := int(kp.Proc.P_pid), int(kp.Eproc.Ucred.Uid)
		if uid == os.Geteuid() || pid == 0 {
			continue
		}
		_, e1 := lib.procArgs2(pid, argBuf)
		_, e2 := lib.pidinfo(pid, procPidTaskInfo, buf[:sizeofProcTaskInfo])
		_, e3 := lib.pidinfo(pid, procPidVnodePathInfo, buf[:sizeofVnodePathInfo])
		_, e4 := lib.pidinfo(pid, procPidListFDs, buf)
		counts[fmt.Sprintf("procargs2=%v taskinfo=%v vnodepathinfo=%v listfds=%v", e1, e2, e3, e4)]++
		if pid == 1 {
			t.Logf("launchd: procargs2=%v taskinfo=%v vnodepathinfo=%v listfds=%v", e1, e2, e3, e4)
		}
	}
	b, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	_, pcbs := decodePCBList(b)
	t.Logf("other-uid processes by outcome: %v; pcblist_n: %d bytes, %d PCBs, err %v", counts, len(b), pcbs, err)
}
