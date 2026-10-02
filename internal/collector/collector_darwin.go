//go:build darwin

package collector

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// New returns the macOS collector.
func New() Collector { return darwinCollector{} }

type darwinCollector struct{}

func (darwinCollector) Collect(ctx context.Context) (Result, error) {
	lib, err := loadLibSystem()
	if err != nil {
		return Result{}, fmt.Errorf("collector: load libSystem: %w", err)
	}
	res := Result{Timings: map[string]time.Duration{}}

	t := time.Now()
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return Result{}, fmt.Errorf("collector: sysctl kern.proc.all: %w", err)
	}
	res.Processes = make([]Process, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
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
	denied := 0
	kept := res.Processes[:0]
	for _, p := range res.Processes {
		var argvErr, cwdErr, taskErr error
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
		if n, err := lib.procArgs2(p.PID, argBuf); err != nil {
			argvErr = err // EINVAL for other users' processes, so not used to decide on exit
		} else if argv, ok := decodeProcArgs2(argBuf[:n]); ok {
			p.Argv = argv
		} else {
			argvErr = syscall.EINVAL
		}
		for _, f := range []struct {
			name string
			err  error
		}{{"argv", argvErr}, {"cwd", cwdErr}, {"cpu", taskErr}, {"mem", taskErr}} {
			if f.err != nil {
				p.Unknown = append(p.Unknown, f.name)
			}
		}
		if errors.Is(argvErr, syscall.EPERM) || errors.Is(cwdErr, syscall.EPERM) || errors.Is(taskErr, syscall.EPERM) {
			denied++
		}
		kept = append(kept, p)
	}
	res.Processes = kept
	if denied > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d processes of other users: argv, cwd or cpu/mem not readable without root", denied))
	}
	res.Timings["argv_cwd"] = time.Since(t)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	t = time.Now()
	res.Listeners = fdListeners(lib, res.Processes)
	tp := time.Now()
	pcb, warn := pcbListeners()
	res.Timings["pcblist"] = time.Since(tp)
	if warn != "" {
		res.Warnings = append(res.Warnings, warn)
	}
	res.Listeners = mergeListeners(res.Listeners, pcb)
	res.Timings["listeners"] = time.Since(t)
	return res, nil
}

// fdListeners walks the socket fds of own-uid processes (the only ones libproc lets us read)
// and returns their TCP listeners with the owning pid.
func fdListeners(lib *libSystem, procs []Process) []Listener {
	uid := os.Geteuid()
	var ls []Listener
	fdBuf := make([]byte, 64*1024)
	sockBuf := make([]byte, sizeofSocketFDInfo)
	for _, p := range procs {
		if p.UID != uid || p.PID == 0 {
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
			if l, ok := decodeSocketFDInfo(sockBuf[:n]); ok {
				l.PID = p.PID
				ls = append(ls, l)
			}
		}
	}
	return ls
}

// pcbListeners reads every TCP listener on the host from net.inet.tcp.pcblist_n, with
// so_last_pid as the owner. An empty list means unknown, never "no listeners".
func pcbListeners() ([]Listener, string) {
	b, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return nil, "net.inet.tcp.pcblist_n: " + err.Error() + "; other users' listeners unknown"
	}
	ls, pcbs := decodePCBList(b)
	if pcbs == 0 {
		return nil, "net.inet.tcp.pcblist_n is empty; other users' listeners unknown"
	}
	return ls, ""
}

// mergeListeners keeps the fd-walk listeners (exact pid; a socket shared by several pids is
// listed once per pid) and adds PCB-list listeners whose (proto, addr, port) the fd walk did
// not see, typically sockets of other users.
func mergeListeners(fd, pcb []Listener) []Listener {
	type key struct {
		proto string
		addr  netip.Addr
		port  uint16
	}
	var out []Listener
	seenFD := map[Listener]bool{}
	seen := map[key]bool{}
	for _, l := range fd {
		if !seenFD[l] {
			seenFD[l] = true
			seen[key{l.Proto, l.Addr, l.Port}] = true
			out = append(out, l)
		}
	}
	for _, l := range pcb {
		k := key{l.Proto, l.Addr, l.Port}
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

// exited reports whether a proc_pidinfo error means the process exited or is a zombie (ESRCH),
// so its row is dropped rather than shown half-filled. EPERM (another user's process) keeps
// the row with the field marked unknown.
func exited(err error) bool { return errors.Is(err, syscall.ESRCH) }
