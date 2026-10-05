//go:build linux

package collector

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// bootTime is read once at startup, like each collector's, because the kernel shifts btime when
// the wall clock is stepped and a start time must compare equal to the snapshot's.
//
// ponytail: two separate reads; a clock step between the collector's and this one makes every
// kill fail with ErrStartTime (safe, nothing signalled). Share one btime if that is ever seen.
var bootTime, bootErr = readBtime("/proc")

// ProcStat reads pid's start time, as the collector's table has it, and parent from
// /proc/[pid]/stat. A missing pid, or a zombie that procStat.gone counts as exited, is ErrGone.
func ProcStat(pid int) (start time.Time, ppid int, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return time.Time{}, 0, ErrGone
	}
	if err != nil {
		return time.Time{}, 0, err
	}
	s, err := parseStat(b)
	switch {
	case err != nil:
		return time.Time{}, 0, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	case s.gone():
		return time.Time{}, 0, ErrGone
	case bootErr != nil:
		return time.Time{}, 0, bootErr
	}
	return s.start(bootTime), s.ppid, nil
}

// start is the process's start time, given the boot time in Unix seconds: the one conversion
// that the table read and ProcStat share, so kill's pid-reuse check compares like with like.
func (s procStat) start(btime int64) time.Time {
	return time.Unix(btime, 0).Add(time.Duration(s.starttime) * clockTick)
}
