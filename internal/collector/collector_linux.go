//go:build linux

package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"
)

// New returns the Linux collector, reading from /proc.
func New() Collector { return linuxCollector{root: "/proc"} }

type linuxCollector struct {
	root string // proc root, "/proc" outside tests
}

const clockTick = 10 * time.Millisecond // USER_HZ = 100 on every Linux ABI

func (c linuxCollector) Collect(ctx context.Context) (Result, error) {
	res := Result{Timings: map[string]time.Duration{}}

	t := time.Now()
	procs, err := c.procTable()
	if err != nil {
		return Result{}, err
	}
	res.Timings["proctable"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	t = time.Now()
	procs = c.argvCwd(procs)
	res.Timings["argv_cwd"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	t = time.Now()
	res.Listeners, err = c.listeners(procs, os.Getuid())
	if err != nil {
		return Result{}, err
	}
	res.Timings["listeners"] = time.Since(t)
	res.Processes = procs

	if mounts, err := os.ReadFile(c.root + "/mounts"); err == nil {
		if opt := hidepidOption(mounts, c.root); opt != "" {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s is mounted with %s: other users' processes are hidden and their listeners have no owner", c.root, opt))
		}
	}
	unknown := 0
	for _, p := range procs {
		if len(p.Unknown) > 0 {
			unknown++
		}
	}
	if unknown > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d processes of other users have unreadable fields; run with sudo to see them", unknown))
	}
	return res, nil
}

// procTable reads pid, ppid, uid, name, start time, CPU time and RSS for every
// user-space process. A process that disappears mid-read is dropped.
func (c linuxCollector) procTable() ([]Process, error) {
	stat, err := os.ReadFile(c.root + "/stat")
	if err != nil {
		return nil, err
	}
	btime, err := parseBtime(stat)
	if err != nil {
		return nil, fmt.Errorf("%s/stat: %w", c.root, err)
	}
	names, err := readDirNames(c.root)
	if err != nil {
		return nil, err
	}
	pageSize := uint64(os.Getpagesize())

	procs := make([]Process, 0, len(names))
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue // not a process directory
		}
		dir := c.root + "/" + name

		// stat and status have the same mode; if either fails (exited, or
		// hidepid=1) there is nothing to show.
		b, err := os.ReadFile(dir + "/stat")
		if err != nil {
			continue
		}
		st, err := parseStat(b)
		if err != nil || st.flags&pfKthread != 0 {
			continue
		}
		b, err = os.ReadFile(dir + "/status")
		if err != nil {
			continue
		}
		uid, err := parseStatusUID(b)
		if err != nil {
			continue
		}

		p := Process{
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
			p.Unknown = append(p.Unknown, "mem")
		case err != nil:
			continue
		default:
			pages, err := parseStatmResident(b)
			if err != nil {
				continue
			}
			p.RSSBytes = pages * pageSize
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// argvCwd fills Argv and Cwd. Processes with an empty cmdline (kernel threads,
// zombies) and processes that exited since procTable are dropped.
func (c linuxCollector) argvCwd(procs []Process) []Process {
	out := procs[:0]
	for _, p := range procs {
		dir := c.root + "/" + strconv.Itoa(p.PID)

		b, err := os.ReadFile(dir + "/cmdline")
		switch {
		case errors.Is(err, fs.ErrPermission):
			p.Unknown = append(p.Unknown, "argv")
		case err != nil:
			continue
		default:
			if p.Argv = parseCmdline(b); p.Argv == nil {
				continue
			}
		}

		p.Cwd, err = os.Readlink(dir + "/cwd")
		switch {
		case errors.Is(err, fs.ErrPermission):
			p.Unknown = append(p.Unknown, "cwd")
		case err != nil:
			continue
		}
		out = append(out, p)
	}
	return out
}

// listeners reads listening TCP sockets and finds their owners by walking the
// fds of processes that uid may inspect, stopping once every inode is matched.
// An unmatched listener keeps PID 0.
func (c linuxCollector) listeners(procs []Process, uid int) ([]Listener, error) {
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
