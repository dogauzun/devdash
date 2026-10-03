// Package collector reads processes and listening sockets from the OS.
// One implementation per OS is compiled in via build tags (collector_<goos>.go);
// this file holds only the platform-independent types.
package collector

import (
	"context"
	"os"
	"runtime"

	"github.com/dogauzun/devdash/internal/model"
)

// The raw types are model's, so a Result goes into model.Build without conversion.
type (
	// Result is one raw sample.
	Result = model.Raw
	// Process carries only the collected fields; see model.Raw.
	Process = model.Process
	// Listener is a listening socket with its owner pid, 0 when unknown.
	Listener = model.RawListener
)

// Collector produces a Result for the current machine. Collect may block on a stuck read
// and checks ctx only between reads, so callers run it in a goroutine.
type Collector interface {
	Collect(ctx context.Context, o Options) (Result, error)
}

// Options adjusts one Collect. The zero value reads everything.
type Options struct {
	// InProject, when set, limits argv reads (spec "Performance and degraded modes": over
	// 5000 processes, argv mostly for processes in a project or with a listener). Collect then
	// reads every other field and the listeners first, calls InProject once with the processes
	// it kept (cwd read, Argv still nil) and reads argv only for those InProject marks, those
	// holding a listener, those whose name may be a runtime's and those named systemd (see
	// argvWanted). Every other process has a nil Argv and FieldArgv in Unknown, and does not count toward
	// process_fields_unreadable. InProject returns one bool per process, in the order given
	// (a missing one means false), and runs on Collect's goroutine.
	InProject func(procs []Process) []bool
}

// commCut is the shortest length at which a kernel name may have been cut (model's commCut):
// Linux keeps 15 bytes of comm, macOS 16 of p_comm.
const commCut = 15

// argvWanted reports, for each of procs, whether Collect reads its argv when o.InProject is
// set: the process holds one of ls, InProject marks it, its Name may have been cut by the
// kernel, model.MayBeRuntime(Name), or its Name is systemd. A runtime process is known by
// argv[0] when its name is cut (com.docker.backend's comm is com.docker.back) or it re-execs
// under another name (pasta as pasta.avx2): without argv, kill would not refuse it. A
// `systemd --user` manager is a subreaper only by the --user in its argv (model.Tag), and
// without it its children are never tagged orphaned; there is one per user session, so the
// reads are few (DEV-123).
func argvWanted(o Options, procs []Process, ls []Listener) []bool {
	marked := o.InProject(procs)
	holds := make(map[int]bool, len(ls))
	for _, l := range ls {
		if l.PID != 0 {
			holds[l.PID] = true
		}
	}
	want := make([]bool, len(procs))
	for i, p := range procs {
		want[i] = holds[p.PID] || (i < len(marked) && marked[i]) || len(p.Name) >= commCut || model.MayBeRuntime(p.Name) || p.Name == "systemd"
	}
	return want
}

// host describes this machine and the effective uid devdash runs as.
func host() model.Host {
	name, _ := os.Hostname()
	return model.Host{OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: name, UID: os.Geteuid()}
}
