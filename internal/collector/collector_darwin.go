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
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return Result{}, fmt.Errorf("collector: sysctl kern.proc.all: %w", err)
	}
	res.Processes = make([]Process, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
		if kp.Proc.P_pid == 0 {
			continue // kernel_task; PID 0 is the "unknown owner" pseudo-process in model.Build
		}
		res.Processes = append(res.Processes, Process{
			PID:       int(kp.Proc.P_pid),
			PPID:      int(kp.Eproc.Ppid),
			UID:       int(kp.Eproc.Ucred.Uid),
			StartTime: time.Unix(kp.Proc.P_starttime.Unix()),
			Name:      cstring(kp.Proc.P_comm[:]),
		})
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
	argBuf := make([]byte, argmax)
	pathBuf := make([]byte, sizeofVnodePathInfo)
	taskBuf := make([]byte, sizeofProcTaskInfo)
	uid := os.Geteuid()
	limit := o.InProject != nil
	denied := map[int]bool{} // processes with a field denied by EPERM
	kept := res.Processes[:0]
	for _, p := range res.Processes {
		var argvErr, cwdErr, taskErr error
		if !limit {
			var drop bool
			if drop, argvErr = readArgv(lib, argBuf, &p, uid); drop {
				continue
			}
		}
		if n, err := lib.pidinfo(p.PID, procPidVnodePathInfo, pathBuf); err != nil {
			cwdErr = err
		} else if cwd, ok := decodeVnodePathInfo(pathBuf[:n]); ok {
			p.Cwd = cwd
		} else {
			cwdErr = syscall.EINVAL
		}
		if n, err := lib.pidinfo(p.PID, procPidTaskInfo, taskBuf); err != nil {
			taskErr = err
		} else if rss, ticks, ok := decodeTaskInfo(taskBuf[:n]); !ok {
			taskErr = syscall.EINVAL
		} else {
			p.RSSBytes, p.CPUTime = rss, time.Duration(lib.machToNs(ticks))
		}
		if exited(cwdErr) || exited(taskErr) {
			continue
		}
		for _, f := range []struct {
			bit model.FieldSet
			err error
		}{{model.FieldArgv, argvErr}, {model.FieldCwd, cwdErr}, {model.FieldCPU, taskErr}, {model.FieldMem, taskErr}} {
			if f.err != nil {
				p.Unknown |= f.bit
			}
		}
		if errors.Is(argvErr, syscall.EPERM) || errors.Is(cwdErr, syscall.EPERM) || errors.Is(taskErr, syscall.EPERM) {
			denied[p.PID] = true
		}
		kept = append(kept, p)
	}
	res.Processes = kept
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
		want := argvWanted(o, res.Processes, res.Listeners)
		kept := res.Processes[:0]
		for i, p := range res.Processes {
			if !want[i] {
				p.Unknown |= model.FieldArgv // not read, so not counted as denied
				kept = append(kept, p)
				continue
			}
			drop, argvErr := readArgv(lib, argBuf, &p, uid)
			if drop {
				continue
			}
			if argvErr != nil {
				p.Unknown |= model.FieldArgv
				denied[p.PID] = denied[p.PID] || errors.Is(argvErr, syscall.EPERM)
			}
			kept = append(kept, p)
		}
		res.Processes = kept
		res.Timings["argv_cwd"] += time.Since(t)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
	}
	if n := countDenied(res.Processes, denied); n > 0 {
		res.Warnings = append(res.Warnings, model.Warning{Code: "process_fields_unreadable", Count: n,
			Hint: "other users' processes: argv, cwd, cpu and mem need root; run with sudo"})
	}
	if pcbWarn != "" {
		res.Warnings = append(res.Warnings, model.Warning{Code: "pcblist_unavailable", Count: 1, Hint: pcbWarn})
	}
	return res, nil
}

// countDenied counts the processes of procs that denied has, so one dropped after it was
// marked does not count.
func countDenied(procs []Process, denied map[int]bool) int {
	n := 0
	for _, p := range procs {
		if denied[p.PID] {
			n++
		}
	}
	return n
}

// readArgv fills p.Argv from kern.procargs2 using buf (kern.argmax bytes) and returns why it
// could not, or drop for a process of uid whose read failed: it is exiting, just forked or
// mid-exec, and is dropped rather than shown half-filled (DEV-41).
func readArgv(lib *libSystem, buf []byte, p *Process, uid int) (drop bool, _ error) {
	n, err := lib.procArgs2(p.PID, buf)
	if err != nil && p.UID == uid {
		return true, nil
	}
	switch argv, ok := decodeProcArgs2(buf[:n]); {
	case err != nil:
		return false, err // EINVAL for other users' processes
	case n == len(buf):
		// The strings area is larger than kern.argmax and the kernel returned its tail, so
		// argc no longer lines up: unknown, but the process is alive and keeps its row.
		return false, syscall.E2BIG
	case ok:
		p.Argv = argv
		return false, nil
	default:
		return false, syscall.EINVAL
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
