//go:build darwin

package engine

import (
	"time"

	"golang.org/x/sys/unix"
)

const sZomb = 5 // p_stat of a zombie, <sys/proc.h>

// procStat reads pid's start time and parent from kern.proc.pid, the same kinfo_proc fields
// and conversion as the collector. A missing pid returns no record; an unreaped zombie still
// has one, with p_stat SZOMB, and counts as gone.
func procStat(pid int) (time.Time, int, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(kps) == 0 || kps[0].Proc.P_stat == sZomb {
		return time.Time{}, 0, errGone
	}
	return time.Unix(kps[0].Proc.P_starttime.Unix()), int(kps[0].Eproc.Ppid), nil
}
