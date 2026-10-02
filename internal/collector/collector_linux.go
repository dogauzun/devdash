//go:build linux

package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dogauzun/devdash/internal/model"
)

// New returns the Linux collector, reading from /proc.
func New() Collector { return newLinux("/proc") }

func newLinux(root string) *linuxCollector {
	c := &linuxCollector{root: root}
	// btime is read once: the kernel shifts it when the wall clock is stepped,
	// which would move every StartTime and break (pid, start time) identity.
	c.btime = sync.OnceValues(func() (int64, error) {
		b, err := os.ReadFile(root + "/stat")
		if err != nil {
			return 0, err
		}
		bt, err := parseBtime(b)
		if err != nil {
			return 0, fmt.Errorf("%s/stat: %w", root, err)
		}
		return bt, nil
	})
	return c
}

type linuxCollector struct {
	root  string // proc root, "/proc" outside tests
	btime func() (int64, error)
}

const clockTick = 10 * time.Millisecond // USER_HZ = 100 on every Linux ABI

func (c *linuxCollector) Collect(ctx context.Context) (Result, error) {
	res := Result{TakenAt: time.Now(), Host: host(), Timings: model.Timing{}}

	procs, err := c.processes(ctx, res.Timings)
	if err != nil {
		return Result{}, err
	}

	t := time.Now()
	res.Listeners, err = c.listeners(ctx, procs, os.Getuid())
	if err != nil {
		return Result{}, err
	}
	res.Timings["listeners"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res.Processes = procs

	if mounts, err := os.ReadFile(c.root + "/mounts"); err == nil {
		if opt := hidepidOption(mounts, c.root); opt != "" {
			res.Warnings = append(res.Warnings, model.Warning{Code: "proc_hidepid", Count: 1, Hint: fmt.Sprintf(
				"%s is mounted with %s: other users' processes are hidden and their listeners have no owner", c.root, opt)})
		}
	}
	unknown := 0
	for _, p := range procs {
		if p.Unknown != 0 {
			unknown++
		}
	}
	if unknown > 0 {
		res.Warnings = append(res.Warnings, model.Warning{Code: "process_fields_unreadable", Count: unknown,
			Hint: "processes of other users have unreadable fields; run with sudo to see them"})
	}
	return res, nil
}

// processes reads every user-space process in one pass per pid, so a pid
// reused between reads cannot mix two processes into one row. Kernel threads,
// zombies and processes that exit mid-read are skipped. It adds the
// "proctable" and "argv_cwd" timings.
func (c *linuxCollector) processes(ctx context.Context, timings map[string]time.Duration) ([]Process, error) {
	btime, err := c.btime()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	names, err := readDirNames(c.root)
	if err != nil {
		return nil, err
	}
	tTable, tArgv := time.Since(start), time.Duration(0)
	pageSize := uint64(os.Getpagesize())

	procs := make([]Process, 0, len(names))
	for i, name := range names {
		if i%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue // not a process directory
		}
		dir := c.root + "/" + name
		t0 := time.Now()
		p, ok := readProcTable(dir, pid, btime, pageSize)
		t1 := time.Now()
		tTable += t1.Sub(t0)
		if !ok {
			continue
		}
		ok = readArgvCwd(dir, &p)
		tArgv += time.Since(t1)
		if ok {
			procs = append(procs, p)
		}
	}
	timings["proctable"], timings["argv_cwd"] = tTable, tArgv
	return procs, nil
}

// readProcTable reads pid, ppid, uid, name, start time, CPU time and RSS.
// ok is false for kernel threads, zombies and processes that are gone.
func readProcTable(dir string, pid int, btime int64, pageSize uint64) (p Process, ok bool) {
	// stat and status have the same mode; if either fails (exited, or
	// hidepid=1) there is nothing to show.
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return p, false
	}
	st, err := parseStat(b)
	if err != nil || st.flags&pfKthread != 0 || st.state == 'Z' || st.state == 'X' {
		return p, false
	}
	b, err = os.ReadFile(dir + "/status")
	if err != nil {
		return p, false
	}
	uid, err := parseStatusUID(b)
	if err != nil {
		return p, false
	}
	p = Process{
		PID:       pid,
		PPID:      st.ppid,
		UID:       uid,
		Name:      st.name,
		StartTime: time.Unix(btime, 0).Add(time.Duration(st.starttime) * clockTick),
		CPUTime:   time.Duration(st.utime+st.stime) * clockTick,
	}
	b, err = os.ReadFile(dir + "/statm")
	switch {
	case errors.Is(err, fs.ErrPermission):
		p.Unknown |= model.FieldMem
	case err != nil:
		return p, false
	default:
		pages, err := parseStatmResident(b)
		if err != nil {
			return p, false
		}
		p.RSSBytes = pages * pageSize
	}
	return p, true
}

// readArgvCwd fills Argv and Cwd; ok is false when the process is gone.
// An empty cmdline (a process that blanked its argv) leaves Argv nil.
func readArgvCwd(dir string, p *Process) (ok bool) {
	// ponytail: a pid that exits and is reused between the stat read and these
	// reads (microseconds apart, after the pid space wraps) still mixes two
	// processes; re-check starttime after the last read if that ever matters.
	b, err := os.ReadFile(dir + "/cmdline")
	switch {
	case errors.Is(err, fs.ErrPermission):
		p.Unknown |= model.FieldArgv
	case err != nil:
		return false
	default:
		p.Argv = parseCmdline(b)
	}
	p.Cwd, err = os.Readlink(dir + "/cwd")
	switch {
	case errors.Is(err, fs.ErrPermission):
		p.Unknown |= model.FieldCwd
	case err != nil:
		return false
	}
	return true
}

// listeners reads listening TCP sockets and finds their owners by walking the
// fds of processes that uid may inspect, stopping once every inode is matched.
// An unmatched listener keeps PID 0.
func (c *linuxCollector) listeners(ctx context.Context, procs []Process, uid int) ([]Listener, error) {
	var rows []tcpListen
	for _, f := range [...]struct{ file, proto string }{{"/net/tcp", "tcp4"}, {"/net/tcp6", "tcp6"}} {
		b, err := os.ReadFile(c.root + f.file)
		if errors.Is(err, fs.ErrNotExist) {
			continue // tcp6 is absent when IPv6 is disabled
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, parseNetTCP(b, f.proto)...)
	}

	byInode := make(map[uint64]int, len(rows))
	for i, r := range rows {
		byInode[r.inode] = i
	}
	unmatched := len(byInode)
	for _, p := range procs {
		if unmatched == 0 {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if p.UID != uid && uid != 0 { // others' fds fail with EACCES unless we are root
			continue
		}
		fdDir := c.root + "/" + strconv.Itoa(p.PID) + "/fd/"
		fds, err := readDirNames(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(fdDir + fd)
			if err != nil {
				continue
			}
			inode, ok := parseSocketLink(target)
			if !ok {
				continue
			}
			// ponytail: a socket shared after fork goes to the first pid seen (usually the parent).
			if i, ok := byInode[inode]; ok && rows[i].PID == 0 {
				rows[i].PID = p.PID
				unmatched--
			}
		}
	}

	out := make([]Listener, len(rows))
	for i, r := range rows {
		out[i] = r.Listener
	}
	return out, nil
}

// readDirNames lists a directory without the per-entry allocations of os.ReadDir.
func readDirNames(path string) ([]string, error) {
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	return names, err
}
