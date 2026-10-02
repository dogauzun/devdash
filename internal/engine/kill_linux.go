//go:build linux

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const clockTick = 10 * time.Millisecond // USER_HZ, as in the collector

// bootTime is read once at startup, like the collector's, because the kernel shifts btime when
// the wall clock is stepped and a start time must compare equal to the snapshot's.
//
// ponytail: two separate reads; a clock step between the collector's and this one makes every
// kill fail with ErrStartTime (safe, nothing signalled). Share one btime if that is ever seen.
var bootTime, bootErr = readBootTime()

func readBootTime() (int64, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			return strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
		}
	}
	return 0, errors.New("no btime in /proc/stat")
}

// procStat reads pid's start time (field 22, converted as the collector does) and parent
// (field 4) from /proc/[pid]/stat. A missing pid, or state Z or X (an unreaped zombie),
// counts as gone.
func procStat(pid int) (time.Time, int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return time.Time{}, 0, errGone
	}
	if err != nil {
		return time.Time{}, 0, err
	}
	// Fields after comm, which may contain spaces and ')'; field n is f[n-3].
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 20 {
		return time.Time{}, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	if f[0] == "Z" || f[0] == "X" {
		return time.Time{}, 0, errGone
	}
	ppid, err1 := strconv.Atoi(f[1])
	ticks, err2 := strconv.ParseUint(f[19], 10, 64)
	if err := errors.Join(err1, err2, bootErr); err != nil {
		return time.Time{}, 0, err
	}
	return time.Unix(bootTime, 0).Add(time.Duration(ticks) * clockTick), ppid, nil
}
