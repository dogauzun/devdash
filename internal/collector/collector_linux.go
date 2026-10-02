//go:build linux

package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/model"
)

// New returns the Linux collector, reading from /proc.
func New() Collector { return newLinux("/proc") }

func newLinux(root string) *linuxCollector {
	c := &linuxCollector{root: root, euid: os.Geteuid(), ptrace: hasPtrace()}
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
	root   string // proc root, "/proc" outside tests
	euid   int    // whose fds the walk reads (all when 0); os.Geteuid outside tests
	ptrace bool   // CAP_SYS_PTRACE is effective; only read for the hints when euid is 0
	btime  func() (int64, error)

	// fd-walk state carried to the next Collect. A Collect abandoned on timeout may still be
	// running when the next one starts, so it is copied in and out under mu.
	mu        sync.Mutex
	denied    map[procKey]bool // processes whose fd/ failed with EACCES on the last walk
	unmatched []uint64         // sorted inodes of countable listeners the last walk left unowned
}

type procKey struct {
	pid   int
	start int64
}

const clockTick = 10 * time.Millisecond // USER_HZ = 100 on every Linux ABI

func (c *linuxCollector) Collect(ctx context.Context) (Result, error) {
	res := Result{TakenAt: time.Now(), Host: host(), Timings: model.Timing{}}

	procs, err := c.processes(ctx, res.Timings)
	if err != nil {
		return Result{}, err
	}

	t := time.Now()
	var fdDenied bool
	res.Listeners, fdDenied, err = c.listeners(ctx, procs)
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
			// hidepid falls back to ptrace_may_access, so root without CAP_SYS_PTRACE is not even
			// shown other users' pids: nothing was denied to the walk, but the capability is the fix.
			if c.euid == 0 && !c.ptrace {
				fdDenied = true
			}
		}
	}
	unknown := 0
	for _, p := range procs {
		if p.Unknown != 0 {
			unknown++
		}
	}
	fieldsHint, ownerHint := c.hints(fdDenied)
	if unknown > 0 {
		res.Warnings = append(res.Warnings, model.Warning{Code: "process_fields_unreadable", Count: unknown, Hint: fieldsHint})
	}
	res.OwnerHint = ownerHint
	return res, nil
}

// hints are the process_fields_unreadable hint and model.Raw.OwnerHint. Root is denied only
// by ptrace access checks, so sudo cannot help: without CAP_SYS_PTRACE (a Docker container's
// default) the hint names the capability, with it the denial comes from a security module.
// The owner hint blames permissions only if the fd walk was denied a read (fdDenied); if not,
// root's unowned listeners belong to processes outside its pid namespace or to the kernel.
func (c *linuxCollector) hints(fdDenied bool) (fields, owner string) {
	switch {
	case c.euid != 0:
		return "processes of other users have unreadable fields; run with sudo to see them", ""
	case !c.ptrace:
		const why = "running as root without CAP_SYS_PTRACE; start the container with --cap-add SYS_PTRACE"
		fields, owner = why+" to see them", why+" to see owners"
	default:
		const why = "denied even to root with CAP_SYS_PTRACE, by a security module or sandbox"
		fields, owner = "some processes have unreadable fields: "+why, "listener owners unreadable: "+why
	}
	if !fdDenied {
		owner = "owner not visible from this pid namespace, or the socket is held by the kernel"
	}
	return fields, owner
}

// hasPtrace reports whether CAP_SYS_PTRACE is in this process's effective set. If capget
// fails it reports true, so the hints do not claim a missing capability.
func hasPtrace() bool {
	var data [2]unix.CapUserData // _LINUX_CAPABILITY_VERSION_3 uses two 32-bit words
	if err := unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &data[0]); err != nil {
		return true
	}
	return data[unix.CAP_SYS_PTRACE/32].Effective&(1<<(unix.CAP_SYS_PTRACE%32)) != 0
}

// processes reads every user-space process in one pass per pid, so a pid
// reused between reads cannot mix two processes into one row. Kernel threads,
// zombies and processes that exit mid-read are skipped. The result is sorted by
// pid. It adds the "proctable" and "argv_cwd" timings.
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
	pids := make([]int, 0, len(names))
	for _, name := range names {
		if pid, err := strconv.Atoi(name); err == nil { // else not a process directory
			pids = append(pids, pid)
		}
	}
	// /proc lists pids in ascending order already; sorting keeps the fd walk's
	// "lowest pid owns a shared socket" true for any proc root.
	slices.Sort(pids)
	tTable, tArgv := time.Since(start), time.Duration(0)
	pageSize := uint64(os.Getpagesize())

	procs := make([]Process, 0, len(pids))
	for i, pid := range pids {
		if i%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dir := c.root + "/" + strconv.Itoa(pid)
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
	// The kernel marks a removed cwd "<path> (deleted)"; macOS reports the bare old path.
	// ponytail: a directory really named "x (deleted)" loses its suffix too, since readlink
	// cannot tell them apart; a stat of cwd showing nlink 0 can, if that ever matters.
	p.Cwd = strings.TrimSuffix(p.Cwd, " (deleted)")
	return true
}

// listeners reads listening TCP sockets and finds their owners by walking, in pid
// order, the fds of the processes c.euid may inspect, so a socket shared across fork
// goes to the lowest pid holding it. An unmatched listener keeps PID 0.
//
// The walk stops once every countable listener is matched: those whose socket uid is
// c.euid, or all of them as root, since other users' fds are unreadable anyway. It
// skips processes whose fd/ failed with EACCES on the last walk unless the set of
// unmatched countable inodes differs from the one that walk ended with.
//
// fdDenied reports whether a read of fd/ or of an fd link failed with EACCES or EPERM, counting
// the processes skipped because their fd/ was denied on the last walk.
func (c *linuxCollector) listeners(ctx context.Context, procs []Process) (_ []Listener, fdDenied bool, _ error) {
	var rows []tcpListen
	for _, f := range [...]struct{ file, proto string }{{"/net/tcp", "tcp4"}, {"/net/tcp6", "tcp6"}} {
		b, err := os.ReadFile(c.root + f.file)
		if errors.Is(err, fs.ErrNotExist) {
			continue // tcp6 is absent when IPv6 is disabled
		}
		if err != nil {
			return nil, false, err
		}
		rows = append(rows, parseNetTCP(b, f.proto)...)
	}

	euid := c.euid
	countable := func(r tcpListen) bool { return euid == 0 || r.uid == euid }
	byInode := make(map[uint64]int, len(rows))
	left := 0 // countable listeners still without an owner
	for i, r := range rows {
		byInode[r.inode] = i
		if countable(r) {
			left++
		}
	}
	unmatchedSet := func() []uint64 {
		var s []uint64
		for _, r := range rows {
			if r.PID == 0 && countable(r) {
				s = append(s, r.inode)
			}
		}
		slices.Sort(s)
		return s
	}

	c.mu.Lock()
	wasDenied, wasUnmatched := c.denied, c.unmatched
	c.mu.Unlock()
	denied := map[procKey]bool{}
	linkDenied := false
	scan := func(p Process) {
		fdDir := c.root + "/" + strconv.Itoa(p.PID) + "/fd/"
		fds, err := readDirNames(fdDir)
		if errors.Is(err, fs.ErrPermission) { // not dumpable, e.g. gpg-agent or a setgid binary
			denied[procKey{p.PID, p.StartTime.UnixNano()}] = true
		}
		if err != nil {
			return // EACCES, or the process exited
		}
		for _, fd := range fds {
			target, err := os.Readlink(fdDir + fd)
			if err != nil {
				linkDenied = linkDenied || errors.Is(err, fs.ErrPermission)
				continue
			}
			inode, ok := parseSocketLink(target)
			if !ok {
				continue
			}
			if i, ok := byInode[inode]; ok && rows[i].PID == 0 {
				rows[i].PID = p.PID
				if countable(rows[i]) {
					left--
				}
			}
		}
	}

	var retry []Process // denied last time; scanned only if the unmatched set changed
	for _, p := range procs {
		if left == 0 {
			break
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if p.UID != euid && euid != 0 { // others' fds fail with EACCES unless we are root
			continue
		}
		if wasDenied[procKey{p.PID, p.StartTime.UnixNano()}] {
			retry = append(retry, p)
			continue
		}
		scan(p)
	}
	unmatched := unmatchedSet()
	if len(unmatched) > 0 && !slices.Equal(unmatched, wasUnmatched) {
		// ponytail: retried pids come after higher ones, so a fork-shared socket held by
		// a retried pid and a higher readable pid goes to the higher one.
		for _, p := range retry {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			scan(p)
		}
		unmatched = unmatchedSet()
	} else {
		for _, p := range retry {
			denied[procKey{p.PID, p.StartTime.UnixNano()}] = true
		}
	}
	c.mu.Lock()
	c.denied, c.unmatched = denied, unmatched
	c.mu.Unlock()

	out := make([]Listener, len(rows))
	for i, r := range rows {
		out[i] = r.Listener
	}
	return out, len(denied) > 0 || linkDenied, nil
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
