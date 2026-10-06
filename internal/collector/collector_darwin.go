//go:build darwin

package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dogauzun/devdash/internal/model"
)

// New returns the macOS collector.
func New() Collector { return darwinCollector{} }

type darwinCollector struct{}

func (darwinCollector) Collect(ctx context.Context, o Options) (Result, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return Result{}, fmt.Errorf("collector: load libSystem: %w", err)
	}
	res := Result{TakenAt: time.Now(), Host: host(), Timings: model.Timing{}}

	t := time.Now()
	if res.Processes, err = procTable(); err != nil {
		return Result{}, err
	}
	res.Timings["proctable"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	t = time.Now()
	argmax, err := unix.SysctlUint32("kern.argmax")
	if err != nil {
		return Result{}, fmt.Errorf("collector: sysctl kern.argmax: %w", err)
	}
	uid := os.Geteuid()
	r := &fieldReader{lib: lib, argBuf: make([]byte, argmax), pathBuf: make([]byte, sizeofVnodePathInfo),
		taskBuf: make([]byte, sizeofProcTaskInfo), uid: uid, denied: map[int]bool{}}
	limit := o.InProject != nil
	res.Processes = r.processes(res.Processes, !limit)
	res.Timings["argv_cwd"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	t = time.Now()
	fd := fdListeners(lib, res.Processes)
	tp := time.Now()
	pcb, pcbWarn := pcbListeners()
	res.Timings["pcblist"] = time.Since(tp)
	res.Listeners = mergeListeners(fd, pcb)
	res.Timings["listeners"] = time.Since(t)

	if limit {
		t = time.Now()
		res.Processes = r.limitedArgv(res.Processes, argvWanted(o, res.Processes, res.Listeners))
		res.Timings["argv_cwd"] += time.Since(t)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
	}
	if n := countDenied(res.Processes, r.denied); n > 0 {
		res.Warnings = append(res.Warnings, model.Warning{Code: "process_fields_unreadable", Count: n,
			Hint: "other users' processes: argv, cwd, cpu and mem need root; run with sudo", Sudo: uid != 0})
	}
	res.OwnerSudo = uid != 0 // Build's "run with sudo to see owners" is the hint here
	if pcbWarn != "" {
		res.Warnings = append(res.Warnings, model.Warning{Code: "pcblist_unavailable", Count: 1, Hint: pcbWarn})
	}
	return res, nil
}

// procTable reads every process but kernel_task from kern.proc.all: pid, ppid, uid, start
// time and name.
func procTable() ([]Process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("collector: sysctl kern.proc.all: %w", err)
	}
	procs := make([]Process, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
		if kp.Proc.P_pid == 0 {
			continue // kernel_task; PID 0 is the "unknown owner" pseudo-process in model.Build
		}
		procs = append(procs, Process{
			PID:       int(kp.Proc.P_pid),
			PPID:      int(kp.Eproc.Ppid),
			UID:       int(kp.Eproc.Ucred.Uid),
			StartTime: startTime(kp),
			Name:      cstring(kp.Proc.P_comm[:]),
		})
	}
	return procs, nil
}

// fieldReader reads the per-process fields of one Collect into buffers allocated once:
// argBuf is kern.argmax bytes, uid is devdash's effective uid, and denied collects the
// processes with a field denied by EPERM.
type fieldReader struct {
	lib                      *libSystem
	argBuf, pathBuf, taskBuf []byte
	uid                      int
	denied                   map[int]bool
}

// processes reads the argv, cwd, CPU time and RSS of each of procs, in place, and returns
// those kept: a process that exited mid-read is dropped, and a field it denied is marked
// unknown. Without withArgv, Argv is left for limitedArgv.
func (r *fieldReader) processes(procs []Process, withArgv bool) []Process {
	kept := procs[:0]
	for _, p := range procs {
		if withArgv && !r.argv(&p) {
			continue
		}
		var cwdErr, taskErr error
		if n, err := r.lib.pidinfo(p.PID, procPidVnodePathInfo, r.pathBuf); err != nil {
			cwdErr = err
		} else if cwd, ok := decodeVnodePathInfo(r.pathBuf[:n]); ok {
			p.Cwd, p.CwdDeleted = cwd, cwdGone(cwd)
		} else {
			cwdErr = syscall.EINVAL
		}
		if n, err := r.lib.pidinfo(p.PID, procPidTaskInfo, r.taskBuf); err != nil {
			taskErr = err
		} else if rss, ticks, ok := decodeTaskInfo(r.taskBuf[:n]); !ok {
			taskErr = syscall.EINVAL
		} else {
			p.RSSBytes, p.CPUTime = rss, time.Duration(r.lib.machToNs(ticks))
		}
		if exited(cwdErr) || exited(taskErr) {
			continue
		}
		r.unknown(&p, model.FieldCwd, cwdErr)
		r.unknown(&p, model.FieldCPU|model.FieldMem, taskErr)
		kept = append(kept, p)
	}
	return kept
}

// limitedArgv reads argv for the processes want marks and marks the others' argv unknown, not
// denied. A process that exited since its first read is dropped.
func (r *fieldReader) limitedArgv(procs []Process, want []bool) []Process {
	kept := procs[:0]
	for i, p := range procs {
		switch {
		case !want[i]:
			p.Unknown |= model.FieldArgv // not read, so not counted as denied
		case !r.argv(&p):
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// argv reads p's argv (readArgv); keep is false when p is dropped. A failed read marks
// FieldArgv unknown. A process dropped later in the same read may stay in denied, which
// countDenied ignores.
func (r *fieldReader) argv(p *Process) (keep bool) {
	drop, err := readArgv(r.lib, r.argBuf, p, r.uid)
	if drop {
		return false
	}
	r.unknown(p, model.FieldArgv, err)
	return true
}

// unknown marks bits unknown in p when its read failed with err, and p denied when err is EPERM.
func (r *fieldReader) unknown(p *Process, bits model.FieldSet, err error) {
	if err == nil {
		return
	}
	p.Unknown |= bits
	if errors.Is(err, syscall.EPERM) {
		r.denied[p.PID] = true
	}
}

// cwdGone reports whether a process's cwd was removed. PROC_PIDVNODEPATHINFO keeps reporting
// a removed directory's old path, so only an lstat of that path can tell, at one syscall per
// process with a known cwd. Only ENOENT counts: EACCES on a parent says nothing, and a
// directory recreated at the same path is missed rather than the tag invented (DEV-116).
func cwdGone(path string) bool {
	var st unix.Stat_t
	return errors.Is(unix.Lstat(path, &st), syscall.ENOENT)
}

// readArgv fills p.Argv from kern.procargs2 using buf (kern.argmax bytes) and returns why it
// could not, or drop for a process of uid that is dropped rather than shown half-filled.
func readArgv(lib *libSystem, buf []byte, p *Process, uid int) (drop bool, _ error) {
	n, err := lib.procArgs2(p.PID, buf)
	p.Argv, drop, err = argvFrom(buf[:n], n == len(buf), err, p.UID == uid)
	return drop, err
}

// argvFrom decides a kern.procargs2 read that returned b and err, full when b fills the
// kern.argmax buffer, of a process of devdash's uid when own.
func argvFrom(b []byte, full bool, err error, own bool) (argv []string, drop bool, _ error) {
	argv, ok := decodeProcArgs2(b)
	switch {
	case err == nil && full:
		// The strings area is larger than kern.argmax and the kernel returned its tail, so
		// argc no longer lines up (it may even decode, to env or garbage): unknown, but the
		// process is alive and keeps its row, the one own-uid exception to DEV-41 (DEV-40).
		return nil, false, syscall.E2BIG
	case err == nil && ok:
		return argv, false, nil
	case own:
		// A failed read, or a short one that does not decode (a live process's whole strings
		// area always does): exiting, just forked or mid-exec, so dropped (DEV-41, DEV-197).
		return nil, true, nil
	case err != nil:
		return nil, false, err // EINVAL for other users' processes
	default:
		return nil, false, syscall.EINVAL
	}
}

// fdListeners walks the socket fds of own-uid processes (the only ones libproc lets us read)
// and returns their TCP listeners with the owning pid, one entry per socket and pid.
func fdListeners(lib *libSystem, procs []Process) []sock {
	uid := os.Geteuid()
	var ls []sock
	fdBuf := make([]byte, 64*1024)
	sockBuf := make([]byte, sizeofSocketFDInfo)
	for _, p := range procs {
		if p.UID != uid {
			continue
		}
		n, err := lib.pidinfo(p.PID, procPidListFDs, fdBuf)
		for err == nil && n == len(fdBuf) { // possibly truncated: grow and retry
			fdBuf = make([]byte, 2*len(fdBuf))
			n, err = lib.pidinfo(p.PID, procPidListFDs, fdBuf)
		}
		if err != nil {
			continue // exited, or zero fds
		}
		for _, fd := range decodeFDList(fdBuf[:n]) {
			n, err := lib.pidfdinfo(p.PID, fd, procPidFDSocketInfo, sockBuf)
			if err != nil {
				continue // closed since the list was read
			}
			if s, ok := decodeSocketFDInfo(sockBuf[:n]); ok {
				s.PID = p.PID
				ls = append(ls, s)
			}
		}
	}
	return ls
}

// pcbHint is the warning hint for an empty or denied PCB list: the cause users can act on.
const pcbHint = "other users' listeners unknown: macOS withholds the socket list when an ancestor of devdash " +
	"is ad-hoc signed (go run, a Homebrew-built tmux); start devdash directly from a shell, or run with sudo"

// pcbListeners reads every TCP listener on the host from net.inet.tcp.pcblist_n, with
// so_last_pid as the owner. A list without other processes' sockets is withheld, which means
// unknown, never "no listeners".
func pcbListeners() ([]sock, string) {
	b, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return nil, "net.inet.tcp.pcblist_n: " + err.Error() + "; " + pcbHint
	}
	ls, others := decodePCBList(b, os.Getpid())
	if others == 0 {
		return nil, pcbHint
	}
	return ls, ""
}

// mergeListeners returns one listener per socket handle: a socket shared across fork goes to
// the lowest pid holding it (the fd walk sees every holder; the PCB list's so_last_pid is
// whoever touched it last), and SO_REUSEPORT siblings, being distinct sockets, stay distinct.
// The PCB list only adds sockets the fd walk did not see, typically other users'.
func mergeListeners(fd, pcb []sock) []Listener {
	var out []Listener
	at := map[uint64]int{}
	for _, s := range fd {
		if i, ok := at[s.so]; ok {
			out[i].PID = min(out[i].PID, s.PID)
			continue
		}
		at[s.so] = len(out)
		out = append(out, s.Listener)
	}
	for _, s := range pcb {
		if _, ok := at[s.so]; !ok {
			at[s.so] = len(out)
			out = append(out, s.Listener)
		}
	}
	return out
}

// exited reports whether a proc_pidinfo error means the process exited or is a zombie (ESRCH),
// so its row is dropped rather than shown half-filled. EPERM (another user's process) keeps
// the row with the field marked unknown.
func exited(err error) bool { return errors.Is(err, syscall.ESRCH) }
