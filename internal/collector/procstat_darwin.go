//go:build darwin

package collector

import (
	"time"

	"golang.org/x/sys/unix"
)

const sZomb = 5 // p_stat of a zombie, <sys/proc.h>

// ProcStat reads pid's start time and parent from kern.proc.pid, the same kinfo_proc fields
// and conversion as the collector's table. A missing pid returns no record; an unreaped zombie
// still has one, with p_stat SZOMB, and is ErrGone too.
func ProcStat(pid int) (start time.Time, ppid int, err error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(kps) == 0 || kps[0].Proc.P_stat == sZomb {
		return time.Time{}, 0, ErrGone
	}
	return startTime(&kps[0]), int(kps[0].Eproc.Ppid), nil
}

// startTime is kp's start time: the one conversion that the table read and ProcStat share, so
// kill's pid-reuse check compares like with like.
func startTime(kp *unix.KinfoProc) time.Time { return time.Unix(kp.Proc.P_starttime.Unix()) }
