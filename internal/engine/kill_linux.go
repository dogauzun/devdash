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
// (field 4) from /proc/[pid]/stat. A missing pid, or an unreaped zombie whose threads have
// all exited (see parseStat), counts as gone.
func procStat(pid int) (time.Time, int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return time.Time{}, 0, errGone
	}
	if err != nil {
		return time.Time{}, 0, err
	}
	ticks, ppid, err := parseStat(b)
	switch {
	case errors.Is(err, errGone):
		return time.Time{}, 0, err
	case err != nil:
		return time.Time{}, 0, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	case bootErr != nil:
		return time.Time{}, 0, bootErr
	}
	return time.Unix(bootTime, 0).Add(time.Duration(ticks) * clockTick), ppid, nil
}

// parseStat returns starttime (field 22, in clock ticks) and ppid (field 4) from a
// /proc/[pid]/stat line, or errGone for state X, or Z with num_threads (field 20) 1.
//
// A multi-threaded process's leader shows Z as soon as its own thread has exited, while the
// other threads are still in do_exit: the last of them closes the shared fd table, and the
// process's sockets with it, before it is released and num_threads drops. Counting that Z as
// gone let `devdash kill` re-check the port while it still listened, with no fd left to name
// an owner, so a just-killed Go server's port showed as held by an unknown owner (DEV-83).
func parseStat(b []byte) (ticks uint64, ppid int, err error) {
	// Fields after comm, which may contain spaces and ')'; field n is f[n-3].
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 20 {
		return 0, 0, errors.New("malformed")
	}
	if f[0] == "X" {
		return 0, 0, errGone
	}
	ppid, err1 := strconv.Atoi(f[1])
	threads, err2 := strconv.Atoi(f[17])
	ticks, err3 := strconv.ParseUint(f[19], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return 0, 0, err
	}
	if f[0] == "Z" && threads <= 1 {
		return 0, 0, errGone
	}
	return ticks, ppid, nil
}
